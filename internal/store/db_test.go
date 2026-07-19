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

// v3 -> v4 adds session_meta.platform. A pre-v4 DB has session_meta WITHOUT the
// column; migrate() must ADD it and BACKFILL from the L0 session-id prefix
// ("opencode:" => opencode, else claude), non-destructively and idempotently.
func TestMigrateAddsAndBackfillsSessionPlatform(t *testing.T) {
	s := tempStore(t) // fully migrated (has the column)
	// Simulate a v3 DB: drop the column by rebuilding session_meta without it, seed
	// rows, and set the version back to 3.
	for _, stmt := range []string{
		`DROP TABLE session_meta`,
		`CREATE TABLE session_meta (session TEXT PRIMARY KEY, cwd TEXT NOT NULL DEFAULT '', started TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO session_meta(session) VALUES ('opencode:abc')`,
		`INSERT INTO session_meta(session) VALUES ('plain-cc-session')`,
		`PRAGMA user_version=3`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v3 (%q): %v", stmt, err)
		}
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("v3->v4 migrate: %v", err)
	}

	if got := s.SessionPlatform("opencode:abc"); got != "opencode" {
		t.Fatalf("opencode-prefixed row: platform=%q, want opencode", got)
	}
	if got := s.SessionPlatform("plain-cc-session"); got != "claude" {
		t.Fatalf("unprefixed row: platform=%q, want claude", got)
	}
	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}

	// Idempotent: re-running migrate over the applied v4 schema must not error or
	// reclassify. Force the version back so migrate() actually re-executes the step.
	if _, err := s.db.Exec("PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
	// A value a newer binary wrote must NOT be clobbered by the backfill.
	s.SetSessionPlatform("opencode:abc", "claude") // deliberately "wrong" to prove the backfill won't overwrite a set value
	if err := migrate(s.db); err != nil {
		t.Fatalf("re-run migrate: %v", err)
	}
	if got := s.SessionPlatform("opencode:abc"); got != "claude" {
		t.Fatalf("backfill must not overwrite an already-set value: got %q", got)
	}
}

// v4 -> v5 re-keys progress from PK(session) to PK(session, lens) (issue #55). A
// pre-v5 DB has the old single-key `progress`; migrate() must rebuild it, preserve
// each row's watermark AS THE 'default' LENS (the lens it actually reflects), and
// leave every other lens absent so it reads as pending. Non-destructive, idempotent.
func TestMigrateProgressToPerLens(t *testing.T) {
	s := tempStore(t) // fully migrated (progress has the lens column)
	// Simulate a v4 DB: rebuild progress WITHOUT the lens column, seed a watermark,
	// and set the version back to 4.
	for _, stmt := range []string{
		`DROP TABLE progress`,
		`CREATE TABLE progress (session TEXT PRIMARY KEY, distilled INTEGER NOT NULL DEFAULT 0,
			retries INTEGER NOT NULL DEFAULT 0, distilled_at TEXT NOT NULL DEFAULT '', next_attempt TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO progress(session, distilled, retries, distilled_at) VALUES ('sess', 5, 2, '2026-07-01T00:00:00Z')`,
		`PRAGMA user_version=4`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v4 (%q): %v", stmt, err)
		}
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("v4->v5 migrate: %v", err)
	}

	// The old row survived as the 'default' lens's watermark, values intact.
	if got := s.DistilledCount("sess", LensDefault); got != 5 {
		t.Fatalf("default lens watermark: got %d, want 5 (preserved from v4)", got)
	}
	if got := s.RetryCount("sess", LensDefault); got != 2 {
		t.Fatalf("default lens retries: got %d, want 2 (preserved)", got)
	}
	// A DIFFERENT lens must read as never-mined (absent → 0), so it backfills.
	if got := s.DistilledCount("sess", "codereview"); got != 0 {
		t.Fatalf("a non-default lens must start un-mined, got %d", got)
	}
	// The lens column exists and PK is (session, lens): the same session can hold two
	// independent lens rows.
	if err := s.MarkDistilled("sess", "codereview", 3); err != nil {
		t.Fatalf("MarkDistilled second lens: %v", err)
	}
	if got := s.DistilledCount("sess", "codereview"); got != 3 {
		t.Fatalf("codereview watermark after mark: got %d, want 3", got)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 5 {
		t.Fatalf("default watermark must be untouched by the second lens: got %d, want 5", got)
	}
	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}

	// Idempotent: re-running migrate over the applied v5 schema (version forced back)
	// must not error or lose the rows.
	if _, err := s.db.Exec("PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("re-run migrate must be idempotent: %v", err)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 5 {
		t.Fatalf("default watermark lost on idempotent re-run: got %d", got)
	}
	if got := s.DistilledCount("sess", "codereview"); got != 3 {
		t.Fatalf("codereview watermark lost on idempotent re-run: got %d", got)
	}
}

