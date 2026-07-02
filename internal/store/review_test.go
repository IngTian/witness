package store

import "testing"

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
