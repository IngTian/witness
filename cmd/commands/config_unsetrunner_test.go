package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// `config unset runner` must UNBIND the runner, not pin it to the template default.
//
// It called st.SetRunner(""), which writes a `runner` line AND stamps runner_bound=1 — so the
// command did the opposite of what it says. Measured on a fresh archive with
// WITNESS_RUNNER=opencode (the npm OpenCode user who never ran `install`, whose distillation
// works purely through the plugin env): resolution was "opencode" before and a BOUND "claude"
// after. Every drain then shells a `claude` binary that user does not have, each (session,lens)
// fails and backs off, and no amount of re-setting WITNESS_RUNNER recovers it — only an explicit
// `config set runner opencode` does.
func TestConfigUnsetRunnerUnbindsInsteadOfPinningClaude(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_RUNNER", "opencode")

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.ResolveRunner(st.LoadConfig()); got != "opencode" {
		t.Fatalf("precondition: an unbound archive should resolve WITNESS_RUNNER, got %q", got)
	}

	if err := configApplySet(st, "runner", "", ""); err != nil {
		t.Fatal(err)
	}

	if got := st.ResolveRunner(st.LoadConfig()); got != "opencode" {
		t.Errorf("after `unset runner` the env fallback must still win, got %q — the user is "+
			"pinned to a runner they never chose and cannot un-pin", got)
	}
	if bound := st.MetaString("runner_bound"); bound == "1" {
		t.Error("`unset runner` stamped runner_bound=1: it BOUND the runner instead of unbinding it")
	}
}

// The line must be REMOVED, not blanked: adoptRunnerBound re-stamps the bound flag whenever any
// `runner` line exists, so a `runner = ""` line would silently re-bind on the next Open.
func TestConfigUnsetRunnerRemovesTheLineSoItCannotRebind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_RUNNER", "opencode")

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Bind it first, as `install` would.
	if err := st.SetRunner("claude"); err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "runner", "", ""); err != nil {
		t.Fatal(err)
	}
	path := st.ConfigPath()
	st.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "runner") && strings.Contains(trimmed, "=") {
			t.Errorf("an active runner line survived `unset runner`: %q — adoptRunnerBound will "+
				"re-stamp runner_bound on the next Open and silently re-pin the user", trimmed)
		}
	}

	// Re-open: the flag must STILL be clear and the env fallback must still win.
	st2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if bound := st2.MetaString("runner_bound"); bound == "1" {
		t.Error("re-opening the archive re-bound the runner — the line was blanked, not removed")
	}
	if got := st2.ResolveRunner(st2.LoadConfig()); got != "opencode" {
		t.Errorf("after re-open the env fallback must still win, got %q", got)
	}
}

// An EXPLICIT choice must still bind — that is the whole point of the flag (a dual CC+OpenCode
// user who ran `install` must not be hijacked by the plugin's env).
func TestConfigSetRunnerStillBinds(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_RUNNER", "opencode")

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := configApplySet(st, "runner", "claude", ""); err != nil {
		t.Fatal(err)
	}
	if bound := st.MetaString("runner_bound"); bound != "1" {
		t.Errorf("an explicit `config set runner claude` must bind, got runner_bound=%q", bound)
	}
	if got := st.ResolveRunner(st.LoadConfig()); got != "claude" {
		t.Errorf("an explicit choice must beat WITNESS_RUNNER, got %q", got)
	}
}

// Clearing a MODEL key must keep its old behavior: write an empty value and NOT touch the
// runner-bound flag. The runner special-case must not leak into other keys.
func TestConfigUnsetModelKeepsWritingAnEmptyValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "mine_model", "some-model", ""); err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "mine_model", "", ""); err != nil {
		t.Fatal(err)
	}
	if bound := st.MetaString("runner_bound"); bound == "1" {
		t.Error("clearing a model key must not bind the runner")
	}
	if got := st.LoadConfig().TriageModel; got != "" {
		t.Errorf("cleared model should read empty, got %q", got)
	}
	path := st.ConfigPath()
	st.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "triage_model") {
		t.Errorf("the model key's LINE should remain (written as empty), got:\n%s", data)
	}
}

// A no-op unset on an archive that never had a runner line must not error or create one.
func TestConfigUnsetRunnerIsIdempotent(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 3; i++ {
		if err := configApplySet(st, "runner", "", ""); err != nil {
			t.Fatalf("unset #%d: %v", i+1, err)
		}
	}
	if bound := st.MetaString("runner_bound"); bound == "1" {
		t.Error("repeated unsets bound the runner")
	}
}
