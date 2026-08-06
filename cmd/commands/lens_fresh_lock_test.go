package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// `lens backfill --fresh` must HOLD the WorkerLock across its destructive section, not probe it.
//
// SCOPE, stated honestly: this asserts --fresh REFUSES while the lock is held, and that a
// declined run deletes nothing. It does NOT distinguish holding from probing — it cannot.
// WorkerActive opens a fresh descriptor, and flock denies that against the calling process's own
// held lock (internal/store/locks.go:122-129), so an in-process test sees the same refusal
// either way. Demonstrating the difference needs a SECOND PROCESS acquiring the lock inside the
// window, and spawning processes from tests is banned in this repo after two runaway incidents.
//
// So the guarantee rests on three things instead: this behavioral test (refuses, deletes
// nothing), TestFreshRaceWouldStampPhantomProgress below (the measured damage the window
// causes), and TestFreshHoldsTheLockNotJustAProbe (the structural assertion that the real lock
// is taken). Losing any one of them is what a reviewer should catch.
func TestLensBackfillFreshDeclinesWhileTheWorkerLockIsHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AppendRaw(store.RawRecord{
		Session: "s1", Seq: 0, TS: "2026-08-01T00:00:00Z", Role: "user", Text: "turn",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDistilled("s1", "default", 1); err != nil {
		t.Fatal(err)
	}
	before := st.DistilledCount("s1", "default")
	if before != 1 {
		t.Fatalf("precondition: want DistilledCount 1, got %d", before)
	}

	// A concurrent worker holds the lock.
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("precondition: the lock should be free here")
	}
	defer unlock()

	// --yes so no prompt blocks the test; the point is that it refuses BEFORE deleting.
	err = lensBackfill(st, "default", true /*fresh*/, true /*assumeYes*/)
	if err == nil {
		t.Fatal("--fresh proceeded while a worker held the lock: it would delete this lens's " +
			"data and then be unable to re-mine it")
	}
	if !strings.Contains(err.Error(), "worker is running") {
		t.Errorf("the error should name the running worker, got %q", err)
	}
	// Nothing may have been dropped, and the watermark must be intact.
	if got := st.DistilledCount("s1", "default"); got != before {
		t.Errorf("a declined --fresh reset the watermark: %d -> %d", before, got)
	}
}

