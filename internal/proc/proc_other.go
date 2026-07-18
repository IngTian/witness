//go:build !linux

package proc

import (
	"log/slog"
	"os"
	"os/exec"
)

// BindToParent is a no-op on macOS/Windows: neither has a Pdeathsig equivalent in
// syscall.SysProcAttr (the field is Linux-only), so there is no way to bind the
// child's lifetime to this process at fork time. ReapOrphans is the best-effort
// startup fallback for a parent that was SIGKILL'd/OOM-killed before its Go cleanup
// could stop the child (issue #54 I2).
func (sys) BindToParent(cmd *exec.Cmd) {}

// ReapOrphans kills any ORPHANED process left behind by a hard-killed launcher,
// using `ps` to find processes that (a) satisfy the caller's fingerprint predicate
// and (b) have been reparented to init (ppid==1) because their launcher is gone.
// The ppid==1 gate is what makes it safe to run even while a live sibling exists:
// any live child is still parented by its live launcher (ppid!=1) and is skipped.
// Best-effort: any failure (no `ps`) is logged at debug and ignored, never blocking
// the caller.
//
// macOS reparents orphans to launchd (pid 1), so the ppid==1 gate is meaningful
// there. On Windows there is no `ps` and orphans are not reparented to a fixed pid,
// so this degrades to a no-op — acceptable while OpenCode-on-Windows is deferred
// (issue #10); Windows' own detached worker relies on the Job Object gap (#42).
func (sys) ReapOrphans(want func(cmdline string) bool) {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		slog.Debug("proc reap: ps unavailable, skipping orphan sweep", "err", err)
		return
	}
	for _, pid := range OrphanPIDs(string(out), os.Getpid(), want) {
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := p.Kill(); err != nil {
			// Almost always a stale pid or a process we don't own — harmless, the
			// fingerprint+orphan gate already ruled out any live sibling.
			slog.Debug("proc reap: could not kill orphan", "pid", pid, "err", err)
			continue
		}
		slog.Info("proc reap: killed orphaned process from a prior launcher", "pid", pid)
	}
}
