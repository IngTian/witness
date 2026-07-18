//go:build windows

package proc

import (
	"fmt"
	"os"
	"os/exec"

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
// KNOWN GAP (issue #42): TerminateProcess is uncatchable, so the worker's stop
// context (NotifyStop, which cancels the drain ctx and thus its `claude -p`
// children on Unix) never runs here, and TerminateProcess does not cascade to
// children — up to mine_concurrency `claude -p` can orphan on a Windows stop (each
// self-reaps at its 10-min ctx timeout). The fix is a Job Object with
// kill-on-close; tracked separately, not blocking.
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
