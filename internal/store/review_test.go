package store

import "testing"

// TestReadObservationsSinceOrderedByRowid locks the windowed-fold read (#123): it
// returns obs for the lens with rowid > since, in ROWID order, each carrying its
// rowid. Rowid order (not ts) is what lets the windowed fold advance the watermark
// per contiguous window without ever skipping a low-rowid/high-ts obs.
func TestReadObservationsSinceOrderedByRowid(t *testing.T) {
	s := tempStore(t)
	// Insert so ts order != rowid order: rowid 1 has the LATER ts.
	s.AppendObservations([]Observation{{ID: "a", TS: "2026-03-01T00:00:00Z", Lens: LensDefault, Observation: "a"}})
	s.AppendObservations([]Observation{{ID: "b", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "b"}})
	s.AppendObservations([]Observation{{ID: "c", TS: "2026-02-01T00:00:00Z", Lens: LensDefault, Observation: "c"}})

	got, err := s.ReadObservationsSinceOrdered(LensDefault, 0)
	if err != nil {
		t.Fatalf("ReadObservationsSinceOrdered: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 obs, got %d", len(got))
	}
	// Rowid order → insertion order a,b,c (NOT ts order b,c,a).
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("want rowid order [a,b,c], got [%s,%s,%s]", got[0].ID, got[1].ID, got[2].ID)
	}
	// Each carries a monotonically increasing rowid.
	if !(got[0].Rowid > 0 && got[1].Rowid > got[0].Rowid && got[2].Rowid > got[1].Rowid) {
		t.Fatalf("rowids must be present + increasing, got %d,%d,%d", got[0].Rowid, got[1].Rowid, got[2].Rowid)
	}
	// Windowing past the first rowid drops it.
	got2, _ := s.ReadObservationsSinceOrdered(LensDefault, got[0].Rowid)
	if len(got2) != 2 || got2[0].ID != "b" {
		t.Fatalf("since first rowid should return [b,c], got %d obs", len(got2))
	}
}

// TestReviewCursorSurvivesDeleteNewestReMine is the #125 root-cause repro: the fold
// cursor (Observation.Rowid, advanced by StampReviewLens) must be MONOTONIC — a
// freshly appended obs always gets a cursor value strictly greater than any obs that
// ever existed, so it can never land at or below an already-advanced watermark.
//
// The pre-#125 schema (obs_id TEXT PRIMARY KEY, no AUTOINCREMENT) used the implicit
// rowid, which SQLite REUSES after the max-rowid row is deleted. Sequence: mine a..c
// (cursor 1,2,3), review through 3, delete c (the newest), then mine a genuinely new
// obs d. On the reused-rowid schema d lands at cursor 3 <= watermark 3 → the windowed
// fold (rowid > watermark) never re-reads it → silent permanent skip. With the seq
// AUTOINCREMENT column, d gets cursor 4 > 3 and folds normally.
func TestReviewCursorSurvivesDeleteNewestReMine(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "a"},
		{ID: "b", TS: "2026-01-02T00:00:00Z", Lens: LensDefault, Observation: "b"},
		{ID: "c", TS: "2026-01-03T00:00:00Z", Lens: LensDefault, Observation: "c"},
	})
	all, _ := s.ReadObservationsSinceOrdered(LensDefault, 0)
	if len(all) != 3 {
		t.Fatalf("precondition: want 3 obs, got %d", len(all))
	}
	newestCursor := all[len(all)-1].Rowid // 'c'

	// A review folded everything through the newest obs.
	if err := s.StampReviewLens(LensDefault, newestCursor); err != nil {
		t.Fatalf("StampReviewLens: %v", err)
	}
	// Delete the NEWEST obs (the human "prune a wrong observation" lever), freeing its cursor.
	if _, err := s.DeleteObservation("c"); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}
	// Mine a genuinely new observation.
	s.AppendObservations([]Observation{
		{ID: "d", TS: "2026-01-04T00:00:00Z", Lens: LensDefault, Observation: "d"},
	})

	// The new obs MUST have a cursor strictly greater than the watermark, or the fold
	// (rowid > watermark) silently skips it forever.
	pending, _ := s.ReadObservationsSinceOrdered(LensDefault, s.ReviewRowid(LensDefault))
	if len(pending) != 1 || pending[0].ID != "d" {
		t.Fatalf("new obs after delete-of-newest must be pending (cursor > watermark), got %d obs %+v — this is the #125 skip",
			len(pending), pending)
	}
	if got := s.UnreviewedDelta(LensDefault); got != 1 {
		t.Fatalf("UnreviewedDelta after delete-newest+re-mine = %d, want 1 (the skipped obs)", got)
	}
}

