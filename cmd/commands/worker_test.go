package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

var registerSelfHealOnce sync.Once

// The status sub-state is DERIVED, not stored (issue #75): no worker holding the lock
// → "idle"; a live worker (lock held) → "running", or "stopping" if a graceful stop was
// requested. This drives cmdDistillStatus's JSON path across all three, holding
// WorkerLock in-test to stand in for a live worker. Guards the worker_status removal:
// the derivation must reproduce exactly what the stored key used to say.
func TestDistillStatusDerivesWorkerState(t *testing.T) {
	readStatus := func(t *testing.T, st *store.Store) string {
		t.Helper()
		out := captureStdout(t, func() {
			if err := cmdDistillStatus(true); err != nil {
				t.Fatalf("cmdDistillStatus: %v", err)
			}
		})
		var got distillStatusJSON
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("status JSON: %v\n%s", err, out)
		}
		return got.Worker.Status
	}

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// No lock held → idle, regardless of any leftover meta.
	_ = st.SetMetaString("worker_stop_requested", "1") // must be ignored while idle
	if got := readStatus(t, st); got != "idle" {
		t.Fatalf("no live worker must read idle, got %q", got)
	}

	// A live worker (lock held) with no stop pending → running.
	_ = st.SetMetaString("worker_stop_requested", "")
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take worker lock")
	}
	defer unlock()
	if got := readStatus(t, st); got != "running" {
		t.Fatalf("live worker, no stop pending → running, got %q", got)
	}

	// Same live worker, stop requested → stopping (derived from the flag, not a stored key).
	_ = st.SetMetaString("worker_stop_requested", "1")
	if got := readStatus(t, st); got != "stopping" {
		t.Fatalf("live worker + stop requested → stopping, got %q", got)
	}
}

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

// A worker already draining must block a second auto-spawn — the gate now consults
// the flock (st.WorkerActive) as the sole liveness authority (issue #75), replacing
// the old worker_status+pid+`ps` predicate. Holding WorkerLock makes WorkerActive
// true from any probe against the same root, so the gate returns false even though
// there is pending work and a ready model.
func TestAutoWorkerShouldNotStartWhenWorkerLockHeld(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take worker lock")
	}
	defer unlock()
	if autoWorkerShouldStart(st, store.Config{AutoDistill: true}, []string{"sess"}, true) {
		t.Fatal("must not spawn a second worker while one holds the lock (flock is the liveness authority)")
	}
}

