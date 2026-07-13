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

// TestRunDistillLoopReviewOnlyThenArrivalTerminatesAndDrains reproduces issue #49
// C1: a review-only worker (nothing pending at start) that has L0 land DURING the
// review must (a) NOT spin forever and (b) actually drain the late arrival. Before
// the fix, the drain was gated on a stale one-time hasPending=false, so the loop
// saw the new session via hasUnattempted and looped, but never drained it — an
// unbounded spin. The fix gates the drain on a fresh pending() each pass.
func TestRunDistillLoopReviewOnlyThenArrivalTerminatesAndDrains(t *testing.T) {
	// pending() reports empty until the (single) review runs, then reports one
	// session — modeling capture landing L0 while the worker was reviewing.
	reviewed := false
	pendingCalls := 0
	pending := func() []string {
		pendingCalls++
		if reviewed {
			return []string{"late-sess"}
		}
		return nil
	}
	drained := 0
	attempted := map[string]bool{}
	drainAll := func() {
		for _, s := range pending() {
			attempted[s] = true // mark drained so the arrival isn't re-offered forever
			drained++
		}
	}
	reviewRuns := 0
	// A hard iteration cap converts the pre-fix infinite spin into a test failure
	// rather than a hang.
	iters := 0
	stopRequested := func() bool {
		iters++
		if iters > 1000 {
			t.Fatal("runDistillLoop did not terminate — review-only livelock (issue #49 C1) regressed")
		}
		return false
	}
	runDistillLoop(distillLoopDeps{
		stopRequested: stopRequested,
		pending:       pending,
		drainAll:      drainAll,
		reviewDue:     func() bool { return !reviewed }, // due exactly once
		runReview:     func() { reviewRuns++; reviewed = true },
		ensureMiner:   func() bool { return true },
		hasUnattempted: func(p []string) bool {
			for _, s := range p {
				if !attempted[s] {
					return true
				}
			}
			return false
		},
	})
	if reviewRuns != 1 {
		t.Fatalf("review should run exactly once, ran %d", reviewRuns)
	}
	if drained != 1 {
		t.Fatalf("the session that arrived during review must be drained; drained=%d", drained)
	}
	_ = pendingCalls
}

// TestRunDistillLoopStopsWhenEmbedderUnavailable: if fresh work arrives but the
// embedder can't be built (ensureMiner=false), the loop must stop rather than spin
// on work it can never attempt.
func TestRunDistillLoopStopsWhenEmbedderUnavailable(t *testing.T) {
	iters := 0
	runDistillLoop(distillLoopDeps{
		stopRequested: func() bool {
			iters++
			if iters > 1000 {
				t.Fatal("loop did not terminate when the embedder is unavailable")
			}
			return false
		},
		pending:        func() []string { return []string{"sess"} },
		drainAll:       func() {}, // no miner → drainAll is a no-op
		reviewDue:      func() bool { return false },
		runReview:      func() { t.Fatal("review should not run") },
		ensureMiner:    func() bool { return false }, // embedder unavailable
		hasUnattempted: func([]string) bool { return true },
	})
}

// TestRunDistillLoopHonorsStopRequest: a stop request breaks the loop immediately.
func TestRunDistillLoopHonorsStopRequest(t *testing.T) {
	drained := 0
	runDistillLoop(distillLoopDeps{
		stopRequested:  func() bool { return true },
		pending:        func() []string { return []string{"sess"} },
		drainAll:       func() { drained++ },
		reviewDue:      func() bool { return true },
		runReview:      func() { t.Fatal("review should not run under a stop request") },
		ensureMiner:    func() bool { return true },
		hasUnattempted: func([]string) bool { return true },
	})
	if drained != 0 {
		t.Fatalf("no drain should happen under an immediate stop; drained=%d", drained)
	}
}

func TestParseSessionTimeRangeAcceptsRelativeAgeAndDate(t *testing.T) {
	now := time.Date(2026, 7, 12, 15, 0, 0, 0, time.UTC)
	r, err := parseSessionTimeRange("7d", "2026-07-12", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-7 * 24 * time.Hour); !r.since.Equal(want) {
		t.Fatalf("since = %s, want %s", r.since, want)
	}
	wantUntil := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).Add(-time.Nanosecond)
	if !r.until.Equal(wantUntil) {
		t.Fatalf("until = %s, want %s", r.until, wantUntil)
	}
}

func TestParseSessionTimeRangeRejectsReversedRange(t *testing.T) {
	_, err := parseSessionTimeRange("2026-07-12", "2026-07-01", time.Now())
	if err == nil {
		t.Fatal("reversed range should fail")
	}
}

// autoWorkerShouldStart is the (debounce-free) gate: start iff auto is on, there is
// work (pending or a due review), no worker is already running, and — for mining —
// the model is ready. No cooldown/interval: WorkerLock single-flights and the
// running worker self-drains new arrivals (issue #22 PR2).
func TestAutoWorkerShouldStartWhenWorkAndModelReady(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !autoWorkerShouldStart(st, store.Config{AutoDistill: true}, []string{"sess"}, true) {
		t.Fatal("should start: auto on, pending work, model ready, no worker running")
	}
}

func TestAutoWorkerShouldNotStartWhenDisabledOrIdleOrNoModel(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// ReviewEvery high so an empty store doesn't spuriously report a review due
	// (SessionsSinceReview 0 >= 0 would be true), isolating the intended conditions.
	noReview := store.Config{AutoDistill: true, ReviewEvery: 99}
	if autoWorkerShouldStart(st, store.Config{AutoDistill: false, ReviewEvery: 99}, []string{"sess"}, true) {
		t.Fatal("must not start when auto_distill is off")
	}
	if autoWorkerShouldStart(st, noReview, nil, true) {
		t.Fatal("must not start with no pending work and no review due")
	}
	if autoWorkerShouldStart(st, noReview, []string{"sess"}, false) {
		t.Fatal("must not start mining when the embedding model is not ready")
	}
}

// Review-only work (poignancy threshold crossed) should start even without the
// model, because reviewing needs no embedder.
func TestAutoWorkerShouldStartForReviewWithoutModel(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AppendObservations([]store.Observation{{ID: "o1", Lens: store.LensDefault, Poignancy: 9}}); err != nil {
		t.Fatal(err)
	}
	if !autoWorkerShouldStart(st, store.Config{AutoDistill: true, ReviewEvery: 99, ReviewPoignancy: 5}, nil, false) {
		t.Fatal("review-only auto worker should start even when the model is not ready")
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
