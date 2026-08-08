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
