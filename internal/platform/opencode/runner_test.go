package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// The runner must refuse v2 until PR 2 ports it; driving the v1 serve protocol against v2
// fails silently (the gate passes 2.x as "newer", the routes return 404, wire reports success
// for a plugin v2 never loads). Both Open and ValidateModels must gate identically, and the
// gate runs BEFORE prepareAuth copies credentials into the runtime (so a v2 check never leaks
// the user's API keys to an isolated subtree).
func TestRunnerRefusesV2UntilThePortLands(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")

	prevCmd, prevExec := openCodeVersionCommand, execCommandContext
	t.Cleanup(func() { openCodeVersionCommand, execCommandContext = prevCmd, prevExec })

	// Stub: execCommandContext must not be called (no prepareAuth, no serve spawn)
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("spawned %s %v; the v2 gate should refuse before any spawn", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}

	root := t.TempDir()
	r := &runner{cfg: store.Config{RuntimeRoot: root}}

	// v2: both Open and ValidateModels return ErrOpenCodeRunnerPending
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("opencode v2.3.0"), nil
	}
	err := r.Open(context.Background())
	if !errors.Is(err, ErrOpenCodeRunnerPending) {
		t.Errorf("runner.Open() with v2 = %v, want ErrOpenCodeRunnerPending", err)
	}
	err = r.ValidateModels(context.Background(), "opencode/x")
	if !errors.Is(err, ErrOpenCodeRunnerPending) {
		t.Errorf("runner.ValidateModels() with v2 = %v, want ErrOpenCodeRunnerPending", err)
	}

	// v1: both return ErrOpenCodeV1
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("1.19.5"), nil
	}
	err = r.Open(context.Background())
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Errorf("runner.Open() with v1 = %v, want ErrOpenCodeV1", err)
	}
	err = r.ValidateModels(context.Background(), "opencode/x")
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Errorf("runner.ValidateModels() with v1 = %v, want ErrOpenCodeV1", err)
	}

	// The runtime's auth.json must not exist after a refused Open (prepareAuth never ran)
	authPath := filepath.Join(root, "xdg", "opencode", "auth.json")
	if _, err := os.Stat(authPath); err == nil {
		t.Errorf("auth.json exists at %s after a refused Open; credentials were copied before the gate", authPath)
	}
}