// hasIndex reports whether a named index exists in sqlite_master.
func hasIndex(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("probe index %q: %v", name, err)
	}
	return n > 0
}

// v6 (issue #73-S3) adds two hot-path progress indexes. Because they live in
// schemaV1 as CREATE INDEX IF NOT EXISTS, a fresh DB has them AND a stored-v5 DB
// re-runs the (idempotent) schema apply on the version bump to gain them — the
// whole point of bumping schemaVersion 5->6. A stored-v6 DB would early-return, so
// the version bump is load-bearing: without it these never reach existing archives.
func TestMigrateAddsProgressIndexes(t *testing.T) {
	s := tempStore(t) // fully migrated
	for _, idx := range []string{"idx_progress_lens_next", "idx_progress_distilled_at"} {
		if !hasIndex(t, s, idx) {
			t.Fatalf("fresh DB missing hot-path index %q", idx)
		}
	}

	// Simulate a stored-v5 archive: drop the indexes and force the version back. A
	// re-migrate must recreate them (proving the version bump lands the additive
	// schema on existing archives, not just fresh ones).
	for _, stmt := range []string{
		`DROP INDEX idx_progress_lens_next`,
		`DROP INDEX idx_progress_distilled_at`,
		`PRAGMA user_version=5`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v5 (%q): %v", stmt, err)
		}
	}
	if hasIndex(t, s, "idx_progress_lens_next") {
		t.Fatal("precondition: index should be dropped before re-migrate")
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("v5->v6 migrate: %v", err)
	}
	for _, idx := range []string{"idx_progress_lens_next", "idx_progress_distilled_at"} {
		if !hasIndex(t, s, idx) {
			t.Fatalf("v5->v6 migrate did not create %q", idx)
		}
	}
	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

// hasColumn reports whether a named column exists on a table.
func hasColumn(t *testing.T, s *Store, table, col string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col).Scan(&n); err != nil {
		t.Fatalf("probe %s.%s: %v", table, col, err)
	}
	return n > 0
}

// v6 -> v7 (issue #69 Part 2) adds progress.drift_at. A pre-v7 DB has progress
// WITHOUT the column; migrate() must ADD it (guarded ALTER), leave existing rows at
// the empty-string default (never drifted), and preserve their data. Non-destructive, idempotent.
func TestMigrateAddsDriftColumn(t *testing.T) {
	s := tempStore(t) // fully migrated (progress has drift_at)
	if !hasColumn(t, s, "progress", "drift_at") {
		t.Fatal("fresh DB missing progress.drift_at")
	}

	// Simulate a stored-v6 archive: rebuild progress WITHOUT drift_at (but WITH the
	// v5 lens column so migrateProgressPerLens stays a no-op), seed a watermark row,
	// and force the version back to 6 so migrate() re-runs the v7 step.
	for _, stmt := range []string{
		`DROP TABLE progress`,
		`CREATE TABLE progress (session TEXT NOT NULL, lens TEXT NOT NULL DEFAULT 'default',
			distilled INTEGER NOT NULL DEFAULT 0, retries INTEGER NOT NULL DEFAULT 0,
			distilled_at TEXT NOT NULL DEFAULT '', next_attempt TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (session, lens))`,
		`INSERT INTO progress(session, lens, distilled, retries) VALUES ('sess', 'default', 5, 1)`,
		`PRAGMA user_version=6`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v6 (%q): %v", stmt, err)
		}
	}
	if hasColumn(t, s, "progress", "drift_at") {
		t.Fatal("precondition: drift_at should be absent before re-migrate")
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("v6->v7 migrate: %v", err)
	}

	// Column added, the pre-existing row survived with the '' (never-drifted) default,
	// and its other watermark fields are intact.
	if !hasColumn(t, s, "progress", "drift_at") {
		t.Fatal("v6->v7 migrate did not add progress.drift_at")
	}
	if got := s.DriftAt("sess", LensDefault); got != "" {
		t.Fatalf("a migrated row must default to '' (never drifted), got %q", got)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 5 {
		t.Fatalf("watermark lost across v7 migrate: got %d, want 5", got)
	}
	if got := s.RetryCount("sess", LensDefault); got != 1 {
		t.Fatalf("retries lost across v7 migrate: got %d, want 1", got)
	}
	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}

	// Idempotent: re-running migrate over the applied v7 schema (version forced back)
	// must not error, re-add, or lose the row.
	if _, err := s.db.Exec("PRAGMA user_version=6"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("re-run migrate must be idempotent: %v", err)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 5 {
		t.Fatalf("watermark lost on idempotent re-run: got %d", got)
	}
}

