package distill

import (
	"context"
	"testing"
	"time"
)

// The end-to-end consequence: a TRUNCATED mine reply must not advance the watermark.
//
// Drift advances it deliberately (a below-floor model would otherwise wedge the queue), and
// truncation used to be classified as drift. So a reply carrying real observations that was
// cut off by an output cap advanced the watermark past those turns — and because the
// watermark counts raw records, the session was never offered again and the observations
// were gone. Contrast TestDriftPersistsAndClearsOnReMine, which pins the opposite behavior
// for genuine drift.
func TestTruncatedMineReplyBacksOffInsteadOfAdvancingTheWatermark(t *testing.T) {
	s := newStore(t)
	truncates := true
	w := testWorker(s, &fakeMiner{})
	w.Run = func(_ context.Context, _, _, _ string) (string, error) {
		if truncates {
			// Two complete observations and a third cut off — real extracted content.
			return `[{"dimension":"thinking","observation":"first real finding","evidence":"e","poignancy":3},
			         {"dimension":"thinking","observation":"second real finding","evidence":"e","poignancy":3},
			         {"dimension":"thinking","observation":"third, cut off mid`, nil
		}
		return `[{"dimension":"thinking","observation":"recovered","evidence":"e","poignancy":3}]`, nil
	}
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	// Pass 1: truncated. Process reports no error (per-lens failures are absorbed), but the
	// delta must be RETAINED for a retry.
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := s.DistilledCount("s", "default"); got != 0 {
		t.Fatalf("a truncated reply advanced the watermark to %d — the observations it was "+
			"mid-way through listing are now unreachable", got)
	}
	// It is a FAILURE, not drift: nothing should be stamped as a below-floor model.
	if got := s.DriftAt("s", "default"); got != "" {
		t.Errorf("truncation must not be recorded as prose drift, got drift_at=%q", got)
	}
	if got := s.DriftTotal(); got != 0 {
		t.Errorf("truncation must not count toward DriftTotal, got %d", got)
	}

	// The lens is now sleeping off a retry backoff (5m for the first failure) — which is
	// precisely the desired outcome, and the proof it was treated as a failure rather than
	// drift (drift never backs off). Assert that, then clear it to stand in for elapsed time.
	if !s.LensBackedOff("s", "default", time.Now()) {
		t.Fatal("a truncated reply must back the lens off so the same delta is retried")
	}
	if err := s.SetNextAttempt("s", "default", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Pass 2: the model completes its reply. The SAME delta re-mines and lands.
	truncates = false
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	obs, err := s.ReadObservations("default")
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Observation != "recovered" {
		t.Fatalf("the retried mine must produce the observation, got %+v", obs)
	}
	if got := s.DistilledCount("s", "default"); got != 2 {
		t.Errorf("after a clean re-mine the watermark should cover both raw rows, got %d", got)
	}
}
