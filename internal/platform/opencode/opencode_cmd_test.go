package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	// No runtime root: still isolated (relative to the temp-dir cwd), never the user's DB —
	// the root-less default must fail safe, because several subcommands open it read-WRITE.
	cmd = openCodeCommand(context.Background(), "", "--version")
	if slices.Contains(cmd.Env, "OPENCODE_DB=/user/opencode.db") || slices.Contains(cmd.Env, "XDG_DATA_HOME=/user/data") {
		t.Errorf("root-less command reaches the user's OpenCode data: env=%v", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "OPENCODE_DB=opencode.db") || !slices.Contains(cmd.Env, "WITNESS_WORKER=1") ||
		cmd.Dir != os.TempDir() || !slices.Contains(fake.Bound, cmd) {
		t.Errorf("root-less command lost a rule: dir=%q bound=%v env=%v", cmd.Dir, slices.Contains(fake.Bound, cmd), cmd.Env)
	}
}

// export reads the USER's sessions, so commandOutput must layer the caller's OPENCODE_DB (the
// read-only copy of their opencode.db) over the shared policy's isolated one. Dropping that
// layer still runs, but `opencode export` then reads witness's own empty DB and every
// native-session export fails. Every native test stubs nativeCommand, so only this drives the
// real commandOutput.
func TestCommandOutputLayersTheCallerEnvOverThePolicy(t *testing.T) {
	prev := execCommandContext
	t.Cleanup(func() { execCommandContext = prev })
	var got *exec.Cmd
	execCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// The test binary itself, running no tests: harmless, and present on every OS.
		got = prev(ctx, os.Args[0], "-test.run=^$")
		return got
	}

	root := t.TempDir()
	copyDB := filepath.Join(t.TempDir(), "copy.db")
	if _, err := commandOutput(context.Background(), root, []string{"OPENCODE_DB=" + copyDB}, "export", "ses_1"); err != nil {
		t.Fatalf("commandOutput: %v", err)
	}
	var dbs []string
	for _, e := range got.Env {
		if v, ok := strings.CutPrefix(e, "OPENCODE_DB="); ok {
			dbs = append(dbs, v)
		}
	}
	if len(dbs) != 1 || dbs[0] != copyDB {
		t.Errorf("OPENCODE_DB = %q, want exactly [%q]: export must read the user's DB copy", dbs, copyDB)
	}
	if !slices.Contains(got.Env, "XDG_DATA_HOME="+filepath.Join(root, "xdg")) || !slices.Contains(got.Env, "WITNESS_WORKER=1") {
		t.Errorf("caller env replaced the policy instead of layering on it: %v", got.Env)
	}
	if got.Dir != os.TempDir() {
		t.Errorf("cwd = %q, want the neutral temp dir", got.Dir)
	}
}

func TestResolveExePrefersThePluginBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode-cli.exe")
	os.WriteFile(bin, nil, 0o755)
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	t.Setenv("USERPROFILE", empty)
	t.Setenv("LOCALAPPDATA", empty)
	t.Setenv("WITNESS_OPENCODE_BIN", bin)
	if got, err := ResolveExe(); err != nil || got != bin {
		t.Fatalf("got %q, %v; want the plugin's %q", got, err, bin)
	}
	t.Setenv("WITNESS_OPENCODE_BIN", empty) // a directory is not a binary
	if got, err := ResolveExe(); err == nil {
		t.Fatalf("a directory must be ignored; got %q", got)
	}
}

// Review Focus: `brew upgrade opencode` deletes the Cellar path the plugin passed, while a sleeping
// worker-wakeup still carries it in its env. Nothing is executed: LookPath only stats the file.
func TestResolveExeFallsBackWhenThePluginBinaryIsGone(t *testing.T) {
	name := "opencode"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := t.TempDir()
	onPath := filepath.Join(dir, name)
	os.WriteFile(onPath, nil, 0o755)
	t.Setenv("PATH", dir)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("WITNESS_OPENCODE_BIN", filepath.Join(t.TempDir(), "Cellar", "opencode", "2.2.7", "bin", "opencode"))
	if got, err := ResolveExe(); err != nil || !strings.EqualFold(got, onPath) {
		t.Fatalf("got %q, %v; want PATH's %q", got, err, onPath)
	}
}
