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

// The free-model pause keys on this capability, never on the runner's name: only OpenCode Zen's
// own opencode/* models draw on the free allowance. The serve allows free models when any of its
// models is one (allowsFree, the same predicate); otherwise it never falls back to one.
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
		if got := allowsFree([]string{"openai/x", model}); got != want {
			t.Errorf("allowsFree([openai/x %q]) = %v, want %v", model, got, want)
		}
	}
	if allowsFree(nil) {
		t.Error("allowsFree(nil) = true, want false")
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

// An Open that fails after the serve started (reconcile cannot read the native dir) must end that
// serve: runnerSet keeps no runner whose Open failed, so nothing would ever Close it, and in the
// MCP server it would live as long as the process.
func TestOpenClosesItsServeWhenReconcileFails(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")
	useFakeServe(t, "lease")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "opencode-native"), nil, 0o600); err != nil { // a file: ReadDir fails
		t.Fatal(err)
	}
	r := &runner{cfg: store.Config{RuntimeRoot: root}}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Open(context.Background()); err == nil {
		t.Fatal("Open succeeded with an unreadable native dir")
	}
	if r.server != nil {
		t.Error("a failed Open kept its server")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "opencode-serve")); err != nil || len(entries) != 0 {
		t.Errorf("serve dirs after the failed Open = %d (%v), want 0: the serve was not closed", len(entries), err)
	}
}

// The runner refuses v1 by name before any serve spawns; Open and ValidateModels gate identically.
// Open first removes the v1-era runtime: the auth.json copy of the user's credentials that witness
// once made, the v1 DB and the pid file. It keeps the v2 DB.
func TestRunnerRefusesV1ByName(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")

	prevCmd, prevExec := openCodeVersionCommand, execCommandContext
	t.Cleanup(func() { openCodeVersionCommand, execCommandContext = prevCmd, prevExec })

	// Stub: execCommandContext must not be called (no serve spawn)
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("spawned %s %v; the v1 gate should refuse before any spawn", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}

	root := t.TempDir()
	v1 := []string{"xdg/opencode/auth.json", "opencode.db", "opencode.db-wal", "opencode.db-shm", ".witness-serve.pid"}
	seedFiles(t, root, append(v1, "opencode-v2.db")...)
	r := &runner{cfg: store.Config{RuntimeRoot: root}}

	// v1: both return ErrOpenCodeV1
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("1.19.5"), nil
	}
	err := r.Open(context.Background())
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Errorf("runner.Open() with v1 = %v, want ErrOpenCodeV1", err)
	}
	for _, p := range append(v1, "xdg") {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Errorf("%s survived Open (stat err %v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "opencode-v2.db")); err != nil {
		t.Errorf("Open removed the v2 DB: %v", err)
	}
	err = r.ValidateModels(context.Background(), "opencode/x")
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Errorf("runner.ValidateModels() with v1 = %v, want ErrOpenCodeV1", err)
	}
}
