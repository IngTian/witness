package store

import (
	"database/sql"
	"testing"
)

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

// Every transaction that READS before it WRITES must hold the write lock up front. Under
// WAL a DEFERRED tx that takes a read snapshot and then writes fails with
// SQLITE_BUSY_SNAPSHOT (517) — a conflict busy_timeout CANNOT retry, because there is no
// lock to wait for; the snapshot is simply stale. witness is multi-process (a capture hook
// per user turn, a draining worker, an MCP server), so these must survive a concurrent
// commit. The three read-then-write sites are StampReview, DeleteObservation and
// PruneSessionsBefore; the other transactions in this package write first, which takes the
// lock immediately, so they are already safe.
func TestReadThenWriteTxSurvivesConcurrentWriter(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Store) error
	}{
		{"StampReview", func(s *Store) error { return s.StampReview() }},
		{"DeleteObservation", func(s *Store) error { _, err := s.DeleteObservation("o1"); return err }},
		{"PruneSessionsBefore", func(s *Store) error { _, _, err := s.PruneSessionsBefore("2025-01-01T00:00:00Z"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for attempt := 0; attempt < 5; attempt++ { // the race is timing-dependent
				s := tempStore(t)
				_ = s.AppendRaw(RawRecord{Session: "old", Seq: 0, TS: "2020-01-01T00:00:00Z", Role: "user", Text: "x"})
				_ = s.AppendObservations([]Observation{{ID: "o1", Lens: LensDefault, Session: "old", Observation: "x", Poignancy: 3}})

				other, err := sql.Open("sqlite", s.dbPath())
				if err != nil {
					t.Fatal(err)
				}
				other.SetMaxOpenConns(1)
				for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
					if _, err := other.Exec(p); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() {
					_, e := other.Exec(`INSERT INTO raw(session,seq,ts,role,text) VALUES ('concurrent',0,'2030-01-01T00:00:00Z','user','a turn')`)
					done <- e
				}()
				runErr := tc.run(s)
				<-done
				other.Close()
				if runErr != nil {
					t.Fatalf("attempt %d: must survive a concurrent writer, got: %v (517 = SQLITE_BUSY_SNAPSHOT)", attempt, runErr)
				}
			}
		})
	}
}

// DeleteLensData (the `lens backfill --fresh` drop) must clear the lens's DERIVED
// bookkeeping too, not just its observations and facets. Those meta keys are caches ABOUT
// the deleted data, so leaving them makes a rebuilt lens look already-processed:
//   - emerge_seen:<lens>:<sig> keys a cluster by its member obs_ids, and obs_id is
//     sha1(session|lens|text) — a CONTENT hash — so a re-mine of unchanged text
//     regenerates identical ids and an identical signature. The emergent pass would skip
//     re-verifying an arc whose facets --fresh just deleted, and nothing would restore them.
//   - profile_sig:<lens> would vouch for an L4 narrative derived from facets that are gone.
//
// A SIBLING lens's state must survive untouched.
func TestDeleteLensDataClearsDerivedState(t *testing.T) {
	s := tempStore(t)
	_ = s.AppendObservations([]Observation{
		{ID: "obs_a", Lens: LensDefault, Session: "s", Observation: "x"},
		{ID: "obs_b", Lens: "sibling", Session: "s", Observation: "y"},
	})
	_ = s.WriteFacets([]Facet{
		{Lens: LensDefault, Dimension: "d", Key: "k", Versions: []FacetVersion{{Value: "v", ValidFrom: "t"}}},
		{Lens: "sibling", Dimension: "d", Key: "k", Versions: []FacetVersion{{Value: "v", ValidFrom: "t"}}},
	})
	_ = s.SetMetaString("emerge_seen:"+LensDefault+":sig1", `{"outcome":"accepted","members":9}`)
	_ = s.SetMetaString("emerge_seen:"+LensDefault+":sig2", `{"outcome":"rejected","members":4}`)
	_ = s.SetMetaString("profile_sig:"+LensDefault, "old-facet-signature")
	_ = s.SetMetaString("emerge_seen:sibling:sigX", `{"outcome":"accepted","members":3}`)
	_ = s.SetMetaString("profile_sig:sibling", "sibling-signature")
	_ = s.SetMetaString("review_ts", "2026-01-01T00:00:00Z") // a GLOBAL key, must survive

	if _, _, err := s.DeleteLensData(LensDefault); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{
		"emerge_seen:" + LensDefault + ":sig1",
		"emerge_seen:" + LensDefault + ":sig2",
		"profile_sig:" + LensDefault,
	} {
		if got := s.MetaString(gone); got != "" {
			t.Errorf("--fresh must clear derived key %q, still %q", gone, got)
		}
	}
	for _, kept := range []string{"emerge_seen:sibling:sigX", "profile_sig:sibling", "review_ts"} {
		if got := s.MetaString(kept); got == "" {
			t.Errorf("must NOT touch %q", kept)
		}
	}
}

// A lens name containing a LIKE wildcard must be escaped, or '_' (which matches ANY
// character) would let one lens's --fresh delete a sibling's emergent bookkeeping.
func TestDeleteLensDataEscapesLikeWildcardsInLensName(t *testing.T) {
	s := tempStore(t)
	_ = s.SetMetaString("emerge_seen:my_lens:sig", "target")
	_ = s.SetMetaString("emerge_seen:myXlens:sig", "innocent bystander")
	_ = s.SetMetaString("emerge_seen:my%lens:sig", "percent lens")

	if _, _, err := s.DeleteLensData("my_lens"); err != nil {
		t.Fatal(err)
	}
	if got := s.MetaString("emerge_seen:my_lens:sig"); got != "" {
		t.Errorf("the named lens's key must be cleared, still %q", got)
	}
	if got := s.MetaString("emerge_seen:myXlens:sig"); got == "" {
		t.Error("'_' must be escaped: a lens whose name merely matches the wildcard was wiped")
	}
	// And a '%' in the name must not turn into "match everything".
	if _, _, err := s.DeleteLensData("my%lens"); err != nil {
		t.Fatal(err)
	}
	if got := s.MetaString("emerge_seen:my%lens:sig"); got != "" {
		t.Errorf("the percent-named lens's key must be cleared, still %q", got)
	}
}
