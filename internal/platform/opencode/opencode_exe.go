//go:build !windows

package opencode

import "os/exec"

// ResolveExe resolves the `opencode` CLI to an absolute path.
//
// Off Windows a plain PATH lookup is correct: there is no GUI-launcher-vs-headless-CLI
// collision to disambiguate (that is an artifact of the Windows desktop install — see
// opencode_exe_windows.go) and no PATHEXT to honour. The plugin-supplied binary is checked
// first, because the plugin knows where OpenCode actually runs from (Homebrew Cellar, a
// standalone unpack, etc.), which beats a stale PATH entry. Kept as its own function anyway
// so the single spawn helper (openCodeCommand) has one resolution seam on every platform,
// rather than a GOOS branch at the call site.
func ResolveExe() (string, error) {
	if path, ok := pluginExe(); ok {
		return path, nil
	}
	return exec.LookPath("opencode")
}
