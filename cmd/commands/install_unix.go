//go:build !windows

package commands

// resolveClaudeInstall (Unix) keeps the long-standing behavior: hooks call the
// in-repo witness.sh shim in shell form. The shim locates the per-OS binary,
// exports CLAUDE_PLUGIN_ROOT, sets GOMLX_BACKEND=go, and enforces the recursion
// guard — and it gives a `go run` dev fallback so a fresh checkout works before
// `make build`. Nothing is copied; the install points at the working copy. This
// is deliberately NOT unified with the Windows path (there is no portable shell
// on Windows to run the shim) — see install_windows.go.
func resolveClaudeInstall() (hookInvocation, error) {
	shim, err := repoShim()
	if err != nil {
		return hookInvocation{}, err
	}
	return shellInvocation(shim), nil
}

// resolveOpenCodeInstall (Unix) returns the in-repo witness.sh shim for the OpenCode
// plugin + MCP entry to invoke — the long-standing behavior, unchanged.
//
// Unlike Claude Code (where CC spawns the hook), the OpenCode PLUGIN spawns witness itself
// via Bun.spawn([target, ...args]). That takes an argv array with no shell, so the target
// just has to be directly executable: the shim is on Unix, and the installed .exe is on
// Windows (see install_windows.go). Splitting it here keeps cmdInstallOpenCode
// platform-agnostic — it bakes in whatever this returns.
func resolveOpenCodeInstall() (string, error) {
	return repoShim()
}

// resolveCodexInstall (Unix) points the Codex hooks at the in-repo witness.sh shim,
// matching the other two venues. Codex runs the hook command through a shell, so the
// shim is directly usable — and using it rather than the bare binary is what gives the
// Codex hooks the shim's WITNESS_WORKER recursion guard, its CLAUDE_PLUGIN_ROOT export
// for asset self-location, and its `go run` dev fallback for a fresh checkout.
func resolveCodexInstall() (codexHook, error) {
	shim, err := repoShim()
	if err != nil {
		return codexHook{}, err
	}
	return codexHook{program: shim}, nil
}
