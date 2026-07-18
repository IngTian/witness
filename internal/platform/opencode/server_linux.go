//go:build linux

package opencode

import "syscall"

// serveSysProcAttr ties the `opencode serve` child's lifetime to the worker's on
// Linux via Pdeathsig=SIGKILL: the moment the parent thread-group dies — including a
// SIGKILL or an OOM-kill the worker's Go cleanup (Close/ctx-cancel) can never
// survive — the kernel SIGKILLs this child too. This is the OpenCode analog of the
// Windows Job Object fix (#42) and the direct answer to issue #54 I2, where a hard
// kill of the worker used to orphan the serve process (a fresh port each start, so
// nothing reclaimed the stray).
//
// Known Go caveat (golang/go#27505): Pdeathsig keys on the death of the OS THREAD
// that forked the child, not the whole process, so a retired creating-thread could
// signal the child early. In practice a full process kill (the case we care about,
// SIGKILL/OOM) tears down every thread at once and the signal fires as intended;
// the graceful path already reaps via Close()/ctx-cancel before any thread churn
// matters.
func serveSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// reapStrayServes is a no-op on Linux: Pdeathsig guarantees the kernel has already
// killed any serve whose worker died, so a startup scan would never find an orphan.
// The pattern-based reap is the macOS/Windows fallback (server_other.go), for the
// platforms that lack a Pdeathsig equivalent.
func reapStrayServes() {}
