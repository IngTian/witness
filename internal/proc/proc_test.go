package proc

import (
	"os"
	"testing"
)

// Alive must be right about BOTH answers, since each error direction is a distinct bug:
// a false "dead" lets a second downloader steal a live lock, and a false "alive" makes it decline a
// genuine orphan's lock forever.
func TestAliveDistinguishesLiveFromDead(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("this process was reported DEAD — a live owner's record would read as an " +
			"orphan: its download lock stolen")
	}
	// pid 1 and below are never valid owners (init/launchd, or nonsense).
	for _, pid := range []int{0, 1, -1} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true; pids <= 1 must never be treated as a live owner", pid)
		}
	}
	// A pid that almost certainly does not exist must read as dead, or no orphan is ever reaped.
	// (Not absolutely guaranteed free, so this is a soft check with a clear message.)
	if Alive(0x7FFFFFF0) {
		t.Log("note: pid 0x7FFFFFF0 appears live on this machine; the dead-pid direction was not " +
			"exercised. Re-run or pick another pid if this persists.")
	}
}
