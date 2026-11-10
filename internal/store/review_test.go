package store

import (
	"reflect"
	"testing"
)

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

	// The new obs MUST be visible to the fold, or it is silently skipped forever (#125).
	pending, _ := s.ReadObservationsSinceOrdered(LensDefault, s.ReviewRowid(LensDefault))
	var sawNew bool
	for _, ob := range pending {
		if ob.ID == "d" {
			sawNew = true
		}
		if ob.ID == "c" {
			t.Fatalf("the deleted obs must not reappear: %+v", pending)
		}
	}
	if !sawNew {
		t.Fatalf("new obs after delete-of-newest must be pending (seq > watermark), got %d obs %+v — this is the #125 skip",
			len(pending), pending)
	}
	if got := s.UnreviewedDelta(LensDefault); got != len(pending) {
		t.Fatalf("UnreviewedDelta = %d but the fold read returned %d obs — they must agree", got, len(pending))
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

// (TestReadObservationsSinceWindowsAndOrdersByTS used to live here. It tested ReadObservationsSince
// and nothing else — the method it tested had no production caller, having been superseded by
// ReadObservationsSinceOrdered when the windowed fold needed SEQ order instead of ts order. The
// ORDERED variant is covered by db_test.go; this test went with its subject.)

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

// Deleting an observation must NOT move any review cursor. It used to clear the lens's, so the
// next review "re-derived from what's left" by re-folding the whole lens — ~650 model calls on a
// 20k-observation lens, and the facet the deleted obs grounded still survived, since evidence only
// grows. The facet is now cleaned up by the next review's id sweep (distill's
// pruneDeletedEvidence), and nothing is re-read.
func TestDeleteObservationKeepsTheReviewCursor(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "keep", TS: "2026-01-01T00:00:00Z", Lens: LensDefault, Observation: "keep"},
		{ID: "wrong", TS: "2026-01-02T00:00:00Z", Lens: LensDefault, Observation: "wrong"},
	})
	all, _ := s.ReadObservationsSinceOrdered(LensDefault, 0)
	folded := all[len(all)-1].Rowid
	if err := s.StampReviewLens(LensDefault, folded); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.DeleteObservation("wrong")
	if err != nil || !deleted {
		t.Fatalf("DeleteObservation: deleted=%v err=%v", deleted, err)
	}
	if got := s.ReviewRowid(LensDefault); got != folded {
		t.Fatalf("a delete must leave the review cursor at %d, got %d", folded, got)
	}
	if got := s.UnreviewedDelta(LensDefault); got != 0 {
		t.Fatalf("nothing may be re-offered to the fold after a delete, delta=%d", got)
	}
	if left, _ := s.ReadObservationsSinceOrdered(LensDefault, 0); len(left) != 1 || left[0].ID != "keep" {
		t.Fatalf("only the named obs is deleted, left %+v", left)
	}
	deleted, err = s.DeleteObservation("nonexistent")
	if err != nil || deleted {
		t.Fatalf("deleting a missing id: deleted=%v err=%v, want false, nil", deleted, err)
	}
}

// MissingObservationIDs answers for the whole archive: an id another lens holds is not missing,
// and duplicates or an empty query cost nothing.
func TestMissingObservationIDs(t *testing.T) {
	s := tempStore(t)
	s.AppendObservations([]Observation{
		{ID: "here", Lens: LensDefault, Observation: "a"},
		{ID: "elsewhere", Lens: "math", Observation: "b"},
	})
	got, err := s.MissingObservationIDs([]string{"here", "gone", "elsewhere", "gone2", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"gone": true, "gone2": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	if got, err := s.MissingObservationIDs(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty query: %v %v", got, err)
	}
}
