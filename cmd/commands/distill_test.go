package commands

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// --all means the ENTIRE backlog; combining it with a time bound is contradictory,
// so cmdDistillBackfill rejects it before doing any work. (A bounded backfill is
// just `distill start --since ...`, the background path.)
func TestDistillBackfillRejectsTimeBounds(t *testing.T) {
	for _, tc := range []struct{ since, until string }{
		{"7d", ""},
		{"", "2026-07-01"},
		{"2026-06-01", "2026-07-01"},
	} {
		err := cmdDistillBackfill(true, tc.since, tc.until, false)
		if err == nil {
			t.Fatalf("--all with since=%q until=%q should error", tc.since, tc.until)
		}
		if !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("unexpected error for since=%q until=%q: %v", tc.since, tc.until, err)
		}
	}
}

// Review #2: `--all` must NOT report success when the backlog was not drained.
// With pending L0 but no embedding model available (the test env has none), the
// worker can't mine, so work stays pending — and cmdDistillBackfill must surface
// that as a non-nil error ("backfill incomplete"), not print "complete" + exit 0.
func TestDistillBackfillFailsWhenWorkRemains(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	// Lenses load from the repo's prompts/ (as other worker tests do); the embedder
	// points at an empty dir so embed.New fails (model not ready) — the drain then
	// can't mine and work stays pending, which is exactly what we're asserting on.
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Leave a pending session in L0 (no model → it can never be distilled here).
	if err := st.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	err = cmdDistillBackfill(true, "", "", false)
	if err == nil {
		t.Fatal("--all with undistillable pending work must return an error, not report success")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected an 'incomplete' error, got: %v", err)
	}
}

// --wait-backoffs is meaningless without --all (there's no foreground loop to retry
// in the detached-spawn path), so `distill start --wait-backoffs` alone must error
// rather than silently no-op.
func TestDistillStartWaitBackoffsRequiresAll(t *testing.T) {
	if err := newDistillStartForWaitBackoffsTest(t); err == nil {
		t.Fatal("--wait-backoffs without --all should error")
	} else if !strings.Contains(err.Error(), "only") || !strings.Contains(err.Error(), "--all") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// newDistillStartForWaitBackoffsTest exercises the RunE guard for --wait-backoffs
// without --all by driving the assembled cobra command with just that flag set.
func newDistillStartForWaitBackoffsTest(t *testing.T) error {
	t.Helper()
	cmd := newDistillCmd()
	cmd.SetArgs([]string{"start", "--wait-backoffs"})
	// Silence cobra's own usage/error printing; we only inspect the returned error.
	cmd.SetOut(nopWriter{})
	cmd.SetErr(nopWriter{})
	for _, c := range cmd.Commands() {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
	return cmd.Execute()
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// B4 self-heal: a session that's backed off but whose retry is soon (within maxWait)
// must be waited out and re-drained until it clears. We model two backoff rounds that
// resolve on the third rerun; the loop must sleep twice and rerun three times total
// beyond the initial pass (which the caller does), then stop when nothing is backed off.
func TestBackfillDrainWithRetrySelfHealsTransientBackoff(t *testing.T) {
	// backoffLeft counts down: 2 → 1 → 0 backed-off rounds remaining.
	backoffLeft := 2
	reruns := 0
	var slept []time.Duration
	err := backfillDrainWithRetry(context.Background(), backfillRetryDeps{
		rerun: func() error {
			reruns++
			if backoffLeft > 0 {
				backoffLeft--
			}
			return nil
		},
		waitForNextRetry: func() (time.Duration, bool) {
			if backoffLeft == 0 {
				return 0, false // healed → nothing outstanding
			}
			return 5 * time.Minute, true // a soon retry (within maxWait)
		},
		sleep:      func(_ context.Context, d time.Duration) { slept = append(slept, d) },
		maxWait:    backfillMaxWait,
		logWaiting: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two backed-off rounds → two sleeps + two reruns, then the third waitForNextRetry
	// reports clean and the loop exits.
	if reruns != 2 {
		t.Fatalf("reruns = %d, want 2 (one per backed-off round)", reruns)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(slept))
	}
	for _, d := range slept {
		if d != 5*time.Minute {
			t.Fatalf("each wait should be the reported 5m, got %s", d)
		}
	}
}

// B4 termination: a session whose soonest retry is BEYOND maxWait is treated as a
// deterministic failure — the loop must NOT wait it out (that could block the
// foreground for hours), it stops immediately and lets the end-state check report
// incomplete. Guards against a giant-session (B1) timeout wedging `--all`.
func TestBackfillDrainWithRetryStopsWhenRetryTooFarOut(t *testing.T) {
	reruns, sleeps := 0, 0
	err := backfillDrainWithRetry(context.Background(), backfillRetryDeps{
		rerun:            func() error { reruns++; return nil },
		waitForNextRetry: func() (time.Duration, bool) { return 2 * time.Hour, true }, // way past maxWait
		sleep:            func(context.Context, time.Duration) { sleeps++ },
		maxWait:          backfillMaxWait,
		logWaiting:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reruns != 0 || sleeps != 0 {
		t.Fatalf("a too-far-out retry must not sleep or rerun; reruns=%d sleeps=%d", reruns, sleeps)
	}
}

// B4: no outstanding backoff → the retry loop is a no-op (the caller already ran the
// initial drain pass); it must neither sleep nor rerun.
func TestBackfillDrainWithRetryNoBackoffsIsNoop(t *testing.T) {
	reruns, sleeps := 0, 0
	err := backfillDrainWithRetry(context.Background(), backfillRetryDeps{
		rerun:            func() error { reruns++; return nil },
		waitForNextRetry: func() (time.Duration, bool) { return 0, false },
		sleep:            func(context.Context, time.Duration) { sleeps++ },
		maxWait:          backfillMaxWait,
		logWaiting:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reruns != 0 || sleeps != 0 {
		t.Fatalf("no backoff should mean no work; reruns=%d sleeps=%d", reruns, sleeps)
	}
}

// B4: a cancelled context (Ctrl-C during a between-passes wait) aborts the loop
// promptly and returns nil — the caller's end-state check then reports the honest,
// still-incomplete state rather than the loop blocking or erroring.
func TestBackfillDrainWithRetryHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	reruns := 0
	err := backfillDrainWithRetry(ctx, backfillRetryDeps{
		rerun:            func() error { reruns++; return nil },
		waitForNextRetry: func() (time.Duration, bool) { return time.Minute, true },
		sleep:            func(context.Context, time.Duration) {},
		maxWait:          backfillMaxWait,
		logWaiting:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("cancel should return nil, got %v", err)
	}
	if reruns != 0 {
		t.Fatalf("a pre-cancelled context must not rerun; reruns=%d", reruns)
	}
}
