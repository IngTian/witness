package opencode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// openCodeCommand owns the whole spawn policy, so no call site can drift from it (the four
// sites once disagreed: `--version` had none of it, `export` no WITNESS_WORKER). Every rule,
// on one command.
func TestOpenCodeCommandAppliesTheSpawnPolicy(t *testing.T) {
	// Spawn isolation (critic.md G1): every inherited OPENCODE_* var is dropped
	// (case-insensitively, since Windows env names are case-insensitive), and witness's own
	// data, cache, state dirs and DB are under the runtime root, never the user's. HOME stays
	// (Bedrock credentials). API-key env vars stay (the runner copies no credentials).
	for k, v := range map[string]string{"OPENCODE_DB": "/user/opencode.db", "XDG_DATA_HOME": "/user/data",
		"OPENCODE_CONFIG": "/user/leak.json", "OPENCODE_CONFIG_CONTENT": `{"agent":{"leak":{}}}`,
		"OPENCODE_SERVER_PASSWORD": "x", "OPENCODE_CONFIG_PROJECT_DISABLE": "0", "opencode_config_dir": "/user/cfg",
		"WITNESS_OPENCODE_BIN": "/x/opencode", "HOME": "/home/u"} {
		t.Setenv(k, v)
	}

	root := t.TempDir()
	cmd := openCodeCommand(context.Background(), root, "--version")
	if cmd.Dir != os.TempDir() {
		t.Errorf("cwd = %q, want the neutral temp dir: OpenCode records the cwd's project", cmd.Dir)
	}
	env := envMap(t, cmd.Env)
	for k := range env {
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "OPENCODE_") && up != "OPENCODE_DB" && up != "OPENCODE_API_KEY" {
			t.Errorf("inherited %s reached opencode (critic G1)", k)
		}
	}
	want := map[string]string{"OPENCODE_DB": filepath.Join(root, "opencode-v2.db"), "XDG_DATA_HOME": filepath.Join(root, "opencode-data"),
		"XDG_CACHE_HOME": filepath.Join(root, "opencode-cache"), "XDG_STATE_HOME": filepath.Join(root, "opencode-state"),
		"HOME": "/home/u", "WITNESS_OPENCODE_BIN": "/x/opencode", "WITNESS_WORKER": "1"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// OPENCODE_API_KEY passes in either spelling (spec §7.6: env-var keys). One spelling per
	// subtest: on Windows the two are ONE variable, so the entry keeps whichever name the OS
	// holds and is matched case-insensitively there.
	for _, name := range []string{"OPENCODE_API_KEY", "opencode_api_key"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "sk-zen-"+name)
			var got []string
			for _, e := range openCodeCommand(context.Background(), root, "--version").Env {
				k, v, _ := strings.Cut(e, "=")
				if k == name || runtime.GOOS == "windows" && strings.EqualFold(k, name) {
					got = append(got, v)
				}
			}
			if len(got) != 1 || got[0] != "sk-zen-"+name {
				t.Errorf("%s reached opencode as %q, want exactly [sk-zen-%s]", name, got, name)
			}
		})
	}

	// No runtime root: still isolated (relative to the temp-dir cwd), never the user's DB —
	// the root-less default must fail safe, because several subcommands open it read-WRITE.
	cmd = openCodeCommand(context.Background(), "", "--version")
	env = envMap(t, cmd.Env)
	if env["OPENCODE_DB"] != "opencode-v2.db" || env["XDG_DATA_HOME"] != "opencode-data" || env["WITNESS_WORKER"] != "1" ||
		cmd.Dir != os.TempDir() {
		t.Errorf("root-less command lost a rule: dir=%q env=%v", cmd.Dir, cmd.Env)
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
