package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMalformedRolloutsDoNotStarveOtherSessions(t *testing.T) {
	for _, kind := range []string{"header", "row", "timestamp", "content"} {
		t.Run(kind, func(t *testing.T) {
			im, st := testImporter(t)
			bad := rollout(t, im.Home, "a-bad", message("user", "valid prefix"))
			body, err := os.ReadFile(bad)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "header":
				body = []byte("not json\n")
			case "row":
				body = append(body, []byte("not json\n")...)
			case "timestamp":
				body = []byte(strings.ReplaceAll(string(body), "2026-10-28T12:00:00.123Z", "invalid"))
			case "content":
				body = append(body, []byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":"wrong shape"}}`+"\n")...)
			}
			if err := os.WriteFile(bad, body, 0600); err != nil {
				t.Fatal(err)
			}
			rollout(t, im.Home, "z-good", message("user", "healthy session"))
			first := runImport(t, im)
			if first.Failed != 1 || first.Sessions != 1 || first.Records != 1 {
				t.Fatalf("first: %+v", first)
			}
			if st.RawCount("codex:a-bad") != 0 || st.MetaString(statePrefix+"codex:a-bad") != "" {
				t.Fatal("bad input advanced capture state")
			}
			if st.RawCount("codex:z-good") != 1 {
				t.Fatal("bad input starved healthy session")
			}
			if again := runImport(t, im); again.Failed != 1 || again.Records != 0 {
				t.Fatalf("repeat: %+v", again)
			}
		})
	}
}

func TestMalformedAppendPreservesCommittedCapture(t *testing.T) {
	im, st := testImporter(t)
	bad := rollout(t, im.Home, "a-prior", message("user", "already durable"))
	runImport(t, im)
	if err := st.MarkDistilled("codex:a-prior", "default", 1); err != nil {
		t.Fatal(err)
	}
	oldState := st.MetaString(statePrefix + "codex:a-prior")
	f, err := os.OpenFile(bad, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString("broken json\n" + message("assistant", "after corruption"))
	f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	rollout(t, im.Home, "z-new", message("user", "new session"))
	s := runImport(t, im)
	if s.Failed != 1 || s.Records != 1 {
		t.Fatalf("stats: %+v", s)
	}
	if st.RawCount("codex:a-prior") != 1 || st.DistilledCount("codex:a-prior", "default") != 1 || st.MetaString(statePrefix+"codex:a-prior") != oldState {
		t.Fatal("bad append changed previously committed state")
	}
}

func TestCompleteFinalJSONWithoutNewline(t *testing.T) {
	im, st := testImporter(t)
	path := rollout(t, im.Home, "s1", message("user", "first")+strings.TrimSuffix(message("assistant", "last"), "\n"))
	if s := runImport(t, im); s.Failed != 0 || s.Records != 2 {
		t.Fatalf("first: %+v", s)
	}
	if s := runImport(t, im); s.Records != 0 {
		t.Fatalf("repeat: %+v", s)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString("\n" + message("user", "later"))
	f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if s := runImport(t, im); s.Records != 1 {
		t.Fatalf("newline and later append: %+v", s)
	}
	recs, err := st.ReadRaw("codex:s1")
	if err != nil || len(recs) != 3 || recs[1].Text != "last" || recs[2].Seq != 2 {
		t.Fatalf("records: %+v, %v", recs, err)
	}
}

func TestHeaderOnlyWithoutNewlineCanGrow(t *testing.T) {
	im, st := testImporter(t)
	path := rollout(t, im.Home, "s1", "")
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(string(header), "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im, "s1"); s.Failed != 0 || s.Records != 0 {
		t.Fatalf("header: %+v", s)
	}
	if err := os.WriteFile(path, append(header, []byte(strings.TrimSuffix(message("user", "first"), "\n"))...), 0600); err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im); s.Records != 1 || st.RawCount("codex:s1") != 1 {
		t.Fatalf("grown: %+v", s)
	}
}

func TestUnknownHistoryVersionIsReportedAndLegacyContinues(t *testing.T) {
	im, st := testImporter(t)
	if err := os.WriteFile(filepath.Join(im.Home, "thread_history_2.sqlite"), []byte("unknown schema"), 0600); err != nil {
		t.Fatal(err)
	}
	rollout(t, im.Home, "s1", message("user", "legacy remains readable"))
	if s := runImport(t, im); s.Failed != 1 || s.Records != 1 {
		t.Fatalf("stats: %+v", s)
	}
	if st.RawCount("codex:s1") != 1 {
		t.Fatal("unknown history version blocked legacy capture")
	}
	if s := runImport(t, im); s.Failed != 1 || s.Records != 0 {
		t.Fatalf("repeat: %+v", s)
	}
}

func TestBrokenProjectionDoesNotBlockLegacyCapture(t *testing.T) {
	im, _ := testImporter(t)
	if err := os.WriteFile(filepath.Join(im.Home, "thread_history_1.sqlite"), []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	rollout(t, im.Home, "s1", message("user", "legacy session"))
	if s := runImport(t, im); s.Failed != 1 || s.Records != 1 {
		t.Fatalf("stats: %+v", s)
	}
}
