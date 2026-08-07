package opencode

import (
	"context"
	"os/exec"
)

// openCodeCommand builds every `opencode` invocation witness makes, so the three
// platform concerns are decided in exactly one place instead of at each spawn site.
//
//  1. WHICH BINARY. openCodeExe resolves it (see opencode_exe_windows.go: the desktop
//     build puts a GUI shim on PATH ahead of the real CLI). Falling back to the bare
//     name on error is deliberate — a PATH lookup failure should surface as the
//     familiar "executable file not found" from Start(), not as a different error
//     shape from a helper the caller did not know it was calling.
//  2. NO CONSOLE WINDOW. hideOpenCodeWindow is a GOOS-split no-op/setter
//     (opencode_cmd_windows.go). Windows pops a blank console for every child of a
//     GUI-subsystem parent, so serve/models/export/import each flashed a window on
//     the user's desktop.
//  3. THE TEST SEAM. It routes through execCommandContext, the package's single
//     indirection point, so tests can stub command execution. Calling
//     exec.CommandContext directly here would bypass that and let a unit test spawn
//     the user's real opencode.
func openCodeCommand(ctx context.Context, args ...string) *exec.Cmd {
	binary, err := openCodeExe()
	if err != nil {
		binary = "opencode"
	}
	cmd := execCommandContext(ctx, binary, args...)
	hideOpenCodeWindow(cmd)
	return cmd
}
