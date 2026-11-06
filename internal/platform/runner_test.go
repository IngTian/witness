package platform_test

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

// RunnerForName resolves a runner by name (fail-closed on an unknown name) and
// tolerates Close on an unopened runner (no work this drain).
func TestRunnerForName(t *testing.T) {
	for _, name := range []string{"", "claude", "opencode"} {
		r, err := platform.RunnerForName(name, store.Config{})
		if err != nil {
			t.Fatalf("runner %q should resolve, got %v", name, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close on unopened %q runner: %v", name, err)
		}
	}

	if _, err := platform.RunnerForName("bogus", store.Config{}); err == nil {
		t.Fatal("unknown runner must fail closed")
	}
}

// (TestRunnerSweepsOnClose used to live here. It asserted that BOTH built-in runners return false —
// i.e. it pinned the very fact that made the SweepsOnCloser capability unreachable. The capability is
// gone from the port; see platform.Runner for why the shared-DB sweep it reported on no longer
// exists.)

// InvocationHint distinguishes the two runners for doctor/diagnostics.
func TestRunnerInvocationHint(t *testing.T) {
	cc, _ := platform.RunnerForName("claude", store.Config{})
	if cc.InvocationHint() != "claude -p" {
		t.Fatalf("claude hint = %q", cc.InvocationHint())
	}
	oc, _ := platform.RunnerForName("opencode", store.Config{})
	if oc.InvocationHint() != "opencode serve" {
		t.Fatalf("opencode hint = %q", oc.InvocationHint())
	}
}
