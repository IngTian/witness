//go:build !windows

package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// isOurServePID reports whether pid is a live witness-private `opencode serve`.
//
// Unix asks `ps` for that one pid's command line and applies the SAME fingerprint the
// orphan sweep uses (isStrayServeLine), so the two reapers agree on what "our serve" means.
// A non-zero exit from ps means no such process — treated as "not ours", which is the safe
// answer for a caller that is deciding whether to send a kill.
//
// On Unix this function is largely redundant with procCtl.ReapOrphans, which already sweeps
// by fingerprint; the pid-file path exists for Windows, where a ppid-based orphan sweep is
// unavailable. Implementing it here anyway keeps the reap logic identical on both platforms
// rather than leaving a Windows-only code path that no one can exercise locally.
func isOurServePID(pid int) bool {
	// BOUNDED for the same reason as the Windows variant: this runs under the machine-wide
	// WorkerLock (reapPriorServe <- StartOpenCodeServerIn), so an unbounded child would wedge every
	// drain. `ps` is normally instant, but "normally" is not a bound.
	ctx, cancel := context.WithTimeout(context.Background(), openCodeProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return isStrayServeLine(string(out))
}

// processAlive reports whether pid names a live process.
//
// Unix: signal 0 is the portable liveness probe — it performs the permission and existence
// checks without delivering anything. EPERM means the process EXISTS but belongs to another
// user, which still answers "alive" (and is why the error is inspected rather than assumed
// fatal). Only ESRCH means gone.
//
// It answers "is the OWNER still running", so a false positive is the safe direction: it makes
// reapPriorServe decline to kill.
func processAlive(pid int) bool {
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
