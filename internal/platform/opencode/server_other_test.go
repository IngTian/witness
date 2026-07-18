//go:build !linux

package opencode

import (
	"context"
	"testing"
)

// TestServeSysProcAttrNilOffLinux documents that macOS/Windows have no Pdeathsig
// equivalent, so the serve child cannot be lifetime-bound at fork; cleanup of a
// hard-killed worker's serve relies on the startup reap (reapStrayServes) instead.
// A nil SysProcAttr must not break the launch.
func TestServeSysProcAttrNilOffLinux(t *testing.T) {
	if got := serveSysProcAttr(); got != nil {
		t.Fatalf("serveSysProcAttr() = %+v, want nil off Linux", got)
	}
	cmd := buildOpenCodeServeCmd(context.Background(), 12345, "secret")
	if cmd.SysProcAttr != nil {
		t.Fatalf("buildOpenCodeServeCmd wired non-nil SysProcAttr off Linux: %+v", cmd.SysProcAttr)
	}
}

// TestReapStrayServesIsBestEffort ensures the startup reap never panics or blocks
// even when `ps` output is degenerate; the selection logic itself is covered by
// TestStrayServePIDs. This just exercises the real (ps-invoking) path for safety.
func TestReapStrayServesIsBestEffort(t *testing.T) {
	reapStrayServes() // must not panic; on a box with no matching orphan it's a no-op
}
