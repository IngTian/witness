package claude

import (
	"testing"
	"time"

	"github.com/IngTian/claude-witness/internal/store"
)

// openTmpStore gives each test its own on-disk store under a temp WITNESS_HOME.
func openTmpStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustReadRaw(t *testing.T, st *store.Store, session string) []store.RawRecord {
	t.Helper()
	recs, err := st.ReadRaw(session)
	if err != nil {
		t.Fatalf("ReadRaw(%q): %v", session, err)
	}
	return recs
}

// The Claude Code capture path (HookEvent -> Capture -> raw row) is the tool's
// core promise: don't lose turns. These cases mirror what Claude Code actually
// sends on UserPromptSubmit and Stop, plus the degenerate events, so a JSON-tag
// or routing regression fails loudly instead of silently capturing nothing.

func TestCaptureUserPrompt(t *testing.T) {
	st := openTmpStore(t)
	e := HookEvent{HookEventName: "UserPromptSubmit", SessionID: "s1", Prompt: "hello"}
	if err := Capture(st, e, time.Now()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	recs := mustReadRaw(t, st, "s1")
	if len(recs) != 1 {
		t.Fatalf("want 1 raw record, got %d", len(recs))
	}
	if recs[0].Role != "user" || recs[0].Text != "hello" {
		t.Fatalf("user turn mis-captured: role=%q text=%q", recs[0].Role, recs[0].Text)
	}
}

func TestCaptureAssistantWithEffort(t *testing.T) {
	st := openTmpStore(t)
	e := HookEvent{
		HookEventName:        "Stop",
		SessionID:            "s1",
		LastAssistantMessage: "hi back",
		Effort:               map[string]any{"level": "high"},
	}
	if err := Capture(st, e, time.Now()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	recs := mustReadRaw(t, st, "s1")
	if len(recs) != 1 {
		t.Fatalf("want 1 raw record, got %d", len(recs))
	}
	if recs[0].Role != "assistant" || recs[0].Text != "hi back" {
		t.Fatalf("assistant turn mis-captured: role=%q text=%q", recs[0].Role, recs[0].Text)
	}
	if recs[0].Effort != "high" {
		t.Fatalf("effort level not extracted from map: got %q", recs[0].Effort)
	}
}

// When both fields are present (should not happen from real CC, but be defensive)
// the prompt wins — matching the switch order in Capture — and it's a user turn.
func TestCaptureBothFieldsPrefersPrompt(t *testing.T) {
	st := openTmpStore(t)
	e := HookEvent{HookEventName: "Stop", SessionID: "s1", Prompt: "P", LastAssistantMessage: "A"}
	if err := Capture(st, e, time.Now()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	recs := mustReadRaw(t, st, "s1")
	if len(recs) != 1 || recs[0].Role != "user" || recs[0].Text != "P" {
		t.Fatalf("both-fields case should capture the prompt as a user turn, got %+v", recs)
	}
}

// An empty session_id (or a content-free event) must be a silent no-op, never an
// error and never an orphan row: capture must not break the user's session.
func TestCaptureNoOpCases(t *testing.T) {
	st := openTmpStore(t)
	cases := []struct {
		name string
		e    HookEvent
	}{
		{"empty session", HookEvent{HookEventName: "UserPromptSubmit", Prompt: "orphan"}},
		{"no content", HookEvent{HookEventName: "Stop", SessionID: "s1"}},
	}
	for _, c := range cases {
		if err := Capture(st, c.e, time.Now()); err != nil {
			t.Fatalf("%s: Capture returned error (must be nil): %v", c.name, err)
		}
	}
	// Neither case should have written anything for s1.
	if recs := mustReadRaw(t, st, "s1"); len(recs) != 0 {
		t.Fatalf("no-op cases wrote %d rows, want 0", len(recs))
	}
}

// Successive captures in a session get monotonic, gap-free per-session sequence
// numbers (0,1,2...) — the unit the distillation watermark counts in.
func TestCaptureAssignsMonotonicSeq(t *testing.T) {
	st := openTmpStore(t)
	now := time.Now()
	for i, text := range []string{"a", "b", "c"} {
		var e HookEvent
		if i%2 == 0 {
			e = HookEvent{HookEventName: "UserPromptSubmit", SessionID: "s1", Prompt: text}
		} else {
			e = HookEvent{HookEventName: "Stop", SessionID: "s1", LastAssistantMessage: text}
		}
		if err := Capture(st, e, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("Capture #%d: %v", i, err)
		}
	}
	recs := mustReadRaw(t, st, "s1")
	if len(recs) != 3 {
		t.Fatalf("want 3 rows, got %d", len(recs))
	}
	for i, r := range recs {
		if r.Seq != i {
			t.Fatalf("row %d has seq %d, want %d (seq must be gap-free 0..n)", i, r.Seq, i)
		}
	}
}
