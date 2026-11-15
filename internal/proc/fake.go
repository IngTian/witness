package proc

import (
	"context"
	"os/exec"
)

// Fake is a test double for Control that touches no real OS process. It records only
// what a caller's test asserts on — the cmd handed to BindToParent, the pids handed
// to TerminateGroup (which returns TerminateErr) and the ContainChildren calls (which
// return ContainErr) — and makes Detach a no-op.
type Fake struct {
	Bound      []*exec.Cmd
	Terminated []int
	Contained  int

	// TerminateErr, if set, is returned by TerminateGroup (to simulate a dead pid).
	TerminateErr error
	// ContainErr, if set, is returned by ContainChildren (to simulate a refused job).
	ContainErr error
}

var _ Control = (*Fake)(nil)

func (f *Fake) Detach(*exec.Cmd)           {}
func (f *Fake) BindToParent(cmd *exec.Cmd) { f.Bound = append(f.Bound, cmd) }
func (f *Fake) ContainChildren() error     { f.Contained++; return f.ContainErr }

func (f *Fake) TerminateGroup(pid int) error {
	f.Terminated = append(f.Terminated, pid)
	return f.TerminateErr
}

// NotifyStop returns a plain cancellable child of parent — no real signal handler
// is installed, so tests stay hermetic.
func (f *Fake) NotifyStop(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}
