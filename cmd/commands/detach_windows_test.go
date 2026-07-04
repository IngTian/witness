//go:build windows

package commands

import (
	"testing"

	"golang.org/x/sys/windows"
)

// On Windows there's no session/pgid to probe (no Getpgid), so we assert the
// detach flags directly: the worker must be created DETACHED_PROCESS (no console
// to receive a close event) and in a NEW_PROCESS_GROUP (isolated from the
// parent's Ctrl-C/Ctrl-Break) — the Windows analogue of setsid.
func TestDetachSysProcAttrFlags(t *testing.T) {
	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if got := detachSysProcAttr().CreationFlags; got != want {
		t.Errorf("detach CreationFlags = %#x, want %#x", got, want)
	}
}
