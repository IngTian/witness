package store

import "testing"

// CommitLensDistillation is the atomic write for issue #67-2: it writes the lens-
// mined observations AND advances the successful lenses' watermarks in ONE
// generation-gated transaction, so mined obs derived from a since-replaced raw
// generation are never written (no orphans). These tests lock that contract.

// countObs returns how many L1 observations exist for a session (across all lenses).
func countObs(t *testing.T, s *Store, session string) int {
	t.Helper()
	obs, err := s.ReadObservations("")
	if err != nil {
		t.Fatalf("ReadObservations: %v", err)
	}
	n := 0
	for _, o := range obs {
		if o.Session == session {
			n++
		}
	}
	return n
}

// TestCommitLensDistillationWritesAndAdvancesWhenCurrent: on a live generation, the
// mined obs land in L1 AND every named lens's watermark advances — reported true.
func TestCommitLensDistillationWritesAndAdvancesWhenCurrent(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "sess", 3)
	high := s.MaxRawID("sess")

	mined := []Observation{
		{ID: "obs_a", Session: "sess", Lens: "default", Observation: "a", Source: "mined"},
		{ID: "obs_b", Session: "sess", Lens: "codereview", Observation: "b", Source: "mined"},
	}
	advanced, err := s.CommitLensDistillation(mined, "sess", 3, high, []string{"default", "codereview"})
	if err != nil {
		t.Fatalf("CommitLensDistillation: %v", err)
	}
	if !advanced {
		t.Fatal("advanced=false on a live generation; want true")
	}
	if n := countObs(t, s, "sess"); n != 2 {
		t.Fatalf("wrote %d obs; want 2 (both mined obs on a live generation)", n)
	}
	if got := s.DistilledCount("sess", "default"); got != 3 {
		t.Fatalf("default watermark = %d; want 3", got)
	}
	if got := s.DistilledCount("sess", "codereview"); got != 3 {
		t.Fatalf("codereview watermark = %d; want 3", got)
	}
}

// TestCommitLensDistillationWritesNothingWhenGenerationStale is the #67-2 guard: if
// the mined generation's high id no longer exists (an edit-style replace landed
// mid-mine), NEITHER the mined obs nor any watermark advance is written — reported
// false — so the session cleanly re-mines the new generation with no orphan obs.
func TestCommitLensDistillationWritesNothingWhenGenerationStale(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "sess", 3)
	staleHigh := s.MaxRawID("sess")

	// A replace-import edits the session: old ids gone, a fresh higher generation.
	if err := replaceGen(s, "sess", 2); err != nil {
		t.Fatalf("replaceGen: %v", err)
	}

	mined := []Observation{
		{ID: "obs_stale", Session: "sess", Lens: "default", Observation: "from old gen", Source: "mined"},
	}
	advanced, err := s.CommitLensDistillation(mined, "sess", 3, staleHigh, []string{"default"})
	if err != nil {
		t.Fatalf("CommitLensDistillation: %v", err)
	}
	if advanced {
		t.Fatal("advanced=true over a replaced generation; want false")
	}
	if n := countObs(t, s, "sess"); n != 0 {
		t.Fatalf("wrote %d obs over a stale generation; want 0 (no orphans)", n)
	}
	if got := s.DistilledCount("sess", "default"); got != 0 {
		t.Fatalf("watermark advanced to %d over a stale generation; want 0", got)
	}
}

// TestCommitLensDistillationIsIdempotent locks obsID dedup: committing the same
// mined batch twice (a crash-then-re-run) never duplicates L1 rows.
func TestCommitLensDistillationIsIdempotent(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "sess", 2)
	high := s.MaxRawID("sess")
	mined := []Observation{
		{ID: "obs_x", Session: "sess", Lens: "default", Observation: "x", Source: "mined"},
	}
	for i := 0; i < 2; i++ {
		if _, err := s.CommitLensDistillation(mined, "sess", 2, high, []string{"default"}); err != nil {
			t.Fatalf("CommitLensDistillation pass %d: %v", i, err)
		}
	}
	if n := countObs(t, s, "sess"); n != 1 {
		t.Fatalf("wrote %d obs after 2 identical commits; want 1 (obsID dedup)", n)
	}
}
