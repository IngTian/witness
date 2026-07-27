package store

import "testing"

// TestReadObservationsSinceWindowsAndOrdersByTS locks the incremental-fold read (#16):
// it returns only observations for the lens with rowid > sinceRowid, in valid-time
// (ts) order — NOT insertion/rowid order (which, under the parallel commit-as-ready
// drain, is not valid-time order). Folding "the stance as it was at that moment"
// requires ts order.
func TestReadObservationsSinceWindowsAndOrdersByTS(t *testing.T) {
	s := tempStore(t)
	// Insert in an order where rowid order != ts order (simulating out-of-ts commit
	// under #56-B3): rowid 1 has the LATER ts, rowid 2 the earlier.
	s.AppendObservations([]Observation{{ID: "late", TS: "2026-02-01T00:00:00Z", Lens: LensDefault, Observation: "late"}})
	s.AppendObservations([]Observation{{ID: "early", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "early"}})
	s.AppendObservations([]Observation{{ID: "other", TS: "2026-03-01T00:00:00Z", Lens: "codereview", Observation: "x"}})

	// Since 0 → all default-lens obs, ts-ordered (early before late despite rowid order).
	got, err := s.ReadObservationsSince(LensDefault, 0)
	if err != nil {
		t.Fatalf("ReadObservationsSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 default-lens obs, got %d", len(got))
	}
	if got[0].ID != "early" || got[1].ID != "late" {
		t.Fatalf("want ts order [early, late], got [%s, %s]", got[0].ID, got[1].ID)
	}

	// Window past the first two rows → only rows with a higher rowid remain (none for
	// default lens; the codereview row is a different lens and excluded regardless).
	got, err = s.ReadObservationsSince(LensDefault, 2)
	if err != nil {
		t.Fatalf("ReadObservationsSince(2): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 default-lens obs beyond rowid 2, got %d", len(got))
	}
}

// TestPerLensReviewWatermarkAdvancesIndependently locks the per-lens fold watermark:
// StampReviewLens advances one lens's review_rowid to the current max obs rowid
// without touching a sibling lens's — so a healthy lens folds forward even when a
// sibling's review failed (the clean #55 fix).
func TestPerLensReviewWatermarkAdvancesIndependently(t *testing.T) {
	s := tempStore(t)
	if got := s.ReviewRowid(LensDefault); got != 0 {
		t.Fatalf("unstamped lens must read watermark 0, got %d", got)
	}
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "a"},
		{ID: "b", TS: "2026-01-02T00:00:00Z", Lens: "codereview", Observation: "b"},
	})
	maxRow := int64(2)

	if err := s.StampReviewLens(LensDefault); err != nil {
		t.Fatalf("StampReviewLens: %v", err)
	}
	if got := s.ReviewRowid(LensDefault); got != maxRow {
		t.Fatalf("default watermark = %d, want %d (current max rowid)", got, maxRow)
	}
	// The sibling lens was not stamped — its watermark stays 0.
	if got := s.ReviewRowid("codereview"); got != 0 {
		t.Fatalf("unstamped sibling watermark = %d, want 0 (independent)", got)
	}
}

func TestPoignancySinceReview(t *testing.T) {
	s := tempStore(t)
	// No review stamped yet → everything counts.
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2020-01-01T00:00:00Z", Lens: LensDefault, Poignancy: 5},
		{ID: "b", TS: "2020-01-02T00:00:00Z", Lens: LensDefault, Poignancy: 3},
	})
	if got := s.PoignancySinceReview(); got != 8 {
		t.Fatalf("before any review: want 8, got %d", got)
	}

	// After a review, only observations newer than the stamp count.
	if err := s.StampReview(); err != nil {
		t.Fatal(err)
	}
	s.AppendObservations([]Observation{
		{ID: "c", TS: "2099-01-01T00:00:00Z", Lens: LensDefault, Poignancy: 7},
	})
	if got := s.PoignancySinceReview(); got != 7 {
		t.Fatalf("after review: want 7 (old excluded), got %d", got)
	}
}

func TestReviewDue(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2099-01-01T00:00:00Z", Lens: LensDefault, Poignancy: 7},
	})

	// Poignancy threshold reached, even though the session count cap is far off.
	if !s.ReviewDue(Config{ReviewEvery: 999, ReviewPoignancy: 6}) {
		t.Errorf("poignancy 7 >= 6 should be due")
	}
	// Neither trigger met → not due.
	if s.ReviewDue(Config{ReviewEvery: 999, ReviewPoignancy: 999}) {
		t.Errorf("neither trigger met should NOT be due")
	}
	// Poignancy trigger disabled (0) and count not met → not due.
	if s.ReviewDue(Config{ReviewEvery: 999, ReviewPoignancy: 0}) {
		t.Errorf("disabled poignancy + count not met should NOT be due")
	}
}