// The crash-recovery property (issue #75): stale worker_status="running" + a dead
// pid left by a SIGKILL/OOM'd worker must NOT block a fresh spawn — with no live
// flock holder the gate sees "not active" and starts. The old predicate needed a
// `ps` probe to reach this conclusion; the flock reaches it for free.
func TestAutoWorkerStartsDespiteStaleRunningMeta(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetMetaString("worker_pid", "999999") // crash residue: a dead worker's stale pid
	if !autoWorkerShouldStart(st, store.Config{AutoDistill: true}, []string{"sess"}, true) {
		t.Fatal("stale worker meta with no live worker must not block a spawn")
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
	spawn := func(args ...string) bool {
		spawnCount++
		spawned = append([]string(nil), args...)
		return true
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

// cmdDistillStop with no live worker (flock free) must report "not running" cleanly
// and NOT error, even when stale worker_status="running" meta is present (issue #75:
// flock is the liveness authority, not the meta). Guards the removed self-heal block
// B — a crashed worker's residue must not make `distill stop` fail.
func TestDistillStopWhenNoWorkerRunning(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetMetaString("worker_pid", "999999") // crash residue: a dead worker's stale pid
	st.Close()

	if err := cmdDistillStop(false); err != nil {
		t.Fatalf("distill stop with no live worker must not error, got %v", err)
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
	// The cancelled auto worker must bail at the early return (worker.go) BEFORE stamping
	// its pid — so worker_pid stays empty. (There's no worker_status to check anymore: the
	// running/stopping sub-state is derived from the flock, which this exited worker has
	// released.) An empty pid proves it never entered the drain path.
	if got := st.MetaString("worker_pid"); got != "" {
		t.Fatalf("cancelled auto worker stamped a pid (entered running state): %q", got)
	}
}

func TestAutoWorkerRunHonorsDisabledConfig(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.ConfigPath(), []byte("auto_distill = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.Close()

	ran, err := runWorker(true)
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("disabled automatic worker should exit before claiming work")
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.MetaString("worker_pid"); got != "" {
		t.Fatalf("disabled automatic worker stamped pid %q", got)
	}
}

// TestWorkerSelfHealSkipsRunnersWhenFresh verifies that the end-of-run source-resolution
// self-heal opens runners ONLY when there is stale work (StaleSources returns a non-empty
// list). An idle worker run with every portrait current must never open a runner.
func TestWorkerSelfHealSkipsRunnersWhenFresh(t *testing.T) {
	// Register the fake platform once per process.
	registerSelfHealOnce.Do(func() {
		platform.Register(selfHealFakePlatform{})
	})

	// Set up resolve prompts in a fake bundle dir so LoadResolvePrompts succeeds.
	// Without this, the self-heal block is never entered (prompts fail to load) and
	// the test passes vacuously.
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "summarize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "summarize", "resolve.md"), []byte("RESOLVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "summarize", "resolve_unified.md"), []byte("RESOLVE_UNIFIED"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_PROMPTS", bundleDir)

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Point config at the fake runner and set up a minimal test lens.
	if err := st.SetConfigString("runner", "selfheal"); err != nil {
		t.Fatal(err)
	}
	lensDir := setupTestLens(t, st.Root)
	if err := st.RegisterLens("test", lensDir); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("test"); err != nil {
		t.Fatal(err)
	}

	// Write a facet and profile, then write portrait sources so StaleSources returns empty.
	if err := st.WriteFacets([]store.Facet{
		{Lens: "test", Dimension: "traits", Key: "k",
			Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
	}); err != nil {
		t.Fatal(err)
	}
	md := "Summary."
	if err := st.WriteProfile("test", md); err != nil {
		t.Fatal(err)
	}
	// Write current sources (hash matches, so it's not stale).
	src := store.PortraitSources{Sentences: []store.SourcedSentence{{Text: "Summary."}}}
	if err := st.WritePortraitSources("test", store.PortraitHash(md), src, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reset counters and run the worker. With sources fresh, it must NOT open the runner.
	atomic.StoreInt32(&selfHealFakeOpens, 0)
	atomic.StoreInt32(&selfHealFakeRuns, 0)

	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("runWorker: %v", err)
	}
	if !ran {
		t.Fatal("worker should have run")
	}

	// Assert LoadResolvePrompts succeeded (non-empty prompts) so we know the self-heal
	// block was entered. Without this, the test can pass vacuously if prompts fail to load.
	lensPrompt, unifiedPrompt, err := lens.LoadResolvePrompts()
	if err != nil {
		t.Fatalf("LoadResolvePrompts failed; test cannot verify the gate: %v", err)
	}
	if lensPrompt == "" || unifiedPrompt == "" {
		t.Fatal("LoadResolvePrompts returned empty prompts; test cannot verify the gate")
	}

	if got := atomic.LoadInt32(&selfHealFakeOpens); got != 0 {
		t.Errorf("self-heal opened runner %d times with fresh sources; want 0", got)
	}
	if got := atomic.LoadInt32(&selfHealFakeRuns); got != 0 {
		t.Errorf("self-heal ran %d times with fresh sources; want 0", got)
	}
}

// TestWorkerSelfHealResolvesStalePortraits verifies that the end-of-run self-heal DOES
// open the runner and call ResolveSources when there are stale portraits (portrait exists
// but no sources row, or hash mismatch).
func TestWorkerSelfHealResolvesStalePortraits(t *testing.T) {
	registerSelfHealOnce.Do(func() { platform.Register(selfHealFakePlatform{}) })

	// Set up resolve prompts in a fake bundle dir.
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "summarize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "summarize", "resolve.md"), []byte("RESOLVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "summarize", "resolve_unified.md"), []byte("RESOLVE_UNIFIED"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_PROMPTS", bundleDir)

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetConfigString("runner", "selfheal"); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterLens("test", setupTestLens(t, st.Root)); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("test"); err != nil {
		t.Fatal(err)
	}

	// Write a facet and profile, but NO sources row → stale.
	if err := st.WriteFacets([]store.Facet{
		{Lens: "test", Dimension: "traits", Key: "k",
			Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteProfile("test", "Stale summary."); err != nil {
		t.Fatal(err)
	}
	st.Close()

	atomic.StoreInt32(&selfHealFakeOpens, 0)
	atomic.StoreInt32(&selfHealFakeRuns, 0)

	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("runWorker: %v", err)
	}
	if !ran {
		t.Fatal("worker should have run")
	}

	if got := atomic.LoadInt32(&selfHealFakeOpens); got != 1 {
		t.Errorf("self-heal opened runner %d times with stale portrait; want exactly 1", got)
	}
	if got := atomic.LoadInt32(&selfHealFakeRuns); got < 1 {
		t.Errorf("self-heal ran %d times with stale portrait; want at least 1 (the resolve call)", got)
	}
}

// selfHealFakePlatform is a minimal registered platform for testing the self-heal wiring.
var selfHealFakeOpens, selfHealFakeRuns int32

type selfHealFakePlatform struct{}

func (selfHealFakePlatform) Name() string          { return "selfheal" }
func (selfHealFakePlatform) SessionPrefix() string { return "selfheal:" }
func (selfHealFakePlatform) RenderInputs(r []store.RawRecord, _ platform.ChunkPolicy) []string {
	return []string{""}
}
func (selfHealFakePlatform) Capture(*store.Store, []byte, time.Time) (bool, error) { return false, nil }
func (selfHealFakePlatform) Import(context.Context, store.ImportStore, []string) (platform.ImportStats, error) {
	return platform.ImportStats{}, nil
}
func (selfHealFakePlatform) NewRunner(store.Config) platform.Runner { return selfHealFakeRunner{} }

type selfHealFakeRunner struct{}

func (selfHealFakeRunner) Open(context.Context) error {
	atomic.AddInt32(&selfHealFakeOpens, 1)
	return nil
}
func (selfHealFakeRunner) Close() error { return nil }
func (selfHealFakeRunner) Run(_ context.Context, _, prompt, _ string) (string, error) {
	atomic.AddInt32(&selfHealFakeRuns, 1)
	if strings.Contains(prompt, "RESOLVE") {
		return `{"links":[],"keywords":[]}`, nil
	}
	return "[]", nil
}
func (selfHealFakeRunner) ValidateModels(context.Context, ...string) error { return nil }
func (selfHealFakeRunner) InvocationHint() string                          { return "selfheal" }
func (selfHealFakeRunner) ConcurrentRunSafe() bool                         { return true }

// TestWorkerSelfHealRetriesAreBounded verifies that a portrait whose resolve keeps failing
// gets bounded retries: the first self-heal attempts once and sets sources_retry_after =
// now+6h; subsequent self-heal runs within that window make zero resolver calls; after the
// window expires (or a review runs, which ignores the gate) it retries.
func TestWorkerSelfHealRetriesAreBounded(t *testing.T) {
	registerSelfHealOnce.Do(func() { platform.Register(selfHealFakePlatform{}) })
	platform.Register(selfHealFailPlatform{})

	// Set up resolve prompts.
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "summarize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "summarize", "resolve.md"), []byte("RESOLVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "summarize", "resolve_unified.md"), []byte("RESOLVE_UNIFIED"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_PROMPTS", bundleDir)

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetConfigString("runner", "selfhealfail"); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterLens("test", setupTestLens(t, st.Root)); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("test"); err != nil {
		t.Fatal(err)
	}

	// Write a profile with no sources row → stale.
	if err := st.WriteFacets([]store.Facet{
		{Lens: "test", Dimension: "traits", Key: "k",
			Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteProfile("test", "Stale summary."); err != nil {
		t.Fatal(err)
	}
	st.Close()

	atomic.StoreInt32(&selfHealFailRuns, 0)

	// First worker run: resolver fails → sets retry_after.
	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("first runWorker: %v", err)
	}
	if !ran {
		t.Fatal("first worker should have run")
	}
	if got := atomic.LoadInt32(&selfHealFailRuns); got != 1 {
		t.Errorf("first run: resolver called %d times, want 1", got)
	}

	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	retryAfter := st.MetaString("sources_retry_after")
	st.Close()
	if retryAfter == "" {
		t.Fatal("first run should have set sources_retry_after")
	}

	atomic.StoreInt32(&selfHealFailRuns, 0)

	// Second worker run within window: makes zero resolver calls.
	ran, err = runWorker(false)
	if err != nil {
		t.Fatalf("second runWorker: %v", err)
	}
	if !ran {
		t.Fatal("second worker should have run")
	}
	if got := atomic.LoadInt32(&selfHealFailRuns); got != 0 {
		t.Errorf("second run within window: resolver called %d times, want 0", got)
	}

	// Set retry_after in the past.
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	if err := st.SetMetaString("sources_retry_after", past); err != nil {
		t.Fatal(err)
	}
	st.Close()

	atomic.StoreInt32(&selfHealFailRuns, 0)

	// Third run after window: retries.
	ran, err = runWorker(false)
	if err != nil {
		t.Fatalf("third runWorker: %v", err)
	}
	if !ran {
		t.Fatal("third worker should have run")
	}
	if got := atomic.LoadInt32(&selfHealFailRuns); got != 1 {
		t.Errorf("third run after window: resolver called %d times, want 1", got)
	}

	// Verify removing the gate causes the test to fail (would make infinite calls if loop existed).
	// We can't test this here, but the existence of the gate is verified by the zero-calls check above.
}

// selfHealFailPlatform is a runner that always fails, for testing retry bounds.
var selfHealFailRuns int32

type selfHealFailPlatform struct{}

func (selfHealFailPlatform) Name() string          { return "selfhealfail" }
func (selfHealFailPlatform) SessionPrefix() string { return "selfhealfail:" }
func (selfHealFailPlatform) RenderInputs(r []store.RawRecord, _ platform.ChunkPolicy) []string {
	return []string{""}
}
func (selfHealFailPlatform) Capture(*store.Store, []byte, time.Time) (bool, error) { return false, nil }
func (selfHealFailPlatform) Import(context.Context, store.ImportStore, []string) (platform.ImportStats, error) {
	return platform.ImportStats{}, nil
}
func (selfHealFailPlatform) NewRunner(store.Config) platform.Runner { return selfHealFailRunner{} }

type selfHealFailRunner struct{}

func (selfHealFailRunner) Open(context.Context) error { return nil }
func (selfHealFailRunner) Close() error               { return nil }
func (selfHealFailRunner) Run(_ context.Context, _, prompt, _ string) (string, error) {
	atomic.AddInt32(&selfHealFailRuns, 1)
	return "", errors.New("resolver always fails for test")
}
func (selfHealFailRunner) ValidateModels(context.Context, ...string) error { return nil }
func (selfHealFailRunner) InvocationHint() string                          { return "selfhealfail" }
func (selfHealFailRunner) ConcurrentRunSafe() bool                         { return true }

// setupTestLens creates a minimal lens directory for testing.
func setupTestLens(t *testing.T, root string) string {
	t.Helper()
	lensDir := filepath.Join(t.TempDir(), "testlens")
	if err := os.MkdirAll(lensDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write minimal lens.json
	lensJSON := `{"name":"test","dimensions":["traits"]}`
	if err := os.WriteFile(filepath.Join(lensDir, "lens.json"), []byte(lensJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write minimal extract.md
	if err := os.WriteFile(filepath.Join(lensDir, "extract.md"), []byte("Extract prompt."), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write minimal review.md
	if err := os.WriteFile(filepath.Join(lensDir, "review.md"), []byte("Review prompt."), 0o600); err != nil {
		t.Fatal(err)
	}
	return lensDir
}

// TestSelfHealAllowed unit-tests the selfHealAllowed gate that guards the end-of-run
// self-heal. This is the load-bearing stop-request check that prevents fresh model calls
// after `witness distill stop`. Cannot be tested via runWorker because auto mode exits
// before reaching the gate and manual mode clears the flag.
func TestSelfHealAllowed(t *testing.T) {
	tests := []struct {
		name          string
		reviewRan     bool
		stopRequested func() bool
		ctxErr        error
		want          bool
	}{
		{
			name:          "allowed when none of the guards trigger",
			reviewRan:     false,
			stopRequested: func() bool { return false },
			ctxErr:        nil,
			want:          true,
		},
		{
			name:          "blocked when review already ran",
			reviewRan:     true,
			stopRequested: func() bool { return false },
			ctxErr:        nil,
			want:          false,
		},
		{
			name:          "blocked when stop requested",
			reviewRan:     false,
			stopRequested: func() bool { return true },
			ctxErr:        nil,
			want:          false,
		},
		{
			name:          "blocked when context cancelled",
			reviewRan:     false,
			stopRequested: func() bool { return false },
			ctxErr:        context.Canceled,
			want:          false,
		},
		{
			name:          "blocked when all guards trigger",
			reviewRan:     true,
			stopRequested: func() bool { return true },
			ctxErr:        context.Canceled,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ctxErr != nil {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel() // cancel immediately
			}
			got := selfHealAllowed(tt.reviewRan, tt.stopRequested, ctx)
			if got != tt.want {
				t.Errorf("selfHealAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}
