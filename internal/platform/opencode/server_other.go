//go:build !linux

package opencode

import (
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

// serveSysProcAttr is nil on non-Linux platforms: neither macOS nor Windows has a
// Pdeathsig equivalent in syscall.SysProcAttr (the field is Linux-only), so there is
// no way to bind the serve child's lifetime to the worker's at fork time. The
// startup reapStrayServes below is the best-effort fallback for a worker that was
// SIGKILL'd/OOM-killed before its Go cleanup could stop the serve (issue #54 I2).
func serveSysProcAttr() *syscall.SysProcAttr { return nil }

// reapStrayServes kills any ORPHANED witness `opencode serve` process left behind by
// a hard-killed worker, using `ps` to find processes that (a) match our private
// serve fingerprint and (b) have been reparented to init (ppid==1) because their
// launcher is gone. It runs on every StartOpenCodeServer, which is always under
// WorkerLock — so no live witness serve exists to mis-target, and even if one did
// it would be parented by its live worker (ppid!=1) and thus skipped. Best-effort:
// any failure (no `ps`, e.g. on Windows, where OpenCode is unsupported today —
// issue #10) is logged at debug and ignored, never blocking startup.
//
// macOS reparents orphans to launchd (pid 1), so the ppid==1 gate is meaningful
// here. On Windows there is no `ps` and orphans are not reparented to a fixed pid,
// so this degrades to a no-op — acceptable while OpenCode-on-Windows is deferred.
func reapStrayServes() {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		slog.Debug("opencode reap: ps unavailable, skipping stray-serve sweep", "err", err)
		return
	}
	for _, pid := range strayServePIDs(string(out), os.Getpid()) {
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := p.Kill(); err != nil {
			// Almost always a stale pid or a process we don't own — harmless, the
			// fingerprint+orphan gate already ruled out any live sibling.
			slog.Debug("opencode reap: could not kill stray serve", "pid", pid, "err", err)
			continue
		}
		slog.Info("opencode reap: killed orphaned opencode serve from a prior worker", "pid", pid)
	}
}
