package commands

import (
	"github.com/IngTian/witness/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportHookNeverBreaksSession(t *testing.T) {
	for _, payload := range []string{"{", `{}`, `{"session_id":"missing"}`} {
		out, err := captureStdoutErr(t, func() error { return cmdImportHook("codex", strings.NewReader(payload)) })
		if err != nil || strings.TrimSpace(out) != "{}" {
			t.Fatalf("hook: %q, %v", out, err)
		}
	}
}

func TestImportHookCapturesAndRetries(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("WITNESS_CODEX_HOME", home)
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	fixture := `{"type":"session_meta","payload":{"id":"hook-session"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-28T12:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello from Codex"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(home, "sessions", "rollout-hook-session.jsonl"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		out, err := captureStdoutErr(t, func() error {
			return cmdImportHook("codex", strings.NewReader(`{"session_id":"hook-session","hook_event_name":"Stop"}`))
		})
		if err != nil || strings.TrimSpace(out) != "{}" {
			t.Fatalf("hook: %s, %v", out, err)
		}
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs, err := st.ReadRaw("codex:hook-session")
	if err != nil || len(recs) != 1 || recs[0].Text != "hello from Codex" || st.SessionPlatform("codex:hook-session") != "codex" {
		t.Fatalf("captured %+v, %v", recs, err)
	}
}