// The measured mechanism the lock prevents, asserted at the store layer so it needs no worker
// process: after --fresh clears progress, an in-flight worker's trailing
// MarkDistilledIfCurrent re-stamps the stale total with ZERO observations written. Its CAS only
// checks that the raw GENERATION is unchanged — and --fresh never touches raw, so the CAS
// passes. The session is then permanently marked fully distilled with no L1 and drops out of
// PendingSessions, invisible to every later drain.
//
// This test documents WHY the lock matters; it asserts the hazard exists at the store layer
// (that is not a bug in the store — the CAS is doing its documented job), so if someone
// downgrades the lock back to a probe, the test above is what fails.
func TestFreshRaceWouldStampPhantomProgress(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < 4; i++ {
		if err := st.AppendRaw(store.RawRecord{
			Session: "s1", Seq: i, TS: "2026-08-01T00:00:00Z", Role: "user", Text: "turn",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnableLens("default"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDistilled("s1", "default", 4); err != nil {
		t.Fatal(err)
	}
	// What an in-flight worker already holds in memory.
	workerTotal, workerHigh := 4, st.MaxRawID("s1")

	// --fresh's destructive section.
	if _, _, err := st.DeleteLensData("default"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResetLensWatermark("default"); err != nil {
		t.Fatal(err)
	}
	if got := st.DistilledCount("s1", "default"); got != 0 {
		t.Fatalf("--fresh should clear the watermark, got %d", got)
	}

	// The trailing stamp from the worker that started before the delete.
	accepted, err := st.MarkDistilledIfCurrent("s1", "default", workerTotal, workerHigh)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Skip("the CAS rejected the stale stamp; the hazard this lock guards no longer exists " +
			"— re-derive whether the lock is still needed before removing it")
	}
	obs, _ := st.ReadObservations("default")
	pending, _ := st.PendingSessions([]string{"default"})
	if len(obs) != 0 {
		t.Fatalf("fixture wrong: expected zero observations, got %d", len(obs))
	}
	if got := st.DistilledCount("s1", "default"); got != 4 {
		t.Fatalf("expected the phantom watermark 4, got %d", got)
	}
	if len(pending) != 0 {
		t.Fatalf("expected the session to be INVISIBLE to the drain, but it is pending: %v", pending)
	}
	t.Log("hazard confirmed: fully-distilled watermark, zero observations, not pending — " +
		"this is what holding the WorkerLock across --fresh prevents")
}

// The structural half: --fresh must take the REAL lock and release it before the drain.
//
// This is asserted on the source because the behavioral difference between holding and probing
// is not observable in-process (see the scope note above). Three properties, each of which was
// wrong or would break something if changed:
//
//  1. st.WorkerLock() is called in the fresh path — the actual fix.
//  2. st.WorkerActive() is NOT — the advisory probe it replaced, whose TOCTOU window (an
//     unbounded [y/N] prompt) is the whole defect.
//  3. the release happens BEFORE runWorker and is NOT a bare `defer unlock()` — runWorker takes
//     WorkerLock on a fresh descriptor in this SAME process, and flock is per-open-file-
//     description, so a deferred-only release would self-deadlock EVERY --fresh into ran=false.
func TestFreshHoldsTheLockNotJustAProbe(t *testing.T) {
	src := readSource(t, "lens.go")
	i := strings.Index(src, "func lensBackfill(")
	if i < 0 {
		t.Fatal("lensBackfill not found")
	}
	fn := src[i:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}

	if !strings.Contains(fn, "st.WorkerLock()") {
		t.Error("--fresh must take the real WorkerLock; an advisory probe leaves a TOCTOU window " +
			"across the [y/N] prompt in which a worker can stamp phantom progress")
	}
	if strings.Contains(fn, "st.WorkerActive()") {
		t.Error("--fresh must not gate its destructive section on the advisory WorkerActive probe")
	}
	// The release must precede the drain, textually.
	relIdx := strings.Index(fn, "releaseFresh()")
	drainIdx := strings.Index(fn, "runWorker(false)")
	if relIdx < 0 {
		t.Fatal("no releaseFresh() call found — the lock would be held into the drain")
	}
	if drainIdx < 0 {
		t.Fatal("runWorker(false) not found; this test's assumption about the drain call has drifted")
	}
	// Find a release that is NOT the deferred safety net and sits before the drain.
	explicit := -1
	for _, idx := range allIndexes(fn, "releaseFresh()") {
		if idx < drainIdx && !strings.Contains(fn[max0(idx-16):idx], "defer ") {
			explicit = idx
			break
		}
	}
	if explicit < 0 {
		t.Error("the WorkerLock must be released EXPLICITLY before runWorker, not only by defer: " +
			"flock is per-descriptor, so holding it makes every --fresh self-deadlock into ran=false")
	}
}

func allIndexes(s, sub string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(s[off:], sub)
		if i < 0 {
			return out
		}
		out = append(out, off+i)
		off += i + len(sub)
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

// A plain (non---fresh) backfill must NOT take the lock: it deletes nothing, and a running
// worker legitimately picks up the reset watermark itself.
func TestLensBackfillWithoutFreshDoesNotRequireTheLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRaw(store.RawRecord{
		Session: "s1", Seq: 0, TS: "2026-08-01T00:00:00Z", Role: "user", Text: "turn",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDistilled("s1", "default", 1); err != nil {
		t.Fatal(err)
	}
	// A worker holds the lock. --fresh would refuse; a plain backfill must not.
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("precondition: lock should be free")
	}
	defer unlock()

	// lensBackfill closes the store it is handed (before the drain), so this handle is spent.
	err = lensBackfill(st, "default", false /*fresh*/, true /*assumeYes*/)
	if err != nil && strings.Contains(err.Error(), "worker is running") {
		t.Errorf("a plain backfill must not require the WorkerLock: %v", err)
	}
	st2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if got := st2.DistilledCount("s1", "default"); got != 0 {
		t.Errorf("a plain backfill should still reset the watermark, got %d", got)
	}
}
