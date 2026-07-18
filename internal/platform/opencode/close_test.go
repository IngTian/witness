package opencode

import (
	"os/exec"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/proc"
)

// TestCloseGracefulStopsThroughPort proves OpenCodeServer.Close routes its graceful
// termination through the proc.Control port (issue #73-C1 / #43) rather than reaching
// into syscall directly — the invariant that keeps the engine OS-agnostic. It swaps in
// a proc.Fake and asserts Close hands the serve child's process to GracefulStop, so a
// Windows-specific termination nuance could be fixed in ONE adapter without touching
// this file.
func TestCloseGracefulStopsThroughPort(t *testing.T) {
	// A real, quick child so cmd.Process is non-nil; the Fake never actually signals it,
	// so it just exits on its own. Use a long-ish sleep and let Close's fake path win.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	// Ensure the child is reaped regardless of what the Fake does (it won't signal).
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	fake := &proc.Fake{}
	orig := procCtl
	procCtl = fake
	t.Cleanup(func() { procCtl = orig })

	srv := &OpenCodeServer{cmd: cmd, waitDone: waitDone}
	// Close will call GracefulStop (the Fake no-ops), then wait up to 5s for waitDone;
	// the Fake didn't signal, so escalate via Kill in the cleanup — but to keep the test
	// fast we kill first so Close's <-waitDone returns promptly.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = cmd.Process.Kill()
	}()
	if err := srv.Close(); err != nil {
		// Kill produces a non-nil wait error; that's expected and not what we assert.
		t.Logf("Close returned err (expected from Kill): %v", err)
	}

	if len(fake.GracefulStops) != 1 {
		t.Fatalf("Close should call GracefulStop exactly once, got %d calls", len(fake.GracefulStops))
	}
	if fake.GracefulStops[0] != cmd.Process {
		t.Fatalf("GracefulStop got the wrong process: %v (want the serve child %v)", fake.GracefulStops[0], cmd.Process)
	}
}