// A pre-v5 DB (single-key progress) migrated all the way to v7 must land the SAME
// table shape as a fresh DB — including drift_at, defaulted to empty for the copied
// 'default'-lens rows (progress_v4 predates drift, so the rebuild seeds an empty stamp).
func TestMigrateV4ToV7SeedsDriftAtBlank(t *testing.T) {
	s := tempStore(t)
	for _, stmt := range []string{
		`DROP TABLE progress`,
		`CREATE TABLE progress (session TEXT PRIMARY KEY, distilled INTEGER NOT NULL DEFAULT 0,
			retries INTEGER NOT NULL DEFAULT 0, distilled_at TEXT NOT NULL DEFAULT '', next_attempt TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO progress(session, distilled) VALUES ('sess', 5)`,
		`PRAGMA user_version=4`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v4 (%q): %v", stmt, err)
		}
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("v4->v7 migrate: %v", err)
	}
	if !hasColumn(t, s, "progress", "drift_at") {
		t.Fatal("v4->v7 migrate must land drift_at (fresh-DB parity)")
	}
	if got := s.DriftAt("sess", LensDefault); got != "" {
		t.Fatalf("rebuilt default-lens row must seed drift_at='', got %q", got)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 5 {
		t.Fatalf("watermark preserved across v4->v7: got %d, want 5", got)
	}
}

