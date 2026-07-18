//go:build !windows

package proc

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Detach starts the child in its own session (setsid), detaching it from the
// controlling terminal's process group. Without this, closing the tab/terminal
// SIGHUPs the detached worker mid-distillation; the SessionStart backlog sweep
// would still recover it next launch, but this keeps the fast path reliable. A
// session leader's pgid equals its pid, which is what TerminateGroup relies on.
func (sys) Detach(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd).Setsid = true
}

// TerminateGroup stops the detached worker. It signals the worker's process GROUP
// first (SIGTERM to -pid) — the worker is started with Setsid, so it leads its own
// group — then falls back to the bare pid. Killing the group also reaps any
// `claude -p` child the worker spawned.
func (sys) TerminateGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate worker pid=%d: %w", pid, err)
	}
	return nil
}
