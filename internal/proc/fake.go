package proc

import (
	"context"
	"os"
	"os/exec"
)

// Fake is a test double for Control that touches no real OS process. It records only
// what a caller's test asserts on — the cmd handed to BindToParent, the pids handed to
// TerminateGroup (which returns TerminateErr), the processes handed to GracefulStop —
// and makes Detach and ReapOrphans no-ops.
type Fake struct {
	Bound         []*exec.Cmd
	Terminated    []int
	GracefulStops []*os.Process // processes passed to GracefulStop, in order

	// TerminateErr, if set, is returned by TerminateGroup (to simulate a dead pid).
	TerminateErr error
}

var _ Control = (*Fake)(nil)

func (f *Fake) Detach(*exec.Cmd)              {}
func (f *Fake) BindToParent(cmd *exec.Cmd)    { f.Bound = append(f.Bound, cmd) }
func (f *Fake) ReapOrphans(func(string) bool) {}

func (f *Fake) TerminateGroup(pid int) error {
	f.Terminated = append(f.Terminated, pid)
	return f.TerminateErr
}

// GracefulStop records the process it was handed. No real signal is sent.
func (f *Fake) GracefulStop(p *os.Process) error {
	f.GracefulStops = append(f.GracefulStops, p)
	return nil
}

// NotifyStop returns a plain cancellable child of parent — no real signal handler
// is installed, so tests stay hermetic.
func (f *Fake) NotifyStop(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}
