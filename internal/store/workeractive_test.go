package store

import "testing"

// WorkerActive is the sole liveness authority (issue #75): free lock → not active,
// held lock → active. flock denies a second acquisition even from the SAME process
// (a distinct open file description), which is what lets a spawn/stop probe in one
// process see a worker holding the lock in another — the property this asserts via
// two lockFile opens against one store root.
func TestWorkerActiveTracksLock(t *testing.T) {
	s := tempStore(t)

	if s.WorkerActive() {
		t.Fatal("no worker holds the lock; WorkerActive must be false")
	}

	unlock, ok := s.WorkerLock()
	if !ok {
		t.Fatal("could not take worker lock")
	}
	if !s.WorkerActive() {
		t.Fatal("worker lock is held; WorkerActive must be true")
	}

	unlock()
	if s.WorkerActive() {
		t.Fatal("lock released; WorkerActive must be false again")
	}
}

// WorkerActive must ignore stale worker_* meta entirely — the whole reason to make
// the flock the sole authority (issue #75 / #73-C2) is that a worker killed by
// SIGKILL/OOM/power-loss leaves a dead pid (and, before the meta collapse, a
// "running" status) behind, and the OS-released flock is the only trustworthy "is it
// alive" signal. The old meta+`ps`-probe self-heal existed only to paper over exactly
// this; here we prove the replacement needs no probe: crash residue reads as not-active.
func TestWorkerActiveIgnoresStaleRunningMeta(t *testing.T) {
	s := tempStore(t)
	_ = s.SetMetaString("worker_pid", "999999") // a pid no live worker owns (crash residue)
	if s.WorkerActive() {
		t.Fatal("stale worker meta with no held lock must NOT read as active (flock is the authority)")
	}
}

// WorkerActive is repeatable and non-destructive: probing does not leave the lock
// held (it acquires-then-releases), so a real worker can still take it afterward.
func TestWorkerActiveIsNonDestructive(t *testing.T) {
	s := tempStore(t)
	for i := 0; i < 3; i++ {
		if s.WorkerActive() {
			t.Fatalf("probe %d: must be false when free", i)
		}
	}
	unlock, ok := s.WorkerLock()
	if !ok {
		t.Fatal("WorkerActive probing must not leave the lock held; a real worker could not acquire it")
	}
	unlock()
}