// TestStampReviewLensThroughRowid locks the per-window stamp: the watermark advances to
// the GIVEN rowid (this window's max), not the global MAX(rowid) — so stamping window 1
// cannot jump the cursor past unfolded windows 2..N.
func TestStampReviewLensThroughRowid(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "a"},
		{ID: "b", TS: "2026-01-02T00:00:00Z", Lens: LensDefault, Observation: "b"},
		{ID: "c", TS: "2026-01-03T00:00:00Z", Lens: LensDefault, Observation: "c"},
	})
	// Stamp only through the FIRST obs's rowid; the watermark must be exactly that,
	// NOT the global max (3) — the later two obs stay pending.
	if err := s.StampReviewLens(LensDefault, 1); err != nil {
		t.Fatalf("StampReviewLens: %v", err)
	}
	if got := s.ReviewRowid(LensDefault); got != 1 {
		t.Fatalf("watermark = %d, want 1 (this window's rowid, not global max)", got)
	}
	pending, _ := s.ReadObservationsSinceOrdered(LensDefault, s.ReviewRowid(LensDefault))
	if len(pending) != 2 {
		t.Fatalf("after stamping through rowid 1, want 2 obs still pending, got %d", len(pending))
	}
}

// TestResetLensWatermarkClearsReviewRowid locks the #123 --fresh fix: ResetLensWatermark
// (called by `lens backfill --fresh`) must clear the per-lens REVIEW watermark too, not
// just the mining watermark — else a re-mine that reuses low rowids folds an empty delta
// and leaves the lens with the empty facets --fresh dropped.
func TestResetLensWatermarkClearsReviewRowid(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "a"},
		{ID: "b", TS: "2026-01-02T00:00:00Z", Lens: LensDefault, Observation: "b"},
	})
	if err := s.StampReviewLens(LensDefault, 2); err != nil {
		t.Fatalf("StampReviewLens: %v", err)
	}
	if s.ReviewRowid(LensDefault) != 2 {
		t.Fatalf("precondition: watermark should be 2, got %d", s.ReviewRowid(LensDefault))
	}
	if _, err := s.ResetLensWatermark(LensDefault); err != nil {
		t.Fatalf("ResetLensWatermark: %v", err)
	}
	if got := s.ReviewRowid(LensDefault); got != 0 {
		t.Fatalf("--fresh must reset the review watermark to 0, got %d (a re-mine would then fold an empty delta)", got)
	}
}

// TestUnreviewedDelta locks the doctor-visibility count: obs for a lens with rowid past
// its review watermark.
func TestUnreviewedDelta(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "a", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "a"},
		{ID: "b", TS: "2026-01-02T00:00:00Z", Lens: LensDefault, Observation: "b"},
		{ID: "x", TS: "2026-01-03T00:00:00Z", Lens: "other", Observation: "x"},
	})
	if got := s.UnreviewedDelta(LensDefault); got != 2 {
		t.Fatalf("unreviewed delta (unstamped) = %d, want 2", got)
	}
	s.StampReviewLens(LensDefault, 1)
	if got := s.UnreviewedDelta(LensDefault); got != 1 {
		t.Fatalf("after stamping through rowid 1, delta = %d, want 1", got)
	}
}

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

	if err := s.StampReviewLens(LensDefault, maxRow); err != nil {
		t.Fatalf("StampReviewLens: %v", err)
	}
	if got := s.ReviewRowid(LensDefault); got != maxRow {
		t.Fatalf("default watermark = %d, want %d (stamped through-rowid)", got, maxRow)
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
