package opencode

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"
)

func openCodeCommand(ctx context.Context, args ...string) *exec.Cmd {
	binary, err := openCodeExe()
	if err != nil {
		binary = "opencode"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	return cmd
}

func hideOpenCodeWindow(cmd *exec.Cmd) {
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
}
