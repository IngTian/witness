//go:build windows

package opencode

import (
	"os/exec"
	"syscall"
)

// hideOpenCodeWindow suppresses the console window Windows creates for a child process.
//
// witness spawns `opencode` four ways (serve, models, session export, session import) and
// every one flashed a blank console on the desktop — the worker is detached with
// DETACHED_PROCESS, so its children have no console to inherit and Windows allocates a
// fresh one. HideWindow tells the loader not to show it.
//
// It sets the field on any EXISTING SysProcAttr rather than assigning a fresh struct, so it
// composes with an attr another layer already set (proc.BindToParent does the same via
// proc.ensureSysProcAttr). Assigning a new struct here would silently drop those flags —
// which is what the first version of this fix did.
func hideOpenCodeWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
