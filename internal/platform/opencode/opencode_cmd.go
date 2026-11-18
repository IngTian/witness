package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IngTian/witness/internal/proc"
)

// openCodeCommand builds every `opencode` invocation witness makes, so the platform
// concerns and the spawn policy are decided in exactly one place instead of at each spawn
// site.
//
//  1. WHICH BINARY. ResolveExe resolves it (see opencode_exe_windows.go: the desktop
//     build puts a GUI shim on PATH ahead of the real CLI). Falling back to the bare
//     name on error is deliberate — a PATH lookup failure should surface as the
//     familiar "executable file not found" from Start(), not as a different error
//     shape from a helper the caller did not know it was calling.
//  2. NO CONSOLE WINDOW. proc.HideWindow (a no-op off Windows). Windows pops a blank
//     console for every child of a GUI-subsystem parent, so serve and --version each
//     flash a window on the desktop without it.
//  3. THE TEST SEAM. It routes through execCommandContext, the package's single
//     indirection point, so tests can stub command execution. Calling
//     exec.CommandContext directly here would bypass that and let a unit test spawn
//     the user's real opencode.
//  4. THE SPAWN POLICY. Applied per call site it drifted (`--version` had none of it,
//     `export` no WITNESS_WORKER; the same drift produced two of #119's findings), so
//     every command gets every rule here. The cwd is pinned
//     to a neutral temp dir: OpenCode derives the PROJECT it records (worktree, vcs,
//     sandboxes) from it, and the worker's cwd is a user repo, since neither
//     spawnDetached nor the plugin's Bun.spawn sets one. WITNESS_WORKER=1 is the
//     recursion guard. withoutOpenCodeEnv drops every inherited OPENCODE_* variable
//     (case-insensitively, since Windows env names are case-insensitive) to prevent the
//     user's config, plugins, MCP servers and credentials from leaking into witness's
//     private opencode serve (critic.md G1). isolatedEnv(root) points OPENCODE_DB,
//     XDG_DATA_HOME, XDG_CACHE_HOME and XDG_STATE_HOME at witness's runtime root, because
//     several subcommands open their DB read-WRITE and apply migrations, so the ambient
//     env would reach into the USER's database. That holds for root "" too, which
//     ProbeVersion passes (wire and doctor have no runtime root; the runner's entry points
//     refuse an empty one): the relative paths land under the temp-dir cwd — never the
//     user's DB, so the default fails safe. A caller layers its own variables on top with
//     replaceEnv(cmd.Env, …).
func openCodeCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	binary, err := ResolveExe()
	if err != nil {
		binary = "opencode"
	}
	cmd := execCommandContext(ctx, binary, args...)
	proc.HideWindow(cmd)
	cmd.Dir = os.TempDir()
	cmd.Env = replaceEnv(withoutOpenCodeEnv(os.Environ()), append([]string{"WITNESS_WORKER=1"}, isolatedEnv(root)...))
	return cmd
}

const openCodeBinEnv = "WITNESS_OPENCODE_BIN"

// pluginExe returns the plugin-supplied binary when WITNESS_OPENCODE_BIN names an existing
// regular file. A missing path or a directory returns ("", false), so resolution falls through.
func pluginExe() (string, bool) {
	path := os.Getenv(openCodeBinEnv)
	if path == "" {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

// isolatedEnv returns the data, cache, state dirs and DB path under witness's runtime root,
// so every spawned opencode process writes to witness's private copy, never the user's.
func isolatedEnv(root string) []string {
	db := "opencode-v2.db"
	data := "opencode-data"
	cache := "opencode-cache"
	state := "opencode-state"
	if root != "" {
		db = filepath.Join(root, db)
		data = filepath.Join(root, data)
		cache = filepath.Join(root, cache)
		state = filepath.Join(root, state)
	}
	return []string{
		"XDG_DATA_HOME=" + data,
		"XDG_CACHE_HOME=" + cache,
		"XDG_STATE_HOME=" + state,
		"OPENCODE_DB=" + db,
	}
}

// withoutOpenCodeEnv drops every inherited OPENCODE_* variable except OPENCODE_API_KEY (an
// env-var key, allowed by spec §7.6), case-insensitively since Windows env names are
// case-insensitive, to prevent the user's config, plugins and MCP servers from leaking into
// witness's private opencode serve (critic.md G1). HOME stays (Bedrock's credential chain
// reads ~/.aws). Other API-key env vars stay (the runner copies no credentials, spec §7.6).
func withoutOpenCodeEnv(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		up := strings.ToUpper(k)
		// OPENCODE_API_KEY is a credential in the allowed "env-var API keys" class (spec §7.6:
		// the runner accepts env-var keys but copies no credentials). The strip exists to stop
		// config and behaviour leaks (OPENCODE_CONFIG / OPENCODE_CONFIG_CONTENT / OPENCODE_CONFIG_DIR).
		if !strings.HasPrefix(up, "OPENCODE_") || up == "OPENCODE_API_KEY" {
			out = append(out, e)
		}
	}
	return out
}
func replaceEnv(base, updates []string) []string {
	m := map[string]string{}
	order := []string{}
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = e
	}
	for _, e := range updates {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = e
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

// indirection keeps command execution testable without touching process globals.
var execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
