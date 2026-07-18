// Package proc is the process-control port: the small set of OS-process
// capabilities witness actually uses, behind ONE interface (Control) with per-OS
// adapters + a Fake. Before this package, the syscall-level, GOOS-split glue for
// spawning the detached worker, tying the `opencode serve` child's lifetime to the
// worker, reaping orphaned serves, and terminating the worker was scattered across
// //go:build files in cmd/commands and internal/platform/opencode, each reaching
// into syscall directly. Centralizing it here keeps the OS-specific reasoning in
// one place and lets callers be unit-tested against a Fake without touching real
// processes.
//
// It is a LEAF package (no internal/witness imports) so both cmd/commands and
// internal/platform/opencode can depend on it without an import cycle.
//
// GOOS split (each method is defined exactly once per platform):
//   - Detach / TerminateGroup: proc_unix.go (!windows) + proc_windows.go
//   - BindToParent / ReapOrphans: proc_linux.go + proc_other.go (!linux)
//   - NotifyStop + the pure ps-parse helpers: here (portable)
package proc

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// Control is the process-control port. Methods that configure a child mutate the
// supplied *exec.Cmd (before Start) so the syscall detail never leaks to callers.
// The concrete OS implementation is System(); tests use a Fake.
type Control interface {
	// Detach configures cmd (before Start) to run in its own session / process
	// group so closing the launching terminal doesn't signal it mid-work. Unix:
	// Setsid. Windows: DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP.
	Detach(cmd *exec.Cmd)

	// BindToParent configures cmd (before Start) so the OS kills the child when
	// THIS process dies — including an unclean death (SIGKILL/OOM) that no Go
	// cleanup can survive. Linux: Pdeathsig=SIGKILL. macOS/Windows have no
	// fork-time equivalent, so this is a no-op there and ReapOrphans is the
	// startup fallback.
	BindToParent(cmd *exec.Cmd)

	// ReapOrphans best-effort kills processes that (a) have been reparented to init
	// (ppid==1, i.e. their launcher is gone) and (b) whose command line satisfies
	// want. It is the macOS/Windows fallback for BindToParent: on Linux Pdeathsig
	// has already reaped such children so this is a no-op; on Windows there is no
	// `ps` so it degrades to a no-op too. Callers must ensure no live sibling can
	// match (see the opencode WorkerLock invariant) — the ppid==1 gate already
	// excludes any child of a live launcher. Failures are logged, never returned.
	ReapOrphans(want func(cmdline string) bool)

	// TerminateGroup stops the process identified by pid. Unix: SIGTERM to the
	// process GROUP (-pid) first — the worker leads its own group via Detach — then
	// the bare pid as a fallback, so a `claude -p` child dies with it. Windows has
	// no process-group signal, so it opens the process and terminates it (the
	// child-cascade gap is issue #42, tracked separately).
	TerminateGroup(pid int) error

	// NotifyStop returns a context cancelled on the first SIGINT/SIGTERM, plus a
	// stop func that releases the signal handler. The worker threads this ctx into
	// its drain so a `distill stop` (SIGTERM) or Ctrl-C tears down in-flight
	// distillation children too.
	NotifyStop(parent context.Context) (context.Context, context.CancelFunc)

	// GracefulStop asks a single process to terminate cleanly (SIGTERM), giving it a
	// chance to run its own shutdown before a caller escalates to Kill. Unlike
	// TerminateGroup (which signals the whole process GROUP by negative pid for the
	// detached worker + its `claude -p` child), this targets ONE process the caller
	// already holds — e.g. the opencode-serve child in OpenCodeServer.Close. os.Process.
	// Signal(SIGTERM) is portable across every GOOS Go builds for (on Windows it maps to
	// a TerminateProcess), so this is one portable impl, not a GOOS split — routing it
	// through the port only so the engine holds no direct syscall/signal reference and a
	// caller can be tested against a Fake. A nil process is a no-op.
	GracefulStop(p *os.Process) error
}

// System returns the real, OS-backed process controller.
func System() Control { return sys{} }

// sys is the production Control. Its methods are split across the GOOS-tagged
// files (proc_unix.go / proc_windows.go / proc_linux.go / proc_other.go); the
// signal-aware NotifyStop below is portable.
type sys struct{}

func (sys) NotifyStop(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// GracefulStop sends SIGTERM to one process. os.Process.Signal is the portable
// entry point (Windows maps a SIGTERM Signal to TerminateProcess), so no GOOS split
// is needed. A nil process is a no-op — matching the callers that hold an optional
// child handle.
func (sys) GracefulStop(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGTERM)
}

// ensureSysProcAttr returns cmd's SysProcAttr, allocating a fresh one if unset so
// the per-OS Detach/BindToParent adapters can OR their flags in without clobbering
// any attr a caller (or another adapter) already set. syscall.SysProcAttr exists on
// every GOOS — only its fields differ — so this stays portable.
func ensureSysProcAttr(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// OrphanPIDs is the pure selection primitive behind ReapOrphans: it scans
// `ps`-style output (one process per line, columns `pid ppid command...`) and
// returns the pids of ORPHANED processes — those whose parent is init (ppid==1,
// meaning the launcher is gone) AND whose command line satisfies want. self is the
// current pid, excluded defensively; init (pid<=1) is never a candidate. Exported
// so the fingerprint owner (e.g. opencode) can unit-test its predicate composed
// with the orphan gate on synthetic ps output, without spawning processes.
func OrphanPIDs(psOutput string, self int, want func(cmdline string) bool) []int {
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		pid, rest, ok := psField(line)
		if !ok {
			continue
		}
		ppid, cmdline, ok := psField(rest)
		if !ok {
			continue
		}
		if pid == self || pid <= 1 {
			continue
		}
		if ppid != 1 {
			continue // still owned by a live launcher — not an orphan
		}
		if want(cmdline) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// psField parses one whitespace-separated leading integer column (pid or ppid) off
// a `ps -o pid=,ppid=,command=` line, returning the value and the remainder of the
// line. Shared parser for the reap so column handling is unit-testable without
// spawning processes.
func psField(line string) (int, string, bool) {
	line = strings.TrimSpace(line)
	sp := strings.IndexFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	if sp < 0 {
		return 0, "", false
	}
	n, err := strconv.Atoi(line[:sp])
	if err != nil {
		return 0, "", false
	}
	return n, strings.TrimSpace(line[sp+1:]), true
}
