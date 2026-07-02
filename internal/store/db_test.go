package store

import (
	"testing"
)

// The port introduced guarantees the file backend never had; these lock them in.

func TestSchemaVersionStamped(t *testing.T) {
	s := tempStore(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

// migrate() must be safe to re-run over an already-applied schema. A crash after
// the CREATE TABLEs but before the user_version bump leaves tables on disk with
// user_version still behind; re-running must recover (not error "table already
// exists", which would brick every future Open).
func TestMigrateIdempotentAfterPartialApply(t *testing.T) {
	s := tempStore(t) // fully migrated
	if _, err := s.db.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("migrate must be idempotent over an applied schema, got: %v", err)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version not restored to %d, got %d", schemaVersion, v)
	}
}

// A pre-rename database has the raw layer under its old name `l0`. Opening it must
// rename the table to `raw` in place, preserving the data, remove the legacy table,
// and stamp the current version — so no deployed DB is bricked or loses history when
// the on-disk names are modernized. (This migrates from a real v2 `l0` DB, not from
// a fresh tempStore — the coverage gap that a prior collapse slipped through.)
func TestMigrateRenamesLegacyL0(t *testing.T) {
	s := tempStore(t) // fully migrated (has `raw`)
	// Simulate a legacy v2 DB: replace `raw` with the old `l0` schema + a row.
	for _, stmt := range []string{
		`DROP TABLE raw`,
		`CREATE TABLE l0 (id INTEGER PRIMARY KEY AUTOINCREMENT, session TEXT NOT NULL, seq INTEGER NOT NULL,
			ts TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX idx_l0_session ON l0(session, id)`,
		`INSERT INTO l0(session, seq, role, text) VALUES ('s', 0, 'user', 'hello')`,
		`PRAGMA user_version=2`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed legacy schema (%q): %v", stmt, err)
		}
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("migrate legacy l0 -> raw: %v", err)
	}

	// Data survived into `raw`, reachable through the store API.
	recs, err := s.ReadRaw("s")
	if err != nil || len(recs) != 1 || recs[0].Text != "hello" {
		t.Fatalf("data must survive the rename: err=%v recs=%+v", err, recs)
	}
	// The legacy table is gone and the version is current.
	var hasLegacy int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='l0'`).Scan(&hasLegacy)
	if hasLegacy != 0 {
		t.Fatalf("legacy l0 table should be gone after migrate")
	}
	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

// Two record_observation calls with identical content (same obs_id) in a session
// must collapse to ONE staged row, so a retrying agent can't burn the per-session
// quota (or double-write the same active observation).
func TestStageObservationDedup(t *testing.T) {
	s := tempStore(t)
	o := Observation{ID: "dup", Session: "s", Observation: "x"}
	if err := s.StageObservation(o); err != nil {
		t.Fatal(err)
	}
	if err := s.StageObservation(o); err != nil {
		t.Fatal(err)
	}
	if got := s.StagedCount("s"); got != 1 {
		t.Fatalf("duplicate staged obs must collapse: got %d, want 1", got)
	}
}

// The per-session cap must be enforced atomically: once a session is at the cap,
// further distinct observations are refused (returns inserted=false) and the
// count stays put. (Duplicates are a separate no-op, also inserted=false.)
func TestStageObservationCap(t *testing.T) {
	s := tempStore(t)
	for i, id := range []string{"a", "b"} {
		ok, err := s.StageObservationCapped(Observation{ID: id, Session: "s", Observation: id}, 2)
		if err != nil || !ok {
			t.Fatalf("insert %d (%s) should succeed: ok=%v err=%v", i, id, ok, err)
		}
	}
	ok, err := s.StageObservationCapped(Observation{ID: "c", Session: "s", Observation: "c"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("insert past the cap should be refused (inserted=false)")
	}
	if got := s.StagedCount("s"); got != 2 {
		t.Fatalf("cap not enforced: got %d staged, want 2", got)
	}
	// A different session is unaffected by another session's cap.
	if ok, _ := s.StageObservationCapped(Observation{ID: "d", Session: "other", Observation: "d"}, 2); !ok {
		t.Fatalf("a different session should still accept observations")
	}
}

func TestObservationDedupIdempotent(t *testing.T) {
	s := tempStore(t)
	o := Observation{ID: "obs_x", Lens: LensDefault, Observation: "thinks in systems", Poignancy: 4}

	// Same obs_id twice within a batch AND across batches must collapse to one row.
	if err := s.AppendObservations([]Observation{o, o}); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if err := s.AppendObservations([]Observation{o}); err != nil {
		t.Fatalf("append again: %v", err)
	}
	got, err := s.ReadObservations("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("obs_id dedup failed: want 1 row, got %d", len(got))
	}
}

func TestEmbeddingRoundTrip(t *testing.T) {
	s := tempStore(t)
	want := []float32{0.1, -0.2, 3.5, 0, 1e-7}
	if err := s.AppendObservations([]Observation{
		{ID: "obs_e", Lens: LensDefault, Observation: "x", Embedding: want},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadObservations("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Embedding) != len(want) {
		t.Fatalf("embedding length mismatch: got %v", got)
	}
	for i := range want {
		if got[0].Embedding[i] != want[i] {
			t.Fatalf("embedding[%d] = %v, want %v", i, got[0].Embedding[i], want[i])
		}
	}
}

func TestFacetBiTemporalRoundTrip(t *testing.T) {
	s := tempStore(t)
	in := []Facet{{
		Lens: LensDefault, Dimension: "thinking", Key: "uncertainty", LastSeen: "2026-06-28T00:00:00Z",
		Versions: []FacetVersion{
			{Value: "avoids", ValidFrom: "2025-01-01T00:00:00Z", ValidTo: "2026-01-01T00:00:00Z",
				RecordedAt: "2026-01-01T00:00:00Z", BecauseOf: []string{"obs_a"}, Confidence: 0.6},
			{Value: "runs experiments", ValidFrom: "2026-01-01T00:00:00Z",
				RecordedAt: "2026-01-01T00:00:00Z", BecauseOf: []string{"obs_b", "obs_c"}, Confidence: 0.9},
		},
	}}
	if err := s.WriteFacets(in); err != nil {
		t.Fatalf("WriteFacets: %v", err)
	}
	got, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets: %v", err)
	}
	if len(got) != 1 || len(got[0].Versions) != 2 {
		t.Fatalf("want 1 facet w/ 2 versions, got %+v", got)
	}
	cur := got[0].Current()
	if cur == nil || cur.Value != "runs experiments" || cur.Confidence != 0.9 {
		t.Fatalf("Current() wrong: %+v", cur)
	}
	if got[0].Versions[0].ValidTo != "2026-01-01T00:00:00Z" {
		t.Fatalf("closed version lost its valid_to: %+v", got[0].Versions[0])
	}
	if len(cur.BecauseOf) != 2 || cur.BecauseOf[0] != "obs_b" {
		t.Fatalf("because_of not round-tripped: %v", cur.BecauseOf)
	}
}

func TestStagedCount(t *testing.T) {
	s := tempStore(t)
	if got := s.StagedCount("s"); got != 0 {
		t.Fatalf("empty: want 0, got %d", got)
	}
	_ = s.StageObservation(Observation{ID: "a", Session: "s", Observation: "x"})
	_ = s.StageObservation(Observation{ID: "b", Session: "s", Observation: "y"})
	_ = s.StageObservation(Observation{ID: "c", Session: "other", Observation: "z"})
	if got := s.StagedCount("s"); got != 2 {
		t.Fatalf("session s: want 2, got %d", got)
	}
	if got := s.StagedCount("other"); got != 1 {
		t.Fatalf("session other: want 1, got %d", got)
	}
}

func TestStatsSnapshot(t *testing.T) {
	s := tempStore(t)
	_ = s.AppendRaw(RawRecord{Session: "a", Seq: 0, Role: "user", Text: "x"})
	_ = s.AppendRaw(RawRecord{Session: "a", Seq: 1, Role: "assistant", Text: "y"})
	_ = s.AppendObservations([]Observation{{ID: "o1", Lens: LensDefault, Poignancy: 3}})

	st := s.Stats()
	if st.Sessions != 1 || st.RawRecords != 2 || st.Observations != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if st.Pending != 1 {
		t.Fatalf("session a (undistilled) should be pending: %+v", st)
	}
	// After distilling, it's no longer pending.
	_ = s.MarkDistilled("a", 2)
	if st := s.Stats(); st.Pending != 0 {
		t.Fatalf("fully-distilled session should not be pending: %+v", st)
	}
}

// DeleteObservation removes one L1 row by obs_id and reports whether it hit a
// row. This is the human "prune a wrong observation" lever — facets are
// reviewer-owned, so the only way to correct the profile is to fix the inputs.
func TestDeleteObservation(t *testing.T) {
	s := tempStore(t)
	if err := s.AppendObservations([]Observation{
		{ID: "obs_keep", Lens: LensDefault, Observation: "keep me", Poignancy: 3},
		{ID: "obs_drop", Lens: LensDefault, Observation: "prune me", Poignancy: 3},
	}); err != nil {
		t.Fatal(err)
	}

	// Deleting an existing row reports a hit and removes exactly that row.
	deleted, err := s.DeleteObservation("obs_drop")
	if err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}
	if !deleted {
		t.Fatalf("deleting an existing obs must report deleted=true")
	}
	got, _ := s.ReadObservations("")
	if len(got) != 1 || got[0].ID != "obs_keep" {
		t.Fatalf("want only obs_keep left, got %+v", got)
	}

	// Deleting a non-existent id is not an error — it just reports no hit.
	deleted, err = s.DeleteObservation("obs_nope")
	if err != nil {
		t.Fatalf("deleting a missing id must not error: %v", err)
	}
	if deleted {
		t.Fatalf("deleting a missing id must report deleted=false")
	}
}

// `witness cleanup` reclaims bulky raw transcripts (L0) for sessions with no
// activity since a cutoff, while KEEPING the derived L1/L2 (observations and
// facets are the durable archive). Pruning the whole session (raw + progress +
// meta) keeps the count-based distill watermark from ever referencing deleted
// rows.
func TestPruneSessionsBefore(t *testing.T) {
	s := tempStore(t)
	// An old, fully-distilled session with a derived observation.
	_ = s.AppendRaw(RawRecord{Session: "old", Seq: 0, TS: "2020-01-01T00:00:00Z", Role: "user", Text: "a"})
	_ = s.AppendRaw(RawRecord{Session: "old", Seq: 1, TS: "2020-01-01T00:01:00Z", Role: "assistant", Text: "b"})
	_ = s.MarkDistilled("old", 2)
	_ = s.AppendObservations([]Observation{{ID: "obs_old", Lens: LensDefault, Session: "old", Observation: "x"}})
	// A recent session that must survive.
	_ = s.AppendRaw(RawRecord{Session: "new", Seq: 0, TS: "2030-06-01T00:00:00Z", Role: "user", Text: "c"})

	cutoff := "2025-01-01T00:00:00Z"

	// Preview matches what the prune will do.
	sess, recs, err := s.RawPruneStats(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if sess != 1 || recs != 2 {
		t.Fatalf("preview: want 1 session / 2 records, got %d / %d", sess, recs)
	}

	sess, recs, err = s.PruneSessionsBefore(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if sess != 1 || recs != 2 {
		t.Fatalf("prune: want 1 session / 2 records, got %d / %d", sess, recs)
	}

	// Old session's raw L0 + watermark are gone...
	if raw, _ := s.ReadRaw("old"); len(raw) != 0 {
		t.Fatalf("old L0 should be pruned, got %d records", len(raw))
	}
	if d := s.DistilledCount("old"); d != 0 {
		t.Fatalf("old session watermark should be removed, got %d", d)
	}
	// ...but its derived observation (L1) is KEPT.
	obs, _ := s.ReadObservations("")
	if len(obs) != 1 || obs[0].ID != "obs_old" {
		t.Fatalf("L1 must survive a cleanup, got %+v", obs)
	}
	// The recent session is untouched.
	if raw, _ := s.ReadRaw("new"); len(raw) != 1 {
		t.Fatalf("recent session must survive, got %d records", len(raw))
	}
}

// Profile summaries (L4) are plain markdown files under profile/. WriteProfile /
// ReadProfile round-trip a lens's narrative; ReadProfile reports exists=false for
// a lens with no summary yet (so the CLI/MCP can show a friendly message).
func TestProfileReadWrite(t *testing.T) {
	s := tempStore(t)

	if _, ok, err := s.ReadProfile("math"); err != nil || ok {
		t.Fatalf("no summary yet: want (ok=false, nil err), got ok=%v err=%v", ok, err)
	}

	const md = "# Math\n\nRecovers from spirals with arithmetic.\n"
	if err := s.WriteProfile("math", md); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}
	got, ok, err := s.ReadProfile("math")
	if err != nil || !ok {
		t.Fatalf("ReadProfile after write: ok=%v err=%v", ok, err)
	}
	if got != md {
		t.Fatalf("round-trip mismatch: got %q", got)
	}
	// A second write overwrites (regenerated each review).
	if err := s.WriteProfile("math", "# Math v2\n"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.ReadProfile("math"); got != "# Math v2\n" {
		t.Fatalf("overwrite failed: got %q", got)
	}
}

func TestWriteFacetsReplacesAll(t *testing.T) {
	s := tempStore(t)
	_ = s.WriteFacets([]Facet{
		{Lens: LensDefault, Dimension: "d", Key: "old", Versions: []FacetVersion{{Value: "v"}}},
	})
	// A second write is a full replace, not an append (reviewer is sole writer).
	if err := s.WriteFacets([]Facet{
		{Lens: LensDefault, Dimension: "d", Key: "new", Versions: []FacetVersion{{Value: "v"}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ReadFacets()
	if len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("WriteFacets should replace, got %+v", got)
	}
}
