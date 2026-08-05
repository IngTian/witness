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
