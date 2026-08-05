package store

import "testing"

// `witness cleanup` deliberately keeps L1 while reclaiming L0. A later `lens backfill
// --fresh` then deletes those observations on the promise "Mined observations are
// re-created from them" — but the re-mine enumerates candidate sessions FROM `raw`, so a
// session with no raw left is never offered and its observations are destroyed
// permanently. OrphanedL0ObservationCount lets the CLI warn about that class BEFORE the
// irreversible delete (mirroring the existing warning for hand-recorded 'active' obs).
func TestOrphanedL0ObservationCount(t *testing.T) {
	s := tempStore(t)
	_ = s.AppendRaw(RawRecord{Session: "kept", Seq: 0, TS: "2030-01-01T00:00:00Z", Role: "user", Text: "recent"})
	_ = s.AppendRaw(RawRecord{Session: "pruned", Seq: 0, TS: "2020-01-01T00:00:00Z", Role: "user", Text: "old"})
	_ = s.AppendObservations([]Observation{
		{ID: "o_kept", Lens: LensDefault, Session: "kept", Observation: "still backed by L0", Source: "mined"},
		{ID: "o_pruned", Lens: LensDefault, Session: "pruned", Observation: "L0 will be reclaimed", Source: "mined"},
		{ID: "o_active", Lens: LensDefault, Session: "pruned", Observation: "hand recorded", Source: "active"},
		{ID: "o_other", Lens: "sibling", Session: "pruned", Observation: "another lens", Source: "mined"},
	})

	// Before any cleanup every mined obs still has its L0.
	if got := s.OrphanedL0ObservationCount(LensDefault); got != 0 {
		t.Fatalf("before cleanup: want 0 orphaned, got %d", got)
	}

	// `witness cleanup` reclaims the idle session's raw, keeping L1.
	if _, _, err := s.PruneSessionsBefore("2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// Exactly the MINED obs of the pruned session counts. The 'active' one is already
	// covered by ActiveObservationCount (don't double-warn), and a sibling lens is separate.
	if got := s.OrphanedL0ObservationCount(LensDefault); got != 1 {
		t.Fatalf("after cleanup: want 1 orphaned mined obs for %q, got %d", LensDefault, got)
	}
	if got := s.OrphanedL0ObservationCount("sibling"); got != 1 {
		t.Fatalf("sibling lens is counted independently: got %d, want 1", got)
	}
	// And it is genuinely unrecoverable: the re-mine never offers that session.
	pending, err := s.PendingSessions([]string{LensDefault})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p == "pruned" {
			t.Fatal("a session with no raw must not be offered for re-mining")
		}
	}
}
