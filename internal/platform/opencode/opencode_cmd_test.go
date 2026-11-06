package opencode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/proc"
)

// openCodeCommand owns the whole spawn policy, so no call site can drift from it (the four
// sites once disagreed: `--version` had none of it, `models` no BindToParent, `export` no
// WITNESS_WORKER). All four rules, on one command.
func TestOpenCodeCommandAppliesTheSpawnPolicy(t *testing.T) {
	fake := &proc.Fake{}
	orig := procCtl
	procCtl = fake
	t.Cleanup(func() { procCtl = orig })
	t.Setenv("OPENCODE_DB", "/user/opencode.db")
	t.Setenv("XDG_DATA_HOME", "/user/data")

	root := t.TempDir()
	cmd := openCodeCommand(context.Background(), root, "--version")

	if cmd.Dir != os.TempDir() {
		t.Errorf("cwd = %q, want the neutral temp dir: OpenCode records the cwd's project", cmd.Dir)
	}
	env := map[string]string{}
	for _, e := range cmd.Env {
		k, v, _ := strings.Cut(e, "=")
		if _, dup := env[k]; dup {
			t.Fatalf("duplicate env key %q", k)
		}
		env[k] = v
	}
	if env["WITNESS_WORKER"] != "1" {
		t.Error("WITNESS_WORKER=1 recursion guard missing")
	}
	if env["OPENCODE_DB"] != filepath.Join(root, "opencode.db") || env["XDG_DATA_HOME"] != filepath.Join(root, "xdg") {
		t.Errorf("not isolated to the runtime root: OPENCODE_DB=%q XDG_DATA_HOME=%q", env["OPENCODE_DB"], env["XDG_DATA_HOME"])
	}
	if !slices.Contains(fake.Bound, cmd) {
		t.Error("child not bound to the parent's lifetime (BindToParent)")
	}

	// No runtime root yet: the ambient env stays, the other three rules still hold.
	cmd = openCodeCommand(context.Background(), "", "--version")
	if !slices.Contains(cmd.Env, "OPENCODE_DB=/user/opencode.db") || !slices.Contains(cmd.Env, "WITNESS_WORKER=1") ||
		cmd.Dir != os.TempDir() || !slices.Contains(fake.Bound, cmd) {
		t.Errorf("root-less command lost a rule: dir=%q bound=%v env=%v", cmd.Dir, slices.Contains(fake.Bound, cmd), cmd.Env)
	}
}
