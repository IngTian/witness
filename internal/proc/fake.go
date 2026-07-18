package proc

import (
	"context"
	"os/exec"
)

// Fake is a test double for Control that records how it was driven and touches no
// real OS process. It lets a caller be exercised end-to-end — "does spawnDetached
// actually call Detach on the cmd it starts?" — without setsid/Pdeathsig/ps/kill
// ever running. All configuration methods record the *exec.Cmd they were handed;
// TerminateGroup records the pid and returns TerminateErr; ReapOrphans records the
// predicate (and, if ReapPS is set, the pids it would select from that fixture).
type Fake struct {
	Detached      []*exec.Cmd
	Bound         []*exec.Cmd
	Terminated    []int
	NotifyStops   int
	ReapCalls     int
	ReapPredicate func(cmdline string) bool // the last predicate passed to ReapOrphans

	// TerminateErr, if set, is returned by TerminateGroup (to simulate a dead pid).
	TerminateErr error
	// ReapPS, if non-empty, is treated as synthetic `ps` output: ReapOrphans runs
	// OrphanPIDs against it with the supplied predicate and appends the selected
	// pids to ReapSelected, so a test can assert the fingerprint+orphan gate without
	// spawning processes.
	ReapPS       string
	ReapSelected []int
}

var _ Control = (*Fake)(nil)

func (f *Fake) Detach(cmd *exec.Cmd)       { f.Detached = append(f.Detached, cmd) }
func (f *Fake) BindToParent(cmd *exec.Cmd) { f.Bound = append(f.Bound, cmd) }

func (f *Fake) ReapOrphans(want func(cmdline string) bool) {
	f.ReapCalls++
	f.ReapPredicate = want
	if f.ReapPS != "" && want != nil {
		f.ReapSelected = append(f.ReapSelected, OrphanPIDs(f.ReapPS, -1, want)...)
	}
}

func (f *Fake) TerminateGroup(pid int) error {
	f.Terminated = append(f.Terminated, pid)
	return f.TerminateErr
}

// NotifyStop returns a plain cancellable child of parent — no real signal handler
// is installed, so tests stay hermetic. The count is recorded for assertions.
func (f *Fake) NotifyStop(parent context.Context) (context.Context, context.CancelFunc) {
	f.NotifyStops++
	return context.WithCancel(parent)
}
