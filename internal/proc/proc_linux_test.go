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