// SetSessionPlatform upserts even when no session_meta row exists yet (CC sessions
// have none until now), and SessionPlatform reads it back.
func TestSetSessionPlatformUpsert(t *testing.T) {
	s := tempStore(t)
	if got := s.SessionPlatform("nope"); got != "" {
		t.Fatalf("absent session: want empty, got %q", got)
	}
	s.SetSessionPlatform("s1", "opencode") // no prior row -> INSERT
	if got := s.SessionPlatform("s1"); got != "opencode" {
		t.Fatalf("insert path: got %q", got)
	}
	s.SetSessionPlatform("s1", "claude") // existing row -> UPDATE column only
	if got := s.SessionPlatform("s1"); got != "claude" {
		t.Fatalf("update path: got %q", got)
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

	// StagedExists disambiguates the two not-inserted reasons (issue #54 minor):
	// a DUPLICATE at the cap must be distinguishable from hitting the cap with a
	// genuinely new obs, so the caller can report "already recorded" not "too many".
	if !s.StagedExists("s", "a") {
		t.Fatalf("StagedExists should see an already-staged obs")
	}
	if s.StagedExists("s", "c") {
		t.Fatalf("StagedExists must be false for an obs rejected by the cap (never staged)")
	}
	// Re-staging an existing id while the session is AT the cap: still not inserted,
	// but it's a dedup, not a quota breach — StagedExists tells them apart.
	if ok, _ := s.StageObservationCapped(Observation{ID: "a", Session: "s", Observation: "a"}, 2); ok {
		t.Fatalf("re-staging a duplicate must not insert")
	}
	if !s.StagedExists("s", "a") {
		t.Fatalf("the duplicate id is still present (a dedup, not a cap error)")
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

	st := s.Stats([]string{LensDefault})
	if st.Sessions != 1 || st.RawRecords != 2 || st.Observations != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if st.Pending != 1 {
		t.Fatalf("session a (undistilled) should be pending: %+v", st)
	}
	// After distilling, it's no longer pending.
	_ = s.MarkDistilled("a", LensDefault, 2)
	if st := s.Stats([]string{LensDefault}); st.Pending != 0 {
		t.Fatalf("fully-distilled session should not be pending: %+v", st)
	}
}

// ResetLensWatermark drops ONLY the named lens's progress rows (the backfill path,
// #55) — other lenses' watermarks must survive so they're never re-mined.
func TestResetLensWatermark(t *testing.T) {
	s := tempStore(t)
	_ = s.MarkDistilled("s1", LensDefault, 4)
	_ = s.MarkDistilled("s2", LensDefault, 6)
	_ = s.MarkDistilled("s1", "codereview", 4)

	n, err := s.ResetLensWatermark("codereview")
	if err != nil {
		t.Fatalf("ResetLensWatermark: %v", err)
	}
	if n != 1 {
		t.Fatalf("should have removed 1 codereview row, got %d", n)
	}
	// codereview reset to absent (0); default untouched on both sessions.
	if got := s.DistilledCount("s1", "codereview"); got != 0 {
		t.Fatalf("codereview watermark should be reset, got %d", got)
	}
	if got := s.DistilledCount("s1", LensDefault); got != 4 {
		t.Fatalf("default watermark must survive a codereview reset, got %d", got)
	}
	if got := s.DistilledCount("s2", LensDefault); got != 6 {
		t.Fatalf("unrelated session's default watermark must survive, got %d", got)
	}
}

// DeleteLensData removes one lens's observations + facets (rebuild path, #55),
// leaving other lenses' derived data and all raw L0 intact.
func TestDeleteLensData(t *testing.T) {
	s := tempStore(t)
	_ = s.AppendRaw(RawRecord{Session: "s", Seq: 0, Role: "user", Text: "x"})
	_ = s.AppendObservations([]Observation{
		{ID: "o_def", Lens: LensDefault, Observation: "d", Poignancy: 3},
		{ID: "o_cr1", Lens: "codereview", Observation: "c1", Poignancy: 3},
		{ID: "o_cr2", Lens: "codereview", Observation: "c2", Poignancy: 3},
	})
	if err := s.WriteFacets([]Facet{
		{Lens: LensDefault, Dimension: "thinking", Key: "k", Versions: []FacetVersion{{Value: "v", ValidFrom: "t"}}},
		{Lens: "codereview", Dimension: "rule", Key: "r", Versions: []FacetVersion{{Value: "v", ValidFrom: "t"}}},
	}); err != nil {
		t.Fatal(err)
	}

	obs, facets, err := s.DeleteLensData("codereview")
	if err != nil {
		t.Fatalf("DeleteLensData: %v", err)
	}
	if obs != 2 || facets != 1 {
		t.Fatalf("should drop 2 obs + 1 facet, got %d obs + %d facets", obs, facets)
	}
	// codereview data gone; default data + raw survive.
	if got, _ := s.ReadObservations("codereview"); len(got) != 0 {
		t.Fatalf("codereview obs should be gone, got %d", len(got))
	}
	if got, _ := s.ReadObservations(LensDefault); len(got) != 1 {
		t.Fatalf("default obs must survive, got %d", len(got))
	}
	all, _ := s.ReadFacets()
	for _, f := range all {
		if f.Lens == "codereview" {
			t.Fatalf("codereview facet should be gone: %+v", f)
		}
	}
	if recs, _ := s.ReadRaw("s"); len(recs) != 1 {
		t.Fatalf("raw L0 must be untouched by a lens rebuild, got %d", len(recs))
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
	_ = s.MarkDistilled("old", LensDefault, 2)
	_ = s.AppendObservations([]Observation{{ID: "obs_old", Lens: LensDefault, Session: "old", Observation: "x"}})
	// A recent session that must survive.
	_ = s.AppendRaw(RawRecord{Session: "new", Seq: 0, TS: "2030-06-01T00:00:00Z", Role: "user", Text: "c"})

	// Per-session state parked in `meta` under "<namespace>:<session>" (as opencode's
	// import bookkeeping does) must be reclaimed with the session — otherwise it leaks
	// (issue #54 minor). A global meta row and another session's row must survive.
	_ = s.SetMetaString("opencode_import_keys:old", `["k1","k2"]`)
	_ = s.SetMetaString("opencode_import_keys:new", `["k3"]`)
	_ = s.SetMetaString("review_ts", "2024-01-01T00:00:00Z")

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
	if d := s.DistilledCount("old", LensDefault); d != 0 {
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

	// The pruned session's per-session meta row is reclaimed; the surviving
	// session's row and the global row are untouched.
	if got := s.MetaString("opencode_import_keys:old"); got != "" {
		t.Fatalf("pruned session's per-session meta row should be removed, got %q", got)
	}
	if got := s.MetaString("opencode_import_keys:new"); got != `["k3"]` {
		t.Fatalf("surviving session's meta row must be kept, got %q", got)
	}
	if got := s.MetaString("review_ts"); got != "2024-01-01T00:00:00Z" {
		t.Fatalf("a global meta row must never be touched by prune, got %q", got)
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
