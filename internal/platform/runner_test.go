package platform_test

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

// RunnerFor resolves the global runner by cfg (fail-closed on an unknown name) and
// tolerates Close on an unopened runner (no work this drain).
func TestRunnerFor(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	for _, name := range []string{"", "claude", "opencode"} {
		r, err := platform.RunnerFor(st, store.Config{Runner: name})
		if err != nil {
			t.Fatalf("runner %q should resolve, got %v", name, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close on unopened %q runner: %v", name, err)
		}
	}

	if _, err := platform.RunnerFor(st, store.Config{Runner: "bogus"}); err == nil {
		t.Fatal("unknown runner must fail closed")
	}
}

// InvocationHint distinguishes the two runners for doctor/diagnostics.
func TestRunnerInvocationHint(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	cc, _ := platform.RunnerFor(st, store.Config{Runner: "claude"})
	if cc.InvocationHint() != "claude -p" {
		t.Fatalf("claude hint = %q", cc.InvocationHint())
	}
	oc, _ := platform.RunnerFor(st, store.Config{Runner: "opencode"})
	if oc.InvocationHint() != "opencode serve" {
		t.Fatalf("opencode hint = %q", oc.InvocationHint())
	}
}
