package commands

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/proc"
	"github.com/IngTian/witness/internal/store"
)

// The worker contains its children (on Windows, a kill-on-close job, so `worker stop`'s
// TerminateProcess takes them along) only once the lock is its own: a trigger that loses the lock
// exits at once and must not touch a job, and a refused job is best-effort — the drain still runs.
func TestWorkerContainsChildrenOnlyAfterTakingTheLock(t *testing.T) {
	prev := procCtl
	t.Cleanup(func() { procCtl = prev })
	fake := &proc.Fake{}
	procCtl = fake
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take the worker lock")
	}
	if ran, _ := runWorker(false); ran || fake.Contained != 0 {
		t.Fatalf("a worker that lost the lock contained itself (ran=%v, Contained=%d)", ran, fake.Contained)
	}
	unlock()
	_ = st.Close()
	fake.ContainErr = errors.New("job refused")
	if ran, err := runWorker(false); err != nil || !ran || fake.Contained != 1 {
		t.Fatalf("ran=%v err=%v Contained=%d, want true <nil> 1", ran, err, fake.Contained)
	}
}
