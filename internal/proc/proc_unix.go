//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"os"
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

// Alive reports whether pid names a live process. Callers ask "is the OWNER of this record
// still running" — the opencode serve reaper before a kill, the model download lock before
// stealing it — so a false "alive" is the safe direction for both: it declines.
//
// Unix: signal 0 is the portable liveness probe — it performs the permission and existence
// checks without delivering anything. EPERM means the process EXISTS but belongs to another
// user, which still answers "alive" (and is why the error is inspected rather than assumed
// fatal). Only ESRCH means gone.
func Alive(pid int) bool {
	if pid <= 1 {
		return false
	}
	p, err := os.FindProcess(pid) // never errors on Unix, but check anyway
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
