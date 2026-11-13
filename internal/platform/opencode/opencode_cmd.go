package opencode

import (
	"context"
	"os"
	"os/exec"
)

// openCodeCommand builds every `opencode` invocation witness makes, so the platform
// concerns and the spawn policy are decided in exactly one place instead of at each spawn
// site.
//
//  1. WHICH BINARY. openCodeExe resolves it (see opencode_exe_windows.go: the desktop
//     build puts a GUI shim on PATH ahead of the real CLI). Falling back to the bare
//     name on error is deliberate — a PATH lookup failure should surface as the
//     familiar "executable file not found" from Start(), not as a different error
//     shape from a helper the caller did not know it was calling.
//  2. NO CONSOLE WINDOW. hideOpenCodeWindow is a GOOS-split no-op/setter
//     (opencode_cmd_windows.go). Windows pops a blank console for every child of a
//     GUI-subsystem parent, so serve/models/export/--version each flashed a window on
//     the user's desktop.
//  3. THE TEST SEAM. It routes through execCommandContext, the package's single
//     indirection point, so tests can stub command execution. Calling
//     exec.CommandContext directly here would bypass that and let a unit test spawn
//     the user's real opencode.
//  4. THE SPAWN POLICY. Applied per call site it drifted (`--version` had none of it,
//     `models` no BindToParent, `export` no WITNESS_WORKER; the same drift produced two
//     of #119's findings), so every command gets all four rules here. The cwd is pinned
//     to a neutral temp dir: OpenCode derives the PROJECT it records (worktree, vcs,
//     sandboxes) from it, and the worker's cwd is a user repo, since neither
//     spawnDetached nor the plugin's Bun.spawn sets one. WITNESS_WORKER=1 is the
//     recursion guard. isolatedEnv(root) points OPENCODE_DB and XDG_DATA_HOME at
//     witness's runtime root, because several subcommands open their DB read-WRITE and
//     apply migrations, so the ambient env would reach into the USER's database. That
//     holds for root "" too, which ProbeVersion passes (wire and doctor have no runtime
//     root; the runner's entry points refuse an empty one): the relative paths land under
//     the temp-dir cwd — never the user's DB, so the default fails safe. And
//     procCtl.BindToParent makes the child die with the worker on Linux. A caller layers
//     its own variables on top with replaceEnv(cmd.Env, …).
func openCodeCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	binary, err := openCodeExe()
	if err != nil {
		binary = "opencode"
	}
	cmd := execCommandContext(ctx, binary, args...)
	hideOpenCodeWindow(cmd)
	cmd.Dir = os.TempDir()
	cmd.Env = replaceEnv(os.Environ(), append([]string{"WITNESS_WORKER=1"}, isolatedEnv(root)...))
	procCtl.BindToParent(cmd)
	return cmd
}
