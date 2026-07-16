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

// RunnerSweepsOnClose is the predicate `witness lens try` uses to decide whether it
// must hold the WorkerLock: the OpenCode runner sweeps the shared DB on Close (true),
// the Claude runner does not (false, no-op Close). This is a DIFFERENT axis from
// ConcurrentRunSafe (true for both) — gating on the wrong one would over-lock Claude or
// under-lock OpenCode, so it is pinned here.
func TestRunnerSweepsOnClose(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	cases := map[string]bool{"claude": false, "opencode": true}
	for name, want := range cases {
		r, err := platform.RunnerFor(st, store.Config{Runner: name})
		if err != nil {
			t.Fatalf("RunnerFor %q: %v", name, err)
		}
		if got := platform.RunnerSweepsOnClose(r); got != want {
			t.Fatalf("RunnerSweepsOnClose(%q) = %v, want %v", name, got, want)
		}
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
