//go:build !linux && !windows

package proc

import (
	"os/exec"
	"testing"
)

// BindToParent is a no-op on macOS: there is no Pdeathsig equivalent, so no
// SysProcAttr should be forced onto the cmd.
func TestSystemBindToParentNoopOnDarwin(t *testing.T) {
	cmd := exec.Command("true")
	System().BindToParent(cmd)
	if cmd.SysProcAttr != nil {
		t.Fatalf("BindToParent wired non-nil SysProcAttr off Linux: %+v", cmd.SysProcAttr)
	}
}

// TestSystemReapOrphansIsBestEffort ensures the real (ps-invoking) reap never panics
// or blocks on a box with no matching orphan; the selection logic itself is covered
// by TestOrphanPIDs. The predicate matches nothing, so nothing is ever killed.
func TestSystemReapOrphansIsBestEffort(t *testing.T) {
	System().ReapOrphans(func(string) bool { return false })
}
