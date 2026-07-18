//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// The detached worker must outlive the terminal/tab that spawned it: closing a
// terminal SIGHUPs its process group, so the worker has to be in its OWN session
// (setsid) rather than ours. A session leader's pgid equals its pid. This drives
// the real System() adapter through an actual spawn to prove Detach takes effect.
func TestSystemDetachMakesOwnSessionLeader(t *testing.T) {
	cmd := exec.Command("sleep", "1")
	System().Detach(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("Detach did not set Setsid: %+v", cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()

	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid(child): %v", err)
	}
	if pgid != pid {
		t.Errorf("detached child should lead its own session: pgid=%d, pid=%d", pgid, pid)
	}
	if myPgid, _ := syscall.Getpgid(os.Getpid()); pgid == myPgid {
		t.Errorf("child shares our process group (%d) — not detached from terminal", myPgid)
	}
}

// TerminateGroup on a bogus pid must surface an error rather than panic (both the
// group signal and the bare-pid fallback fail for an unused pid).
func TestSystemTerminateGroupBadPID(t *testing.T) {
	if err := System().TerminateGroup(1 << 30); err == nil {
		t.Fatal("TerminateGroup on a nonexistent pid should error")
	}
}
