//go:build windows

package proc

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// On Windows there's no session/pgid to probe (no Getpgid), so we assert the detach
// flags directly: the worker must be created DETACHED_PROCESS (no console to receive
// a close event) and in a NEW_PROCESS_GROUP (isolated from the parent's
// Ctrl-C/Ctrl-Break) — the Windows analogue of setsid.
func TestSystemDetachFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit")
	System().Detach(cmd)
	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags != want {
		t.Errorf("Detach CreationFlags = %#x, want %#x", cmd.SysProcAttr.CreationFlags, want)
	}
}
