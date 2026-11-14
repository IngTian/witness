//go:build !windows

package opencode

import (
	"os"
	"os/exec"
)

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
