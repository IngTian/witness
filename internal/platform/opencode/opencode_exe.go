//go:build !windows

package opencode

import "os/exec"

// openCodeExe resolves the `opencode` CLI to an absolute path.
//
// Off Windows a plain PATH lookup is correct: there is no GUI-launcher-vs-headless-CLI
// collision to disambiguate (that is an artifact of the Windows desktop install — see
// opencode_exe_windows.go) and no PATHEXT to honour. Kept as its own function anyway so the
// single spawn helper (openCodeCommand) has one resolution seam on every platform, rather than
// a GOOS branch at the call site.
func openCodeExe() (string, error) {
	return exec.LookPath("opencode")
}
