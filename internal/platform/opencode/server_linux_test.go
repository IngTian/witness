//go:build linux

package opencode

import (
	"context"
	"syscall"
	"testing"
)

// TestServeSysProcAttrSetsPdeathsig locks the Linux half of the #54 I2 fix: the
// serve child must carry Pdeathsig=SIGKILL so the kernel reaps it when the worker
// dies (SIGKILL/OOM included), and buildOpenCodeServeCmd must wire it onto the cmd.
func TestServeSysProcAttrSetsPdeathsig(t *testing.T) {
	if got := serveSysProcAttr(); got == nil || got.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("serveSysProcAttr() = %+v, want Pdeathsig=SIGKILL", got)
	}
	cmd := buildOpenCodeServeCmd(context.Background(), 12345, "secret")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("buildOpenCodeServeCmd did not wire Pdeathsig: %+v", cmd.SysProcAttr)
	}
}
