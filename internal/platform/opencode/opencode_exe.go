//go:build !windows

package opencode

import "os/exec"

func openCodeExe() (string, error) {
	return exec.LookPath("opencode")
}
