package main

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/claude-witness/internal/store"
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

	ran, err := runWorker()
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatalf("runWorker should report skipped when the worker lock is held")
	}
}

func TestWorkerSessionBudgetIsUnbounded(t *testing.T) {
	for _, runner := range []string{"opencode", "claude"} {
		if got := workerSessionBudget(store.Config{Runner: runner}); got != 0 {
			t.Fatalf("%s budget = %d, want unbounded", runner, got)
		}
	}
}
