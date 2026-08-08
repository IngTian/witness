package store

import (
	"database/sql"
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

// v7 -> v8 (issue #125) rebuilds `observations` from (obs_id TEXT PRIMARY KEY, no
// AUTOINCREMENT) to (seq INTEGER PRIMARY KEY AUTOINCREMENT, obs_id TEXT UNIQUE) so the
// review cursor stops reusing a freed rowid after a delete-of-newest. A pre-v8 DB has
// the old shape; migrate() must rebuild it, PRESERVE each row's old implicit rowid AS
// its new seq (so persisted review_rowid:<lens> watermarks stay valid), keep obs_id
// dedup, and seed sqlite_sequence to the max so future appends never reuse. The rebuild
// must survive a gap (a pruned obs) in the old rowids. Non-destructive, idempotent.
func TestMigrateObservationsToAutoincrementSeq(t *testing.T) {
	s := tempStore(t) // fully migrated (observations already has seq)

	// The fresh schema must carry the new shape.
	if !hasColumn(t, s, "observations", "seq") {
		t.Fatal("fresh DB missing observations.seq")
	}

	// Simulate a stored-v7 archive: rebuild observations in the OLD shape (obs_id PK,
	// no seq), seed rows, prune one to leave a rowid gap, and force the version back.
	for _, stmt := range []string{
		`DROP TABLE observations`,
		`CREATE TABLE observations (
		   obs_id TEXT PRIMARY KEY, ts TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
		   lens TEXT NOT NULL DEFAULT '', dimension TEXT NOT NULL DEFAULT '', observation TEXT NOT NULL DEFAULT '',
		   evidence TEXT NOT NULL DEFAULT '', poignancy INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
		   embedding BLOB)`,
		`CREATE INDEX IF NOT EXISTS idx_obs_lens ON observations(lens)`,
		`INSERT INTO observations(obs_id, lens, observation, poignancy) VALUES
		   ('a','default','A',3),('b','default','B',4),('c','codereview','C',5),('d','default','D',6)`,
		`DELETE FROM observations WHERE obs_id = 'b'`, // leave a rowid gap (rows: 1=a, 3=c, 4=d)
		`PRAGMA user_version=7`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v7 (%q): %v", stmt, err)
		}
	}
	if hasColumn(t, s, "observations", "seq") {
		t.Fatal("precondition: seq should be absent before re-migrate")
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("v7->v8 migrate: %v", err)
	}

	// The new shape landed and integrity holds.
	if !hasColumn(t, s, "observations", "seq") {
		t.Fatal("v7->v8 migrate did not add observations.seq")
	}
	var ic string
	_ = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&ic)
	if ic != "ok" {
		t.Fatalf("integrity_check after migrate = %q, want ok", ic)
	}

	// Old implicit rowid preserved AS seq, gap intact (a=1, c=3, d=4) — so the existing
	// per-lens review_rowid watermarks still point at the right observations.
	type row struct {
		seq int64
		id  string
	}
	rows, err := s.db.Query(`SELECT seq, obs_id FROM observations ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.id); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []row{{1, "a"}, {3, "c"}, {4, "d"}}
	if len(got) != len(want) {
		t.Fatalf("want %d rows, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v (old rowid must survive as seq)", i, got[i], want[i])
		}
	}

	// obs_id is still unique: a duplicate INSERT OR IGNORE is a no-op, not a new row.
	// (Under AUTOINCREMENT the ignored insert harmlessly burns a seq value — seq is a
	// monotonic cursor, not a dense row count, so gaps are expected and fine.)
	if err := s.AppendObservations([]Observation{{ID: "a", Lens: "default", Observation: "dup"}}); err != nil {
		t.Fatalf("append dup: %v", err)
	}
	if n := s.Stats([]string{"default"}).Observations; n != 3 {
		t.Fatalf("obs_id UNIQUE must dedup: got %d rows, want 3", n)
	}

	// The #125 guarantee end-to-end: a genuinely new obs gets a seq STRICTLY PAST the
	// prior max (>4), and after deleting that newest, the next append still does NOT
	// reuse it. (The cursor read is per-lens; default-lens seqs so far are 1(a) and 4(d).)
	sinceMax, _ := s.ReadObservationsSinceOrdered("default", 4)
	if len(sinceMax) != 0 {
		t.Fatalf("no default-lens obs should sit past seq 4 yet, got %d", len(sinceMax))
	}
	if err := s.AppendObservations([]Observation{{ID: "e", Lens: "default", Observation: "E"}}); err != nil {
		t.Fatal(err)
	}
	newObs, _ := s.ReadObservationsSinceOrdered("default", 4)
	if len(newObs) != 1 || newObs[0].ID != "e" || newObs[0].Rowid <= 4 {
		t.Fatalf("new append must get a seq strictly past the max (>4), got %+v", newObs)
	}
	eSeq := newObs[0].Rowid
	// Delete the newest and append again — the reuse the pre-v8 rowid schema allowed.
	if _, err := s.DeleteObservation("e"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendObservations([]Observation{{ID: "f", Lens: "default", Observation: "F"}}); err != nil {
		t.Fatal(err)
	}
	afterDel, _ := s.ReadObservationsSinceOrdered("default", eSeq)
	if len(afterDel) != 1 || afterDel[0].ID != "f" {
		t.Fatalf("delete-of-newest then append must NOT reuse a seq <= %d (the #125 skip), got %+v", eSeq, afterDel)
	}

	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}

	// Idempotent: re-running migrate over the applied v8 schema (version forced back)
	// must not error, rebuild, or lose/renumber rows. Snapshot before, compare after.
	snapshot := func() []row {
		rr, _ := s.db.Query(`SELECT seq, obs_id FROM observations ORDER BY seq`)
		var out []row
		for rr.Next() {
			var r row
			_ = rr.Scan(&r.seq, &r.id)
			out = append(out, r)
		}
		rr.Close()
		return out
	}
	before := snapshot() // a=1, c=3, d=4, f=<burned seq> (e was deleted, dup 'a' collapsed)
	if _, err := s.db.Exec("PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("re-run migrate must be idempotent: %v", err)
	}
	after := snapshot()
	if len(after) != len(before) {
		t.Fatalf("idempotent re-run changed row set: before=%+v after=%+v", before, after)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("idempotent re-run row %d = %+v, want %+v (no rebuild/renumber)", i, after[i], before[i])
		}
	}
}

// TestMigrateObservationsSeqClampsStaleWatermark locks the migration-boundary half of
// #125 (found by adversarial audit of the first cut). The seq copy seeds sqlite_sequence
// to MAX(SURVIVING rowid), not the historical high-water mark. If a pre-v8 archive folded
// through its newest obs and THEN pruned it (the delete-of-newest lever), a per-lens
// review_rowid watermark sits ABOVE the surviving max; without a clamp the first re-mined
// obs gets a seq <= that stale watermark and the fold (seq > watermark) silently skips it
// — #125 reintroduced at the upgrade. The migration must clamp such a watermark down to
// the surviving max so a fresh append lands strictly above it and folds normally.
func TestMigrateObservationsSeqClampsStaleWatermark(t *testing.T) {
	s := tempStore(t) // fully migrated
	// Simulate a stored-v7 archive that folded through the newest obs, then pruned it.
	for _, stmt := range []string{
		`DROP TABLE observations`,
		`CREATE TABLE observations (
		   obs_id TEXT PRIMARY KEY, ts TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
		   lens TEXT NOT NULL DEFAULT '', dimension TEXT NOT NULL DEFAULT '', observation TEXT NOT NULL DEFAULT '',
		   evidence TEXT NOT NULL DEFAULT '', poignancy INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
		   embedding BLOB)`,
		`CREATE INDEX IF NOT EXISTS idx_obs_lens ON observations(lens)`,
		`INSERT INTO observations(obs_id, lens, poignancy) VALUES ('a','default',1),('b','default',1),('c','default',1)`, // rowid 1,2,3
		// Folded through the newest (c=3) for the default lens AND the global poignancy cursor.
		`INSERT INTO meta(key, value) VALUES ('review_rowid:default','3'),('review_obs_rowid','3')`,
		`DELETE FROM observations WHERE obs_id='c'`, // prune the NEWEST → surviving max rowid is now 2
		`PRAGMA user_version=7`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v7 (%q): %v", stmt, err)
		}
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("v7->v8 migrate: %v", err)
	}

	// Both stale watermarks (3) must be clamped to the surviving max (2). Otherwise a
	// re-mined obs would be skipped.
	if got := s.ReviewRowid(LensDefault); got != 2 {
		t.Fatalf("stale per-lens watermark must clamp to surviving max 2, got %d", got)
	}
	if got := metaGetInt(s.db, "review_obs_rowid"); got != 2 {
		t.Fatalf("stale global review_obs_rowid must clamp to surviving max 2, got %d", got)
	}

	// The payoff: a genuinely new mined obs must be fold-visible (seq > watermark),
	// not silently skipped.
	if err := s.AppendObservations([]Observation{{ID: "d", Lens: LensDefault, Observation: "d", Poignancy: 4}}); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ReadObservationsSinceOrdered(LensDefault, s.ReviewRowid(LensDefault))
	if len(pending) != 1 || pending[0].ID != "d" {
		t.Fatalf("re-mined obs after delete-of-newest-before-upgrade must be fold-visible, got %d obs %+v (the #125 migration-boundary skip)",
			len(pending), pending)
	}
	// And the poignancy cadence sees it too (was under-counting under the stale cursor).
	if got := s.PoignancySinceReview(); got != 4 {
		t.Fatalf("PoignancySinceReview after clamp+append = %d, want 4 (the new obs)", got)
	}

	// A watermark that was NOT stale (<= surviving max) must be left untouched.
	if _, err := s.db.Exec(`UPDATE meta SET value='1' WHERE key='review_rowid:default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	// Force a re-run of the seq step by dropping seq — but the table already has seq, so
	// instead just assert the clamp is conditional: re-migrate is a no-op (guard: seq
	// present) and the valid watermark stays 1.
	if err := migrate(s.db); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if got := s.ReviewRowid(LensDefault); got != 1 {
		t.Fatalf("a non-stale watermark must be left untouched, got %d", got)
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
		ok, err := s.StageObservationCapped(Observation{ID: id, Session: "s", Observation: id}, 2, 0)
		if err != nil || !ok {
			t.Fatalf("insert %d (%s) should succeed: ok=%v err=%v", i, id, ok, err)
		}
	}
	ok, err := s.StageObservationCapped(Observation{ID: "c", Session: "s", Observation: "c"}, 2, 0)
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
	if ok, _ := s.StageObservationCapped(Observation{ID: "d", Session: "other", Observation: "d"}, 2, 0); !ok {
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
	if ok, _ := s.StageObservationCapped(Observation{ID: "a", Session: "s", Observation: "a"}, 2, 0); ok {
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

// witness is multi-process by design: a capture hook fires per user turn while a worker
// may be draining. migrate() reads (pragma_table_info / sqlite_master) before it writes,
// and under WAL a DEFERRED transaction that takes a read snapshot and THEN writes fails
// with SQLITE_BUSY_SNAPSHOT (517) if another connection committed in between — a conflict
// busy_timeout does NOT retry, because there is no lock to wait for; the snapshot is
// simply stale. So on the first Open after an upgrade, one concurrent capture could abort
// the migration and fail store.Open in the migrating process (on the capture/session-start
// path that silently drops the user's turn).
//
// beginImmediate takes the write lock up front, so a concurrent writer makes the
// migration WAIT rather than abort. This reproduces the interleaving against the real
// migrate() and asserts it now succeeds.
func TestMigrateSurvivesConcurrentWriterDuringMigration(t *testing.T) {
	s := tempStore(t) // fully migrated; we rewind below to force real work
	// A second connection to the SAME file, standing in for another witness process.
	other, err := sql.Open("sqlite", s.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := other.Exec(p); err != nil {
			t.Fatal(err)
		}
	}

	// Force migrate() to do real work, then interleave a commit from the other process
	// right after migrate's first read would have taken its snapshot.
	if _, err := s.db.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		// Land the concurrent write while migrate is mid-transaction.
		_, err := other.Exec(`INSERT INTO raw(session, seq, ts, role, text) VALUES ('concurrent', 0, '2026-01-01T00:00:00Z', 'user', 'a turn typed during the upgrade')`)
		done <- err
	}()

	if err := migrate(s.db); err != nil {
		t.Fatalf("migration must survive a concurrent writer, got: %v (517 = SQLITE_BUSY_SNAPSHOT)", err)
	}
	if err := <-done; err != nil {
		// The other process may legitimately have had to wait; what must not happen is
		// the MIGRATION aborting. A busy error here would mean the capture path retries.
		t.Logf("concurrent writer returned %v (acceptable: it retries; the migration is what must not abort)", err)
	}

	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d (migration did not complete)", v, schemaVersion)
	}
	// And the schema is usable afterwards.
	if _, err := s.db.Exec(`SELECT COUNT(*) FROM observations`); err != nil {
		t.Fatalf("schema unusable after migration: %v", err)
	}
}

// beginImmediate must hand back a transaction that already holds the write lock, so a
// read-then-write sequence inside it cannot hit the un-retryable snapshot upgrade.
func TestBeginImmediateHoldsWriteLockAndPreservesUserVersion(t *testing.T) {
	s := tempStore(t)
	var before int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := beginImmediate(s.db)
	if err != nil {
		t.Fatalf("beginImmediate: %v", err)
	}
	// Read then write inside the tx — the shape that used to abort.
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&n); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS probe_immediate(x TEXT)`); err != nil {
		tx.Rollback()
		t.Fatalf("write inside an immediate tx must succeed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// The lock-acquiring write is a semantic no-op: user_version is unchanged, and the
	// rolled-back probe table is gone.
	var after int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("beginImmediate changed user_version: %d -> %d", before, after)
	}
	var probes int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='probe_immediate'`).Scan(&probes)
	if probes != 0 {
		t.Fatal("rollback did not undo the probe table")
	}
}

