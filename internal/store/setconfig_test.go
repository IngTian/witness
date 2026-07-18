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
	// comment verbatim. (Runner resolution no longer rides on any comment — issue #71
	// made the runner_bound flag the sole authority — but comments are still part of
	// the user-managed template and shouldn't be silently churned by a model write.)
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
	// A model-key write must NOT bind the runner (issue #71): the runner_bound flag
	// is the sole resolution authority, and a model tweak must leave it untouched.
	if s.MetaString(runnerBoundKey) == "1" {
		t.Fatalf("setting a model key bound the runner; runner resolution must be independent of model writes")
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

// Regression (issue #71, audit finding): the npm OpenCode-plugin user never runs
// `witness install`, so their runner stays UNBOUND (no runner_bound meta, only a
// commented runner line) and resolves via WITNESS_RUNNER=opencode. Setting an
// unrelated MODEL key — which the tool's own doctor drift-remedy recommends — must
// NOT bind the runner, else ResolveRunner would return the "claude" template
// default and break all distillation for a user with no claude CLI. With the flag
// as the sole authority this is structural (a model write never stamps the flag),
// but the test stays as the guard that the coupling can't return.
func TestModelSetPreservesOpenCodeRunnerFallback(t *testing.T) {
	s := tempStore(t) // Open() writes the template: runner commented out, flag unset
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
