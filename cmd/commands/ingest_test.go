package commands

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

func TestParseNDJSONSkipsBadLines(t *testing.T) {
	in := `{"text":"a","id":"1"}
not json
{"id":"2"}
{"text":"b","id":"3","session":"s"}
`
	recs, skipped := parseNDJSON(strings.NewReader(in))
	if len(recs) != 2 { // "a" and "b"; "not json" + text-less {"id":"2"} skipped
		t.Fatalf("parsed %d records, want 2 (%+v)", len(recs), recs)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
}

func TestRecordKeyIdentityVsFallback(t *testing.T) {
	// Same id, different text → different key (detects edit).
	if recordKey("x", "one") == recordKey("x", "two") {
		t.Error("same id + changed text must yield different keys")
	}
	// Same id, same text → same key (idempotent).
	if recordKey("x", "one") != recordKey("x", "one") {
		t.Error("same id + same text must be stable")
	}
	// No id → content-hash fallback (distinct prefix from id-keyed).
	if !strings.HasPrefix(recordKey("", "one"), "h:") {
		t.Error("empty id must fall back to content-hash key")
	}
}

func TestGroupSessionsGroupingAndDefaults(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	recs := []ingestRecord{
		{Text: "x", ID: "a", Session: "feed"}, // grouped
		{Text: "y", ID: "b", Session: "feed"}, // grouped (same session)
		{Text: "z", ID: "c"},                  // own session, id-derived
		{Text: "w"},                           // own session, hash-derived; ts defaulted
	}
	sess := groupSessions(recs, now)
	// feed has 2 records; the other two are their own sessions → 3 sessions total.
	if len(sess) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sess))
	}
	for _, s := range sess {
		if !strings.HasPrefix(s.Session, "file:") {
			t.Errorf("session %q missing file: prefix", s.Session)
		}
		for i, r := range s.Records {
			if r.TS == "" {
				t.Error("RawRecord.TS must never be empty")
			}
			if r.Role != "document" {
				t.Errorf("default role = %q, want document", r.Role)
			}
			if r.Seq != i {
				t.Errorf("seq = %d, want %d (per-session ordinal)", r.Seq, i)
			}
			if r.Session != s.Session {
				t.Error("RawRecord.Session must equal its group session")
			}
		}
		if len(s.Keys) != len(s.Records) {
			t.Error("Keys must parallel Records")
		}
	}
}

func TestCmdIngestWritesL0AndDedups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	nd := `{"text":"alpha","id":"a","session":"feed","ts":"2026-07-01T00:00:00Z"}
{"text":"beta","id":"b","session":"feed","ts":"2026-07-02T00:00:00Z"}
`
	ing, skip, err := cmdIngest(strings.NewReader(nd), true)
	if err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	if ing != 2 || skip != 0 {
		t.Fatalf("ingested=%d skipped=%d, want 2/0", ing, skip)
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := st.RawCount("file:feed"); n != 2 {
		t.Fatalf("raw count for file:feed = %d, want 2", n)
	}
	// Re-ingest identical → idempotent skip (no growth).
	ing2, _, _ := cmdIngest(strings.NewReader(nd), true)
	if ing2 != 0 {
		t.Fatalf("re-ingest identical wrote %d records, want 0 (idempotent)", ing2)
	}
}
