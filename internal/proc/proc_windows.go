//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Detach is the Windows analogue of Unix setsid for the detached worker.
// DETACHED_PROCESS gives the child no console, so a console-close CTRL_CLOSE_EVENT
// can't reach it mid-distillation; CREATE_NEW_PROCESS_GROUP isolates it from the
// Ctrl-C/Ctrl-Break sent to the parent's group. Together they detach the worker
// from the launching terminal the way Setsid does on Unix. (The SessionStart
// backlog sweep is the fallback recovery path on either platform.)
func (sys) Detach(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd).CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}

// TerminateGroup stops the detached worker on Windows. There is no process-group
// signal equivalent to the Unix SIGTERM-to-(-pid), so we open the process by pid
// and terminate it. os.Process.Kill maps to TerminateProcess. The worker also
// honors the worker_stop_requested meta flag cooperatively; this is the forceful
// backstop the way SIGTERM is on Unix.
//
// KNOWN GAP, UNFIXED (tracked in #216, split out of #199 as F138 part b; #42 was folded into #43, which
// closed as a refactor without it): TerminateProcess is uncatchable, so the worker's
// stop context (NotifyStop, which cancels the drain ctx and thus its `claude -p`
// children on Unix) never runs here, and TerminateProcess does not cascade to
// children — up to mine_concurrency `claude -p` can orphan on a Windows stop, with
// nothing bounding them (the 10-min claudeRunTimeout is a context the worker itself
// enforces, and the worker is what just died), and so does the `opencode serve` child,
// which only the pid-file reaper (opencode reapPriorServe) cleans up, at the next
// drain. The fix is to put the worker itself into a KILL_ON_JOB_CLOSE Job Object at
// startup so its children inherit it; BindToParent runs before Start, so it cannot.
// That job would also kill the detached children the worker means to outlive it
// (spawnDetached's wakeups), so those need CREATE_BREAKAWAY_FROM_JOB — part of why it
// waits for a Windows box to verify on.
func (sys) TerminateGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find worker pid=%d: %w", pid, err)
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("terminate worker pid=%d: %w", pid, err)
	}
	return nil
}

// Alive reports whether pid names a live process. Callers ask "is the OWNER of this record
// still running" — the opencode serve reaper before a kill, the model download lock before
// stealing it — so failing toward "alive" is the safe direction for both: it declines.
//
// Windows has no signal-0 equivalent, and os.FindProcess is useless as a liveness test here:
// it succeeds for a live pid AND is the very case pid reuse produces. So ask the OS for the
// process's exit code — OpenProcess+GetExitCodeProcess via the syscall package — and treat
// STILL_ACTIVE (259) as alive. An OpenProcess failure returns true only when the reason is
// ACCESS_DENIED (the process exists but belongs to a more privileged account); a genuine
// "no such process" returns false.
func Alive(pid int) bool {
	if pid <= 1 {
		return false
	}
	const stillActive = 259
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION|syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// ERROR_ACCESS_DENIED means it EXISTS; anything else (typically
		// ERROR_INVALID_PARAMETER for a dead pid) means it does not.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true // handle opened, state unknown: assume alive (declines)
	}
	return code == stillActive
}
