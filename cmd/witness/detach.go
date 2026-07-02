package main

import "syscall"

// detachSysProcAttr starts a child in its own session (setsid), detaching it from
// the controlling terminal's process group. Without this, closing the tab/terminal
// SIGHUPs the detached worker mid-distillation; the SessionStart backlog sweep
// would still recover it next launch, but this keeps the fast path reliable.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
