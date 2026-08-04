package opencode

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Close on a runner that was never Opened (no work this drain) must be a safe no-op.
func TestRunnerCloseUnopened(t *testing.T) {
	r := &runner{cfg: store.Config{Runner: "opencode"}}
	if err := r.Close(); err != nil {
		t.Fatalf("Close on unopened runner: %v", err)
	}
}

func TestRunnerOpenHonorsExternalProcessFuse(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "1")
	r := &runner{cfg: store.Config{Runner: "opencode", RuntimeRoot: t.TempDir()}}
	if err := r.Open(context.Background()); err == nil || !strings.Contains(err.Error(), platform.DisableExternalRunnersEnv) {
		t.Fatalf("Open fuse error = %v", err)
	}
}
