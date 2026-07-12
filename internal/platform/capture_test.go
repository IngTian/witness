package platform_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The Capturer path writes L0 AND stamps session_meta.platform, so a later
// ForSession is column-authoritative (not merely prefix-inferred).
func TestClaudeCapturerWritesAndStampsPlatform(t *testing.T) {
	st := openStore(t)
	p, _ := platform.ByName("claude")

	payload, _ := json.Marshal(map[string]any{"session_id": "cc1", "prompt": "hello"})
	ok, err := p.Capture(st, payload, time.Now())
	if err != nil || !ok {
		t.Fatalf("Capture: ok=%v err=%v", ok, err)
	}
	if raw, _ := st.ReadRaw("cc1"); len(raw) != 1 || raw[0].Text != "hello" {
		t.Fatalf("L0 not written: %+v", raw)
	}
	if got := st.SessionPlatform("cc1"); got != "claude" {
		t.Fatalf("platform not stamped: %q", got)
	}
	// And ForSession now resolves it via the column.
	if platform.ForSession(st, "cc1").Name() != "claude" {
		t.Fatal("ForSession should resolve the stamped claude session")
	}
}

// A malformed payload is best-effort: an error is returned but it must not panic;
// cmd logs it and never breaks the session.
func TestClaudeCapturerMalformedPayload(t *testing.T) {
	st := openStore(t)
	p, _ := platform.ByName("claude")
	if _, err := p.Capture(st, []byte("not json"), time.Now()); err == nil {
		t.Fatal("malformed payload should return an error (logged, non-fatal)")
	}
}

func TestOpenCodeCapturerStampsPrefixedSession(t *testing.T) {
	st := openStore(t)
	p, _ := platform.ByName("opencode")

	payload, _ := json.Marshal(map[string]any{
		"event": map[string]any{
			"sessionID": "oc1",
			"role":      "user",
			"parts":     []any{map[string]any{"type": "text", "text": "hi there"}},
		},
	})
	ok, err := p.Capture(st, payload, time.Now())
	if err != nil || !ok {
		t.Fatalf("Capture: ok=%v err=%v", ok, err)
	}
	if got := st.SessionPlatform("opencode:oc1"); got != "opencode" {
		t.Fatalf("platform not stamped on prefixed session: %q", got)
	}
}

// Claude's Importer is a no-op (hook-fed); it must return cleanly with zero work.
func TestClaudeImporterIsNoOp(t *testing.T) {
	st := openStore(t)
	p, _ := platform.ByName("claude")
	stats, err := p.Import(context.Background(), st)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Sessions != 0 || stats.Records != 0 {
		t.Fatalf("claude import should be a no-op, got %+v", stats)
	}
}
