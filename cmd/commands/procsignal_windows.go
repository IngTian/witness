//go:build windows

package commands

import (
	"fmt"
	"os"
)

// terminateWorkerPID stops the detached worker on Windows. There is no
// process-group signal equivalent to the Unix SIGTERM-to-(-pid), so we open the
// process by pid and terminate it. os.Process.Kill maps to TerminateProcess.
// The worker also honors the worker_stop_requested meta flag cooperatively; this
// is the forceful backstop the way SIGTERM is on Unix.
//
// KNOWN GAP (issue #42): TerminateProcess is uncatchable, so the worker's
// signal.NotifyContext handler (which cancels the drain ctx and thus its `claude
// -p` children on Unix) never runs here, and TerminateProcess does not cascade to
// children — up to mine_concurrency `claude -p` can orphan on a Windows stop (each
// self-reaps at its 10-min ctx timeout). The fix is a Job Object with
// kill-on-close; tracked separately, not blocking.
func terminateWorkerPID(n int) error {
	p, err := os.FindProcess(n)
	if err != nil {
		return fmt.Errorf("find worker pid=%d: %w", n, err)
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("terminate worker pid=%d: %w", n, err)
	}
	return nil
}
