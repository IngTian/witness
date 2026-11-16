// Package proc is the process-control port: the small set of OS-process
// capabilities witness actually uses, behind ONE interface (Control) with per-OS
// adapters + a Fake. Before this package, the syscall-level, GOOS-split glue for
// spawning the detached worker, tying the `opencode serve` child's lifetime to the
// worker, and terminating the worker was scattered across //go:build files in
// cmd/commands and internal/platform/opencode, each reaching into syscall directly.
// Centralizing it here keeps the OS-specific reasoning in one place and lets callers
// be unit-tested against a Fake without touching real processes.
//
// It is a LEAF package (no internal/witness imports) so both cmd/commands and
// internal/platform/opencode can depend on it without an import cycle.
//
// GOOS split (each method is defined exactly once per platform):
//   - Detach / TerminateGroup: proc_unix.go (!windows) + proc_windows.go
//   - ContainChildren: proc_unix.go (no-op) + proc_windows.go
//   - BindToParent: proc_linux.go + proc_other.go (!linux)
//   - NotifyStop: here (portable)
package proc

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Control is the process-control port. Methods that configure a child mutate the
// supplied *exec.Cmd (before Start) so the syscall detail never leaks to callers.
// The concrete OS implementation is System(); tests use a Fake.
type Control interface {
	// Detach configures cmd (before Start) to run in its own session / process
	// group so closing the launching terminal doesn't signal it mid-work. Unix:
	// Setsid. Windows: DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP, plus
	// CREATE_BREAKAWAY_FROM_JOB once this process is contained, so the child outlives
	// the worker's job.
	Detach(cmd *exec.Cmd)

	// ContainChildren ties every child THIS process starts afterwards to its death.
	// Windows: the process joins a kill-on-close Job Object, so they die however it
	// dies — the only way a TerminateProcess'd worker takes its `claude -p` along.
	// Unix: a no-op. A SIGTERM reaches them (TerminateGroup's group signal, or NotifyStop's
	// ctx killing each exec.CommandContext child), but nothing covers a hard kill of the
	// worker alone (SIGKILL, OOM): it orphans its `claude -p` (BindToParent's Pdeathsig and
	// the stdin lease cover only the `opencode serve`). Idempotent; the first call's error
	// is returned every time.
	ContainChildren() error

	// BindToParent configures cmd (before Start) so the OS kills the child when
	// THIS process dies — including an unclean death (SIGKILL/OOM) that no Go
	// cleanup can survive. Linux: Pdeathsig=SIGKILL. macOS/Windows have no
	// fork-time equivalent, so this is a no-op there; the `opencode serve` child
	// needs none, because it ends on its stdin lease when this process dies.
	BindToParent(cmd *exec.Cmd)

	// TerminateGroup stops the process identified by pid. Unix: SIGTERM to the
	// process GROUP (-pid) first — the worker leads its own group via Detach — then
	// the bare pid as a fallback, so a `claude -p` child dies with it. Windows has
	// no process-group signal, so it opens the process and terminates it; its
	// children die with the worker's kill-on-close job (ContainChildren).
	TerminateGroup(pid int) error

	// NotifyStop returns a context cancelled on the first SIGINT/SIGTERM, plus a
	// stop func that releases the signal handler. The worker threads this ctx into
	// its drain so a `distill stop` (SIGTERM) or Ctrl-C tears down in-flight
	// distillation children too.
	NotifyStop(parent context.Context) (context.Context, context.CancelFunc)
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
