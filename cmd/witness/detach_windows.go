//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachSysProcAttr is the Windows analogue of Unix setsid for the detached worker.
// DETACHED_PROCESS gives the child no console, so a console-close CTRL_CLOSE_EVENT
// can't reach it mid-distillation; CREATE_NEW_PROCESS_GROUP isolates it from
// Ctrl-C/Ctrl-Break sent to the parent's group. Together they detach the worker
// from the launching terminal the way Setsid does on Unix. (The SessionStart
// backlog sweep is the fallback recovery path on either platform.)
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
