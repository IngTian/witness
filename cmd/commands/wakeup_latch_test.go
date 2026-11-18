package commands

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/proc"
	"github.com/IngTian/witness/internal/store"
)

// openTestStore opens a fresh archive under a temp WITNESS_HOME, closed at cleanup.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A wakeup that failed to spawn must not latch its stamp.
//
// The stamp used to be written BEFORE the spawn, and the spawn's error was discarded
// (spawnDetached returned nothing). So a failed spawn left a PHANTOM LATCH: the dedup guard
// sees a future stamp and suppresses every subsequent attempt to arm the wakeup — including
// from other processes — for the whole remaining backoff window, up to the 6h cap.
//
// Nothing is lost (raw L0 is durable, the watermark did not advance), but the AUTOMATIC retry
// never fires: the delta then waits for an unrelated external trigger, the user's next
// session-start/-end or the OpenCode plugin's quiet-period kick. On a machine that just hit a
// transient rate limit, automatic is precisely what was promised.
func TestFailedWakeupSpawnDoesNotLatchTheStamp(t *testing.T) {
	st := openTestStore(t)
	next := time.Now().Add(30 * time.Minute)

	failing := func(...string) bool { return false }
	scheduleWorkerWakeupWith(st, next, "auto", failing)

	if got := st.MetaString(workerWakeupKey("auto")); got != "" {
		t.Fatalf("a failed spawn latched the stamp %q — every later re-arm is now suppressed "+
			"and the automatic retry never fires", got)
	}

	// The whole point: a LATER trigger must still be able to arm it.
	spawns := 0
	ok := func(...string) bool { spawns++; return true }
	scheduleWorkerWakeupWith(st, next, "auto", ok)
	if spawns != 1 {
		t.Fatalf("after a failed spawn, the next attempt must re-arm; spawned %d times", spawns)
	}
	if got := st.MetaString(workerWakeupKey("auto")); got == "" {
		t.Error("a SUCCESSFUL spawn must record the stamp, else the dedup guard is inert " +
			"and every trigger spawns another wakeup")
	}
}

// The dedup guard must still work on the success path: an already-armed, earlier-or-equal
// wakeup covers the work, so re-arming is suppressed. Losing this would busy-spawn.
func TestSuccessfulWakeupStillDedupes(t *testing.T) {
	st := openTestStore(t)
	next := time.Now().Add(30 * time.Minute)
	spawns := 0
	ok := func(...string) bool { spawns++; return true }

	scheduleWorkerWakeupWith(st, next, "auto", ok)
	if spawns != 1 {
		t.Fatalf("first arm should spawn once, got %d", spawns)
	}
	// Same time, and a LATER time: both are already covered.
	scheduleWorkerWakeupWith(st, next, "auto", ok)
	scheduleWorkerWakeupWith(st, next.Add(5*time.Minute), "auto", ok)
	if spawns != 1 {
		t.Errorf("an already-armed wakeup must suppress equal/later re-arms; spawned %d times", spawns)
	}
	// An EARLIER need must get through — it is not covered by the later stamp.
	scheduleWorkerWakeupWith(st, time.Now().Add(time.Minute), "auto", ok)
	if spawns != 2 {
		t.Errorf("an earlier wakeup must be armed (work would otherwise wait); spawned %d times", spawns)
	}
}

// The two modes keep independent stamps: a failed auto arm must not disturb a live manual one.
func TestWakeupModesAreIndependent(t *testing.T) {
	st := openTestStore(t)
	next := time.Now().Add(30 * time.Minute)
	ok := func(...string) bool { return true }
	failing := func(...string) bool { return false }

	scheduleWorkerWakeupWith(st, next, "manual", ok)
	manual := st.MetaString(workerWakeupKey("manual"))
	if manual == "" {
		t.Fatal("precondition: the manual wakeup should be armed")
	}
	scheduleWorkerWakeupWith(st, next, "auto", failing)
	if got := st.MetaString(workerWakeupKey("manual")); got != manual {
		t.Errorf("a failed AUTO arm disturbed the manual stamp: %q -> %q", manual, got)
	}
	if got := st.MetaString(workerWakeupKey("auto")); got != "" {
		t.Errorf("the failed auto arm latched: %q", got)
	}
}

// Under `go test` the real spawner is suppressed on purpose, so it must report SUCCESS —
// otherwise every test exercising this path would log "retry not armed" and, worse, would
// leave the stamp unwritten, changing the state under test.
func TestSpawnDetachedOKReportsSuccessWhenSuppressedUnderTest(t *testing.T) {
	if !spawnDetachedOK("worker-wakeup", "1", "stamp", "auto") {
		t.Error("the under-test no-op must report success; reporting failure would make the " +
			"suppression change behavior instead of being inert")
	}
	st := openTestStore(t)
	scheduleWorkerWakeup(st, time.Now().Add(10*time.Minute), "auto")
	if got := st.MetaString(workerWakeupKey("auto")); got == "" {
		t.Error("scheduleWorkerWakeup through the real spawner must still stamp under test")
	}
}

// stopCtl is a proc.Fake whose NotifyStop calls onNotify with the stop ctx's cancel — the point
// mid-run where the worker starts watching for a stop — so a test can fire one there.
type stopCtl struct {
	*proc.Fake
	onNotify func(cancel context.CancelFunc)
}

func (c stopCtl) NotifyStop(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := c.Fake.NotifyStop(parent)
	c.onNotify(cancel)
	return ctx, cancel
}

// A run that ends on a stop arms no wakeup, an older deferred need's (here OpenCode's usage pause)
// included: `worker stop` clears every wakeup before its SIGTERM, and one re-armed by the dying
// worker resumed the stopped drain minutes later (G2, reproduced). Both stop signals count — the
// SIGTERM that cancels the drain ctx, and the flag a stop whose signal missed leaves behind.
func TestAStoppedWorkerArmsNoWakeup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stop  func(st *store.Store, cancel context.CancelFunc)
		armed bool
	}{
		{"no stop", func(*store.Store, context.CancelFunc) {}, true},
		{"SIGTERM cancels the drain", func(_ *store.Store, cancel context.CancelFunc) { cancel() }, false},
		{"stop flag only", func(st *store.Store, _ context.CancelFunc) {
			_ = st.SetMetaString(metaWorkerStopRequested, "1")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			_ = st.SetMetaString(usagePauseKey("opencode"), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			prev := procCtl
			t.Cleanup(func() { procCtl = prev })
			procCtl = stopCtl{&proc.Fake{}, func(cancel context.CancelFunc) { tc.stop(st, cancel) }}
			if ran, err := runWorker(false); err != nil || !ran {
				t.Fatalf("ran=%v err=%v", ran, err)
			}
			got := st.MetaString(workerWakeupKey("manual")) + st.MetaString(workerWakeupKey("auto"))
			if (got != "") != tc.armed {
				t.Fatalf("wakeup stamp %q, want armed=%v", got, tc.armed)
			}
		})
	}
}