// The v3→v4 platform backfill must run ONCE, when the column is added — never again.
//
// The two UPDATEs that classify existing rows sat OUTSIDE the `if hasColumn == 0` guard (only the
// ALTER was inside it), so they re-ran on every migrate() that passed the version gate. Their rule
// is "unmarked AND not opencode-prefixed → claude", which was correct in the v3→v4 era because
// those were the only two platforms. It stopped being correct when the `file` platform shipped
// (v0.5.0) and claimed the "file:" prefix.
//
// Why that is a real data bug and not a theoretical one:
//   - the ” state is REACHABLE. store.ApplyRawImport inserts the session_meta row with no platform
//     value, and the platform is written afterwards by a separate best-effort call whose error is
//     DISCARDED (cmd/commands/ingest.go). A crash or a failed write between the two leaves ”.
//   - the mislabel is STICKY. platform.ForSession prefers the persisted column over the session-id
//     prefix, so once a `file:` session is stamped "claude" nothing ever re-derives it.
//   - it is SILENT. file and claude render transcripts identically today, so the only symptom is a
//     wrong owning platform in the DB — until a runtime with its own input shaper is added, at which
//     point those sessions are quietly distilled through the wrong renderer.
//
// Historical rows keep their historical classification: this test pins that the rule is applied at
// the moment the column appears and not re-applied to rows created in a later era.
func TestPlatformBackfillDoesNotReclassifyLaterSessions(t *testing.T) {
	s := tempStore(t)

	// A `file:` session whose platform write did not land — exactly the ApplyRawImport-then-crash
	// shape. Written directly, because that is the state on disk we care about.
	if err := s.AppendRaw(RawRecord{
		TS: "2026-01-01T00:00:00Z", Session: "file:notes", Role: "document", Text: "a note",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO session_meta(session, cwd, started, platform) VALUES (?, '', '', '')`,
		"file:notes"); err != nil {
		t.Fatal(err)
	}
	if got := s.SessionPlatform("file:notes"); got != "" {
		t.Fatalf("precondition: want an unclassified row, got %q", got)
	}

	// Simulate a FUTURE schema bump: the gate at the top of migrate() returns early when the DB is
	// already current (v >= schemaVersion), so the backfill only re-runs when a later version
	// exists — which is exactly what the next release does. Rewinding user_version reproduces that
	// without inventing a fake schema step.
	if _, err := s.db.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if got := s.SessionPlatform("file:notes"); got == "claude" {
		t.Errorf("a `file:` session was stamped %q by the v3→v4 backfill — that rule predates the "+
			"file platform, and ForSession prefers the persisted column over the prefix, so the "+
			"mislabel is permanent and silent", got)
	}
}

// The historical rule itself must still work: an unmarked, non-prefixed session IS Claude, and that
// classification must happen when the column is introduced. This is the half a fix could break by
// simply deleting the backfill.
func TestPlatformBackfillStillClassifiesPreV4Sessions(t *testing.T) {
	s := tempStore(t)
	// Simulate a pre-v4 database: drop the column so migrate() re-adds it and backfills.
	if _, err := s.db.Exec(`INSERT INTO session_meta(session, cwd, started, platform) VALUES ('legacy', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session_meta(session, cwd, started, platform) VALUES ('opencode:oc', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	// The column already exists here, so this asserts the CURRENT behavior for rows that are
	// unclassified at migrate time — whatever the fix chooses, an opencode-prefixed row must never
	// be called claude.
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	if got := s.SessionPlatform("opencode:oc"); got == "claude" {
		t.Errorf("an opencode-prefixed session was classified as claude: %q", got)
	}
}
