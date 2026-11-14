package opencode

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The free-model pause keys on this capability, never on the runner's name: only OpenCode Zen's
// own opencode/* models draw on the free allowance.
func TestOnFreeModelMatchesOnlyOpenCodeModels(t *testing.T) {
	var fa platform.FreeAllowance = &runner{}
	for model, want := range map[string]bool{
		"opencode/glm-5.3-flash-free": true,
		" opencode/x ":                true,
		"openai/x":                    false,
		"opencode-go/x":               false,
		"":                            false,
	} {
		if got := fa.OnFreeModel(model); got != want {
			t.Errorf("OnFreeModel(%q) = %v, want %v", model, got, want)
		}
	}
}

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

// The runner must refuse v1 by name with no spawn (a v2 check never leaks the user's API keys to
// an isolated subtree). Both Open and ValidateModels must gate identically.
func TestRunnerRefusesV2UntilThePortLands(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")

	prevCmd, prevExec := openCodeVersionCommand, execCommandContext
	t.Cleanup(func() { openCodeVersionCommand, execCommandContext = prevCmd, prevExec })

	// Stub: execCommandContext must not be called (no serve spawn)
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("spawned %s %v; the v1 gate should refuse before any spawn", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}

	root := t.TempDir()
	r := &runner{cfg: store.Config{RuntimeRoot: root}}

	// v1: both return ErrOpenCodeV1
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("1.19.5"), nil
	}
	err := r.Open(context.Background())
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Errorf("runner.Open() with v1 = %v, want ErrOpenCodeV1", err)
	}
	err = r.ValidateModels(context.Background(), "opencode/x")
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Errorf("runner.ValidateModels() with v1 = %v, want ErrOpenCodeV1", err)
	}
}
