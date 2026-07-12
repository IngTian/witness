package opencode

import (
	"os/exec"
	"testing"
	"time"
)

// The `opencode serve` process can exit before it ever becomes healthy (bad
// config, port clash, immediate crash). waitHealthy observes that exit via
// waitDone and returns an error; StartOpenCodeServer then calls Close() to reap
// the process. Close() MUST return promptly in that case.
//
// This is a regression test for a deadlock: waitDone was previously a size-1
// buffered channel written to exactly once. Once waitHealthy drained that single
// value, Close()'s post-signal `<-waitCh` had nothing left to receive and blocked
// forever, wedging the shared distillation worker (which holds WorkerLock and
// calls Close via defer). Closing a channel instead makes the completion signal
// observable by any number of receivers.
func TestOpenCodeServerCloseReturnsAfterWaitObserved(t *testing.T) {
	// `true` exits immediately — stands in for a serve process that dies before
	// the health check ever succeeds.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	srv := &OpenCodeServer{
		cmd:      cmd,
		waitDone: make(chan struct{}),
	}
	go func() {
		srv.waitErr = srv.cmd.Wait()
		close(srv.waitDone)
	}()

	// Stand in for waitHealthy's early-exit branch: observe that the process has
	// already exited (drains nothing now — the channel is closed, not buffered).
	select {
	case <-srv.waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("setup: process should have exited and closed waitDone")
	}

	// Now Close() runs on the failure path. It must return promptly; the bug was
	// an unbounded second receive on an already-consumed channel.
	done := make(chan struct{})
	go func() { _ = srv.Close(); close(done) }()
	select {
	case <-done:
		// returned — no deadlock.
	case <-time.After(8 * time.Second):
		t.Fatal("Close() never returned — waitDone completion signal was consumed once and lost (deadlock regression)")
	}
}

// A normal Close() (process still running when Close is called) must SIGTERM it
// and return the wait result without blocking.
func TestOpenCodeServerCloseTerminatesRunningProcess(t *testing.T) {
	// `sleep 30` stays alive until Close signals it.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	srv := &OpenCodeServer{
		cmd:      cmd,
		waitDone: make(chan struct{}),
	}
	go func() {
		srv.waitErr = srv.cmd.Wait()
		close(srv.waitDone)
	}()

	done := make(chan struct{})
	go func() { _ = srv.Close(); close(done) }()
	select {
	case <-done:
		// SIGTERM path returned promptly (well under the 5s Kill fallback).
	case <-time.After(6 * time.Second):
		t.Fatal("Close() did not terminate a running process in time")
	}
	// Idempotent: a second Close must not block or panic.
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}
