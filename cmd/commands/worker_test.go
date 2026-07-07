package commands

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

func TestRunWorkerReportsSkippedWhenLockHeld(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take worker lock for test")
	}
	defer unlock()

	ran, err := runWorker(false)
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatalf("runWorker should report skipped when the worker lock is held")
	}
}

func TestWorkerSessionBudgetIsUnboundedWhenManual(t *testing.T) {
	for _, runner := range []string{"opencode", "claude"} {
		if got := workerSessionBudget(store.Config{Runner: runner, AutoDistillSessionBudget: 1}, false); got != 0 {
			t.Fatalf("%s budget = %d, want unbounded", runner, got)
		}
	}
}

func TestWorkerSessionBudgetUsesAutoLimit(t *testing.T) {
	if got := workerSessionBudget(store.Config{AutoDistillSessionBudget: 1}, true); got != 1 {
		t.Fatalf("auto budget = %d, want 1", got)
	}
}
