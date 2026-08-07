//go:build !windows

package opencode

import "os/exec"

// hideOpenCodeWindow is a no-op off Windows: there is no console window to hide.
//
// This MUST be a build-tagged file rather than a `runtime.GOOS == "windows"` branch.
// syscall.SysProcAttr is a different struct on every platform and HideWindow exists only
// in the Windows one, so a GOOS check — which is evaluated at RUN time — still fails to
// COMPILE off Windows:
//
//	unknown field HideWindow in struct literal of type syscall.SysProcAttr
//
// The repo already uses this pattern for the same reason (opencode_exe.go /
// opencode_exe_windows.go, and the whole internal/proc GOOS matrix).
func hideOpenCodeWindow(*exec.Cmd) {}
