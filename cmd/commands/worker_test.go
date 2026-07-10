package commands

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestWorkerBudgetReachedOnlyWhenWorkRemains(t *testing.T) {
	if !workerBudgetReached(true, 1, 1, 2) {
		t.Fatal("budget with remaining pending work should pause and reschedule")
	}
	if workerBudgetReached(true, 1, 1, 0) {
		t.Fatal("budget should not skip review when the queue is empty")
	}
}

func TestAutoWorkerStartActionSchedulesCooldownWakeup(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	_ = st.SetMetaString(workerAutoStartedAtKey, now.Add(-2*time.Minute).Format(time.RFC3339))
	action := autoWorkerStartAction(st, store.Config{AutoDistill: true, AutoDistillIntervalMinutes: 10}, []string{"sess"}, true, now)
	if action.start {
		t.Fatal("cooldown action should schedule wakeup, not start immediately")
	}
	if got, want := action.wakeAt, now.Add(8*time.Minute); !got.Equal(want) {
		t.Fatalf("wakeAt = %s, want %s", got, want)
	}
}

func TestAutoWorkerStartActionAllowsReviewWithoutModel(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AppendObservations([]store.Observation{{ID: "o1", Lens: store.LensDefault, Poignancy: 9}}); err != nil {
		t.Fatal(err)
	}
	action := autoWorkerStartAction(st, store.Config{AutoDistill: true, ReviewEvery: 99, ReviewPoignancy: 5}, nil, false, time.Now())
	if !action.start {
		t.Fatal("review-only auto worker should start even when model is not ready")
	}
	if !action.wakeAt.IsZero() {
		t.Fatalf("unexpected wakeAt: %s", action.wakeAt)
	}
}

func TestScheduleWorkerWakeupPreservesMode(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	next := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	var spawned []string
	spawnCount := 0
	spawn := func(args ...string) {
		spawnCount++
		spawned = append([]string(nil), args...)
	}
	scheduleWorkerWakeupWith(st, next, "manual", spawn)
	scheduleWorkerWakeupWith(st, next.Add(time.Minute), "manual", spawn)
	if got := st.MetaString(workerWakeupKey("manual")); got != next.Format(time.RFC3339) {
		t.Fatalf("manual wakeup = %q, want %q", got, next.Format(time.RFC3339))
	}
	if len(spawned) != 4 || spawned[0] != "worker-wakeup" || spawned[2] != next.Format(time.RFC3339) || spawned[3] != "manual" {
		t.Fatalf("spawn args = %v", spawned)
	}
	if spawnCount != 1 {
		t.Fatalf("later wakeup spawned %d processes, want 1", spawnCount)
	}
	if got := workerWakeMode(st); got != "manual" {
		t.Fatalf("workerWakeMode = %q, want manual", got)
	}
}

func TestClearScheduledWakeupRespectsMode(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetMetaString(workerWakeupKey("auto"), time.Now().UTC().Format(time.RFC3339))
	if clearScheduledWakeup(st, "manual") {
		t.Fatal("manual clear should not remove auto wakeup")
	}
	if !clearScheduledWakeup(st, "auto") {
		t.Fatal("auto clear should remove auto wakeup")
	}
	if got := st.MetaString(workerWakeupKey("auto")); got != "" {
		t.Fatalf("auto wakeup not cleared: %q", got)
	}
}

func TestManualWakeupRunsWhenAutoDistillIsDisabled(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.ConfigPath(), []byte("auto_distill = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	_ = st.SetMetaString(workerWakeupKey("manual"), stamp)
	st.Close()

	if err := cmdWorkerWakeup([]string{"0", stamp, "manual"}); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.MetaString(workerWakeupKey("manual")); got != "" {
		t.Fatalf("manual wakeup was not consumed: %q", got)
	}
}

func TestDistillStopAutoOnlyCancelsPendingAutoWakeupWithoutStoppingManual(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetMetaString("worker_mode", "manual")
	_ = st.SetMetaString(workerWakeupKey("auto"), time.Now().UTC().Format(time.RFC3339))
	if err := cmdDistillStop(true); err != nil {
		t.Fatal(err)
	}
	if got := st.MetaString(workerWakeupKey("auto")); got != "" {
		t.Fatalf("pending auto wakeup not cleared: %q", got)
	}
	if got := st.MetaString("worker_stop_requested"); got != "" {
		t.Fatalf("manual worker should not receive stop request, got %q", got)
	}
}

func TestDistillStopAutoOnlyCancelsWorkerBeforeItStarts(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetMetaString("worker_mode", "auto-pending")
	st.Close()

	if err := cmdDistillStop(true); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.MetaString("worker_stop_requested"); got != "1" {
		t.Fatalf("pending auto worker stop request = %q, want 1", got)
	}
}

func TestAutoWorkerHonorsStopBeforeStarting(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetMetaString("worker_stop_requested", "1")
	st.Close()

	ran, err := runWorker(true)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("auto worker should claim the lock before honoring the stop request")
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.MetaString("worker_status"); got == "running" {
		t.Fatal("cancelled auto worker entered running state")
	}
}
