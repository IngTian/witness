package store

import (
	"os"
	"strings"
	"testing"
)

// SetConfigString replaces a key's line in place (no duplicates), appends it when
// absent, preserves comments and other keys, and clears to "" for a model default.
func TestSetConfigString(t *testing.T) {
	s := tempStore(t) // Open() writes the full commented template

	// Count substantive comment lines, EXCLUDING the "unbound until configured" marker —
	// that marker is intentionally stripped the moment any key is explicitly set (the
	// file becomes user-managed), so it must not be part of the preservation invariant.
	countComments := func(t *testing.T) int {
		t.Helper()
		data, err := os.ReadFile(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			tr := strings.TrimSpace(line)
			if strings.HasPrefix(tr, "#") && tr != configTemplateUnboundMarker {
				n++
			}
		}
		return n
	}
	commentsBefore := countComments(t)

	if err := s.SetConfigString("triage_model", "claude-sonnet-5"); err != nil {
		t.Fatalf("set triage_model: %v", err)
	}
	if got := s.LoadConfig().TriageModel; got != "claude-sonnet-5" {
		t.Fatalf("triage_model not persisted, got %q", got)
	}

	// Setting again REPLACES in place — exactly one active triage_model line.
	if err := s.SetConfigString("triage_model", "claude-opus-4-8"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.ConfigPath())
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if isConfigKeyLine(line, "triage_model") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 triage_model assignment, got %d", n)
	}
	if got := s.LoadConfig().TriageModel; got != "claude-opus-4-8" {
		t.Fatalf("replace failed, got %q", got)
	}

	// Clearing back to "" is valid (model default) and round-trips as empty.
	if err := s.SetConfigString("triage_model", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadConfig().TriageModel; got != "" {
		t.Fatalf("clear failed, got %q", got)
	}

	// Substantive comments (all but the unbound marker) were preserved across rewrites.
	if commentsAfter := countComments(t); commentsAfter != commentsBefore {
		t.Fatalf("comment lines changed: before=%d after=%d", commentsBefore, commentsAfter)
	}
}

// Setting runner via SetConfigString marks it bound, so ResolveRunner honors it over
// any WITNESS_RUNNER env fallback (parity with `install`'s SetRunner).
func TestSetConfigStringRunnerBinds(t *testing.T) {
	s := tempStore(t)
	t.Setenv("WITNESS_RUNNER", "claude") // an env fallback that must NOT win once bound
	if err := s.SetConfigString("runner", RunnerOpenCode); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolveRunner(s.LoadConfig()); got != RunnerOpenCode {
		t.Fatalf("a CLI-set runner must win over the env fallback, got %q", got)
	}
}
