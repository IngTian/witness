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

// The usage pause is keyed per runner name, and gates calls based on this capability: only OpenCode
// Zen's own opencode/* models draw on the free allowance. The serve allows free models when any of
// its models is one (allowsFree, the same predicate); otherwise it never falls back to one.
func TestOnSharedAllowanceMatchesOnlyOpenCodeModels(t *testing.T) {
	var sa platform.SharedAllowance = &runner{}
	for model, want := range map[string]bool{
		"opencode/glm-5.3-flash-free": true,
		" opencode/x ":                true,
		"openai/x":                    false,
		"opencode-go/x":               false,
		"":                            false,
	} {
		if got := sa.OnSharedAllowance(model); got != want {
			t.Errorf("OnSharedAllowance(%q) = %v, want %v", model, got, want)
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

// An archive from a release with the native-session cache still has <runtime>/opencode-native.
// Nothing reads it any more; Open must succeed with it present and remove it.
func TestOpenRemovesTheRetiredNativeManifests(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")
	useFakeServe(t, "lease")
	root := t.TempDir()
	seedFiles(t, root, "opencode-native/0a1b.json", "opencode-native/0a1b.json.tmp")
	r := &runner{cfg: store.Config{RuntimeRoot: root}}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Open(context.Background()); err != nil {
		t.Fatalf("Open with a leftover opencode-native dir = %v, want nil", err)
	}
	if r.server == nil {
		t.Fatal("Open succeeded but kept no server")
	}
	if _, err := os.Stat(filepath.Join(root, "opencode-native")); !os.IsNotExist(err) {
		t.Errorf("opencode-native after Open: stat err = %v, want not-exist", err)
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
