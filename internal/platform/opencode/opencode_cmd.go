package opencode

import (
	"context"
	"os/exec"
)

func openCodeCommand(ctx context.Context, args ...string) *exec.Cmd {
	binary, err := openCodeExe()
	if err != nil {
		binary = "opencode"
	}
	return exec.CommandContext(ctx, binary, args...)
}
