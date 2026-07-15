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

	// Count ALL comment lines. Setting a NON-runner key (a model) must preserve every
	// comment verbatim — including the unbound-runner marker, whose survival keeps the
	// npm OpenCode-plugin user's WITNESS_RUNNER fallback intact (see the runner-fallback
	// regression test below). Only binding the runner itself may strip that marker.
	countComments := func(t *testing.T) int {
		t.Helper()
		data, err := os.ReadFile(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
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
	// A model-key write must NOT touch the unbound-runner marker.
	data0, _ := os.ReadFile(s.ConfigPath())
	if !strings.Contains(string(data0), configTemplateUnboundMarker) {
		t.Fatalf("setting a model key stripped the unbound-runner marker; it must survive")
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

// Regression (audit finding): the npm OpenCode-plugin user never runs `witness install`,
// so their runner stays UNBOUND (template marker present, no active runner line, no
// runner_bound meta) and resolves via WITNESS_RUNNER=opencode. Setting an unrelated
// MODEL key — which the tool's own doctor drift-remedy recommends — must NOT strip the
// marker, else configRunnerUnbound() flips to false and ResolveRunner silently returns
// the "claude" template default, breaking all distillation for a user with no claude CLI.
func TestModelSetPreservesOpenCodeRunnerFallback(t *testing.T) {
	s := tempStore(t) // Open() writes the template: marker present, runner commented out
	t.Setenv("WITNESS_RUNNER", RunnerOpenCode)

	// Precondition: the fallback works before any config set.
	if got := s.ResolveRunner(s.LoadConfig()); got != RunnerOpenCode {
		t.Fatalf("precondition: unbound runner should resolve via env to %q, got %q", RunnerOpenCode, got)
	}

	// The exact command doctor recommends for prose drift.
	if err := s.SetConfigString("triage_model", "openai/gpt-5.5"); err != nil {
		t.Fatal(err)
	}

	// The env fallback MUST still win — a model tweak may not touch runner resolution.
	if got := s.ResolveRunner(s.LoadConfig()); got != RunnerOpenCode {
		t.Fatalf("setting a model key broke the OpenCode runner fallback: got %q, want %q", got, RunnerOpenCode)
	}
}
