//go:build linux

package proc

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestSystemBindToParentSetsPdeathsig locks the Linux half of the #54 I2 fix: the
// child must carry Pdeathsig=SIGKILL so the kernel reaps it when the parent dies
// (SIGKILL/OOM included).
func TestSystemBindToParentSetsPdeathsig(t *testing.T) {
	cmd := exec.Command("true")
	System().BindToParent(cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("BindToParent did not set Pdeathsig=SIGKILL: %+v", cmd.SysProcAttr)
	}
}

// ReapOrphans is a no-op on Linux (Pdeathsig already reaps). It must not panic and
// must not invoke the predicate — there is nothing to scan.
func TestSystemReapOrphansNoopOnLinux(t *testing.T) {
	called := false
	System().ReapOrphans(func(string) bool { called = true; return true })
	if called {
		t.Fatal("ReapOrphans on Linux must not scan or invoke the predicate")
	}
}
