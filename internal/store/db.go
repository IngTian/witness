package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver (CGO_ENABLED=0), registered as "sqlite"
)

// schemaVersion is the current on-disk schema, written to PRAGMA user_version so
// a future binary can detect and migrate older databases instead of silently
// misreading them. History: two earlier dev-only versions existed (the
// staged-uniqueness index from v2 is folded into schemaV1 as a plain CREATE ... IF
// NOT EXISTS, and the v2->v3 `l0`->`raw` rename is a guarded legacy step in
// migrate()); v4 adds session_meta.platform (the per-session owning-platform axis,
// issue #21) via a guarded ADD COLUMN + one-time prefix backfill in migrate();
// v5 re-keys the distillation watermark from PK(session) to PK(session, lens) so a
// single lens can be backfilled without re-mining every lens (issue #55) — a
// guarded table rebuild in migrate() that seeds the pre-migration rows as the
// 'default' lens's watermark (the lens they actually reflect); v6 adds two
// hot-path indexes on the progress table (idx_progress_lens_next,
// idx_progress_distilled_at, issue #73-S3) via addProgressIndexes — a guarded,
// idempotent migrate() step that runs AFTER the v5 rebuild (the indexes reference
// the `lens` column that rebuild creates; putting them in schemaV1 would fail on a
// pre-v5 DB and the rebuild's DROP TABLE would drop them anyway); v7 adds
// progress.drift_at (issue #69 Part 2), an RFC3339 stamp of a per-(session,lens)
// prose-drift so drift survives across worker runs and is queryable (empty = never
// drifted) via a guarded ADD COLUMN in migrate() (addDriftColumn) — this persists
// the previously in-memory-only LensMining.Drifted signal; v8 rebuilds observations
// from (obs_id TEXT PRIMARY KEY, no AUTOINCREMENT) to (seq INTEGER PRIMARY KEY
// AUTOINCREMENT, obs_id TEXT UNIQUE NOT NULL) via a guarded table rebuild in migrate()
// (migrateObservationsSeq) that copies each row's old implicit rowid INTO the new seq
// (so persisted review_rowid:<lens> watermarks stay valid) and clamps any watermark left
// above the surviving max down to it. Without AUTOINCREMENT SQLite REUSES a freed rowid
// after the max-rowid row is deleted, so a delete-of-newest + re-mine could land a new
// obs at a rowid <= a lens's review watermark and be silently skipped by the windowed
// fold (issue #125); AUTOINCREMENT makes the fold cursor strictly monotonic so a fresh
// obs always sorts after the watermark.
const schemaVersion = 8

func (s *Store) dbPath() string { return filepath.Join(s.Root, "witness.db") }

// openDB opens (creating if absent) the SQLite database, applies connection
// pragmas, and migrates the schema up to schemaVersion.
//
// MaxOpenConns(1): every witness invocation is a short-lived process doing a
// small amount of work; serializing to a single connection sidesteps SQLite
// "database is locked" churn entirely. WAL + busy_timeout still let separate
// processes (capture vs. a running worker) interleave safely.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",  // wait on a contended write rather than erroring
		"PRAGMA journal_mode=WAL",   // concurrent readers + one writer across processes
		"PRAGMA foreign_keys=ON",    // enforce facet_versions -> facets ON DELETE CASCADE
		"PRAGMA synchronous=NORMAL", // safe under WAL; far less fsync cost than FULL
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// Defense in depth: the root dir is 0700, but tighten the DB file (and its WAL
	// sidecars, created by now) to 0600 so the personal profile is never group/world
	// readable even if the directory perms are ever loosened.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return db, nil
}

// Export writes a consistent, single-file snapshot of the archive database to
// dst via SQLite `VACUUM INTO`. The snapshot folds the WAL into one plain .db
// file with NO -wal/-shm sidecars, so it is safe to copy or hand to a cloud
// syncer — unlike the live data dir, where a syncer racing the WAL can corrupt
// the DB. VACUUM INTO reads a consistent view even while the worker is writing,
// so no worker-stop is needed. The snapshot is itself a normal witness.db: to
// restore, stop witness and copy it into the data dir (or point WITNESS_HOME at
// its folder).
//
// Scope: this snapshots the DATABASE only — L0 raw, L1 observations, L2 facets,
// config, and the distill queue, i.e. the source of truth. The L4 narrative
// profile (profile/*.md) is a DERIVED cache regenerated from L2 facets, so it is
// deliberately NOT exported; after a restore, `witness review` rebuilds it.
//
// dst must not already exist (VACUUM INTO requires a fresh path, and refusing to
// overwrite avoids clobbering a prior backup); callers pass force to remove an
// existing file first.
func (s *Store) Export(dst string, force bool) error {
	if dst == "" {
		return fmt.Errorf("export: destination path is required")
	}
	if _, err := os.Stat(dst); err == nil {
		if !force {
			return fmt.Errorf("export: %s already exists (use --force to overwrite)", dst)
		}
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("export: remove existing %s: %w", dst, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("export: stat %s: %w", dst, err)
	}
	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("export: mkdir %s: %w", dir, err)
		}
	}
	// VACUUM INTO takes the destination as a bound string parameter.
	if _, err := s.db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("export: vacuum into %s: %w", dst, err)
	}
	// The snapshot carries the same private growth data; keep it 0600.
	_ = os.Chmod(dst, 0o600)
	return nil
}

// migrate brings the schema from the database's current user_version up to
// schemaVersion: a guarded legacy rename (if needed) followed by the full current
// schema applied idempotently.
//
// The whole thing plus the version bump runs in ONE transaction (PRAGMA
// user_version is transactional in SQLite), so a crash mid-migration rolls back
// cleanly: the DB is either fully at the new version or fully at the old one,
// never half-applied. The schema is all CREATE ... IF NOT EXISTS and is applied
// unconditionally, so re-runs and any starting version converge harmlessly.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v >= schemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// 1. Legacy rename FIRST. A pre-rename DB has the raw layer under its old name
	// `l0` and no `raw`; rename it in place (preserving the data) before step 2 would
	// otherwise create an empty `raw` beside it. Guarded on `l0` present AND `raw`
	// absent, so it's a no-op on fresh DBs and on any re-run. ALTER TABLE RENAME has
	// no IF EXISTS form, hence the explicit existence checks.
	var hasL0, hasRaw int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='l0'`).Scan(&hasL0); err != nil {
		tx.Rollback()
		return fmt.Errorf("check legacy l0: %w", err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='raw'`).Scan(&hasRaw); err != nil {
		tx.Rollback()
		return fmt.Errorf("check raw: %w", err)
	}
	if hasL0 > 0 && hasRaw == 0 {
		if _, err := tx.Exec(legacyL0Rename); err != nil {
			tx.Rollback()
			return fmt.Errorf("rename legacy l0 -> raw: %w", err)
		}
	}

	// 2. Apply the full current schema UNCONDITIONALLY. Every statement is
	// CREATE ... IF NOT EXISTS, so it creates everything on a fresh DB and is a
	// harmless no-op on an existing one. Applying it regardless of the stored
	// version means a DB at ANY prior version converges to the current schema —
	// there is no "version landed between steps, so a CREATE got skipped" trap.
	// Future additive changes extend schemaV1; future ALTER/data migrations go
	// above this as their own guarded, idempotent step (like the rename).
	if _, err := tx.Exec(schemaV1); err != nil {
		tx.Rollback()
		return fmt.Errorf("apply schema: %w", err)
	}

	// 3. v3 -> v4: session_meta.platform (the per-session owning-platform axis).
	// A pre-v4 DB has session_meta WITHOUT this column; schemaV1's CREATE ... IF
	// NOT EXISTS won't alter an existing table, so add it explicitly (SQLite has no
	// ADD COLUMN IF NOT EXISTS — guard on pragma_table_info), then BACKFILL from the
	// L0 session-id prefix: "opencode:"-prefixed sessions are OpenCode, everything
	// else is Claude (the same asymmetric rule ForSession uses). Idempotent: the
	// guard skips the ALTER once the column exists, and the backfill only writes
	// rows still at '' so a re-run is a no-op. Fresh DBs already have the column
	// (schemaV1) and no rows, so both steps are no-ops there.
	if err := addSessionPlatformColumn(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("v4 add session_meta.platform: %w", err)
	}

	// 4. v4 -> v5: re-key progress from PK(session) to PK(session, lens) so a lens can
	// be backfilled independently (issue #55). schemaV1's CREATE ... IF NOT EXISTS
	// won't reshape an existing table, so rebuild explicitly when the running DB's
	// progress predates the lens column. Idempotent: the guard skips once `lens`
	// exists (fresh DBs already have it from schemaV1; a re-run sees it too).
	if err := migrateProgressPerLens(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("v5 per-lens progress: %w", err)
	}

	// 5. v5 -> v6: hot-path indexes on the (session, lens)-keyed progress table
	// (issue #73-S3). MUST come AFTER step 4: they reference the `lens` column, which
	// on a pre-v5 DB only exists once migrateProgressPerLens has rebuilt the table
	// (and that rebuild's DROP TABLE would drop indexes created earlier anyway).
	// Idempotent CREATE INDEX IF NOT EXISTS, so it's a no-op once they exist.
	if err := addProgressIndexes(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("v6 progress indexes: %w", err)
	}

	// 6. v6 -> v7: progress.drift_at (issue #69 Part 2) — persist the last
	// per-(session,lens) prose-drift stamp so it survives worker restarts instead of
	// living only in the in-memory LensMining.Drifted flag. schemaV1's CREATE ... IF
	// NOT EXISTS won't alter an existing progress table, so add the column explicitly
	// (SQLite has no ADD COLUMN IF NOT EXISTS — guard on pragma_table_info like the v4
	// platform column). Runs AFTER migrateProgressPerLens so the table it targets is
	// already the per-lens shape (a pre-v5 rebuild would drop a column added earlier).
	// Idempotent: the guard skips the ALTER once the column exists; fresh DBs already
	// have it from schemaV1.
	if err := addDriftColumn(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("v7 add progress.drift_at: %w", err)
	}

	// 7. v7 -> v8: rebuild observations with a monotonic AUTOINCREMENT surrogate key
	// (issue #125). A pre-v8 DB has observations keyed by obs_id (no seq column), whose
	// implicit rowid SQLite reuses after a delete-of-newest; schemaV1's CREATE ... IF
	// NOT EXISTS won't reshape the existing table (PK change is not an ALTER), so rebuild
	// explicitly, copying each old rowid INTO the new seq so review watermarks stay valid.
	// Guarded on the seq column's absence, so it's a no-op on fresh DBs (schemaV1 shape)
	// and on any re-run. MUST come last: it references only its own table.
	if err := migrateObservationsSeq(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("v8 observations seq: %w", err)
	}

	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		tx.Rollback()
		return fmt.Errorf("set user_version: %w", err)
	}
	return tx.Commit()
}

// addSessionPlatformColumn is the v3->v4 step: add session_meta.platform if the
// running DB's table predates it, then backfill from the L0 session-id prefix.
//
// The prefix literal "opencode:" is duplicated from internal/platform here on
// purpose: internal/store must NOT import internal/platform (platform imports
// store; the reverse would cycle). This is the one backfill rule and it is frozen
// history once written, so a literal is correct — a future prefix change does not
// retroactively re-key already-backfilled rows.
func addSessionPlatformColumn(tx *sql.Tx) error {
	var hasColumn int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_meta') WHERE name='platform'`,
	).Scan(&hasColumn); err != nil {
		return fmt.Errorf("probe platform column: %w", err)
	}
	if hasColumn == 0 {
		// Column-level DEFAULT '' so existing rows get a concrete value; the backfill
		// then upgrades the ones we can classify.
		if _, err := tx.Exec(`ALTER TABLE session_meta ADD COLUMN platform TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add column: %w", err)
		}
	}
	// Backfill only rows still unclassified (''), so this is idempotent and never
	// overwrites a value a newer binary already wrote. OpenCode sessions carry the
	// "opencode:" id prefix; everything else is Claude.
	if _, err := tx.Exec(
		`UPDATE session_meta SET platform = 'opencode'
		  WHERE platform = '' AND session LIKE 'opencode:%'`,
	); err != nil {
		return fmt.Errorf("backfill opencode: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE session_meta SET platform = 'claude'
		  WHERE platform = '' AND session NOT LIKE 'opencode:%'`,
	); err != nil {
		return fmt.Errorf("backfill claude: %w", err)
	}
	return nil
}

// migrateProgressPerLens is the v4->v5 step: re-key the distillation watermark from
// PK(session) to PK(session, lens) (issue #55).
//
// Why a table rebuild and not ADD COLUMN: the primary key changes (session ->
// session+lens), which SQLite's ALTER TABLE cannot do. So when the running DB's
// progress lacks the `lens` column, rename it aside, let schemaV1 (already applied
// above) provide the new-shape `progress`, and copy the old rows in as the
// 'default' lens's watermark — the lens they actually reflect. Every OTHER
// (session, lens) pair is deliberately left ABSENT so it reads as pending
// (distilled defaults to 0 via the LEFT JOIN): a newly-enabled lens must NOT
// inherit default's watermark, or it would falsely claim to have already mined
// those turns. This mirrors the l0->raw rename discipline: one-time, in place,
// data-preserving, idempotent.
func migrateProgressPerLens(tx *sql.Tx) error {
	var hasLens int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('progress') WHERE name='lens'`,
	).Scan(&hasLens); err != nil {
		return fmt.Errorf("probe lens column: %w", err)
	}
	if hasLens > 0 {
		return nil // already per-lens (fresh DB from schemaV1, or a prior migrate run)
	}
	// The old single-key table exists without a lens column. schemaV1 ran with
	// CREATE ... IF NOT EXISTS, so it did NOT create the new-shape table (the old one
	// was present). Move the old rows aside, build the new shape, seed 'default'.
	stmts := []string{
		`ALTER TABLE progress RENAME TO progress_v4`,
		`CREATE TABLE progress (
		   session      TEXT    NOT NULL,
		   lens         TEXT    NOT NULL DEFAULT 'default',
		   distilled    INTEGER NOT NULL DEFAULT 0,
		   retries      INTEGER NOT NULL DEFAULT 0,
		   distilled_at TEXT    NOT NULL DEFAULT '',
		   next_attempt TEXT    NOT NULL DEFAULT '',
		   drift_at     TEXT    NOT NULL DEFAULT '',
		   PRIMARY KEY (session, lens)
		 )`,
		// drift_at (v7) is created here too so a v4->v7 migration lands the same table
		// shape as a fresh schemaV1 DB. progress_v4 predates drift tracking, so the copy
		// stamps '' (never drifted) explicitly rather than selecting a nonexistent column.
		`INSERT INTO progress(session, lens, distilled, retries, distilled_at, next_attempt, drift_at)
		   SELECT session, 'default', distilled, retries, distilled_at, next_attempt, ''
		     FROM progress_v4`,
		`DROP TABLE progress_v4`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("rebuild progress: %w", err)
		}
	}
	return nil
}

// addProgressIndexes is the v5->v6 step: two hot-path indexes on the per-lens
// progress table (issue #73-S3). Runs after migrateProgressPerLens so the `lens`
// column is guaranteed present. The PK (session, lens) already serves every
// per-(session,lens) point lookup and the PendingSessions CROSS/LEFT JOIN on
// (p.session, p.lens); these add the two remaining drain/review hot-loop predicates
// that were full scans:
//   - (lens, next_attempt): NextBackoffAttempt joins on lens then MINs over
//     next_attempt > ?, and ResetLensWatermark's DELETE WHERE lens=? rides the
//     leftmost lens prefix — one index (covering for the former) serves both.
//   - (distilled_at, session): SessionsSinceReview does COUNT(DISTINCT session)
//     WHERE distilled_at > ? on every ReviewDue check — a covering index answers it
//     straight from the b-tree.
//
// Two composite (covering) indexes, not three single-column ones, keep write
// overhead on the per-mine progress upsert minimal while covering every named
// query. Idempotent: CREATE INDEX IF NOT EXISTS is a no-op once they exist.
func addProgressIndexes(tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_progress_lens_next ON progress(lens, next_attempt)`,
		`CREATE INDEX IF NOT EXISTS idx_progress_distilled_at ON progress(distilled_at, session)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// addDriftColumn is the v6->v7 step: add progress.drift_at if the running DB's
// table predates it (issue #69 Part 2). A pre-v7 progress table has no drift_at;
// schemaV1's CREATE ... IF NOT EXISTS won't alter it, so add the column explicitly
// (SQLite has no ADD COLUMN IF NOT EXISTS — guard on pragma_table_info, mirroring
// addSessionPlatformColumn). The column-level empty-string DEFAULT gives every
// existing row a concrete "never drifted" value with no backfill needed. Idempotent: the guard
// skips the ALTER once the column exists (fresh DBs have it from schemaV1; a re-run
// sees it too).
func addDriftColumn(tx *sql.Tx) error {
	var hasColumn int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('progress') WHERE name='drift_at'`,
	).Scan(&hasColumn); err != nil {
		return fmt.Errorf("probe drift_at column: %w", err)
	}
	if hasColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE progress ADD COLUMN drift_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add column: %w", err)
		}
	}
	return nil
}

// migrateObservationsSeq is the v7->v8 step: rebuild `observations` from the old
// (obs_id TEXT PRIMARY KEY, no surrogate) shape to (seq INTEGER PRIMARY KEY
// AUTOINCREMENT, obs_id TEXT UNIQUE NOT NULL) — issue #125.
//
// Why a table rebuild and not ADD COLUMN: the fix is a NEW integer primary key with
// AUTOINCREMENT semantics (SQLite tracks the high-water mark in sqlite_sequence and
// never reuses a value below it, even after the max row is deleted). A plain ADD
// COLUMN cannot introduce a PRIMARY KEY or AUTOINCREMENT, so — exactly like the v5
// migrateProgressPerLens rebuild — rename the old table aside, let schemaV1 (already
// applied above) provide the new-shape `observations`, and copy the old rows in.
//
// The copy is load-bearing: it writes each row's OLD IMPLICIT ROWID into the explicit
// seq column (SELECT rowid), so every persisted review_rowid:<lens> watermark — a
// rowid cursor — keeps pointing at the same observations after the migration (old
// rowid == new seq). Any gaps left by earlier deletes are preserved. An explicit-seq
// INSERT seeds sqlite_sequence (the AUTOINCREMENT high-water mark) to the MAX seq
// actually copied, so the next mined obs gets a strictly higher seq than any SURVIVING
// row — the reuse hole for a delete-of-newest that happens AFTER the upgrade is closed.
//
// But sqlite_sequence is seeded to the max SURVIVING rowid, NOT the historical
// high-water mark: if the pre-v8 archive deleted its newest obs(es) after folding them,
// a review_rowid:<lens> (or review_obs_rowid) watermark can point ABOVE the surviving
// max. Left alone, the first re-mined obs would get a seq <= that stale watermark and
// the fold (seq > watermark) would silently skip it — #125 reintroduced at the migration
// boundary for exactly the delete-of-newest cohort. So after the copy, CLAMP any such
// watermark down to the surviving max. That is loss-free: every surviving obs has seq <=
// the surviving max, so all of them were already folded (the watermark was above them);
// clamping only lowers a cursor that pointed at deleted rows, and any future append now
// lands strictly above every persisted cursor.
//
// The embedding BLOB rides along verbatim (no re-embedding). Idempotent: the guard skips
// once seq exists (fresh DBs from schemaV1; a re-run sees it too).
func migrateObservationsSeq(tx *sql.Tx) error {
	var hasSeq int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('observations') WHERE name='seq'`,
	).Scan(&hasSeq); err != nil {
		return fmt.Errorf("probe seq column: %w", err)
	}
	if hasSeq > 0 {
		return nil // already the seq shape (fresh DB from schemaV1, or a prior migrate run)
	}
	// The old obs_id-keyed table exists without a seq column. schemaV1 ran with
	// CREATE ... IF NOT EXISTS, so it did NOT create the new-shape table (the old one
	// was present). Move the old rows aside, build the new shape, copy rowid->seq.
	stmts := []string{
		`ALTER TABLE observations RENAME TO observations_v7`,
		`CREATE TABLE observations (
		   seq         INTEGER PRIMARY KEY AUTOINCREMENT,
		   obs_id      TEXT    NOT NULL UNIQUE,
		   ts          TEXT    NOT NULL DEFAULT '',
		   session     TEXT    NOT NULL DEFAULT '',
		   lens        TEXT    NOT NULL DEFAULT '',
		   dimension   TEXT    NOT NULL DEFAULT '',
		   observation TEXT    NOT NULL DEFAULT '',
		   evidence    TEXT    NOT NULL DEFAULT '',
		   poignancy   INTEGER NOT NULL DEFAULT 0,
		   source      TEXT    NOT NULL DEFAULT '',
		   embedding   BLOB
		 )`,
		// Copy old implicit rowid INTO seq so review_rowid watermarks stay valid.
		`INSERT INTO observations(seq, obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding)
		   SELECT rowid, obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding
		     FROM observations_v7`,
		`DROP TABLE observations_v7`,
		// The rename carried the old idx_obs_lens onto observations_v7 and DROP TABLE
		// took it with it; recreate it on the new table (schemaV1 ran before the rename,
		// so its CREATE INDEX IF NOT EXISTS was a no-op against the old-named table).
		`CREATE INDEX IF NOT EXISTS idx_obs_lens ON observations(lens)`,
		// Clamp any fold cursor left ABOVE the surviving max (a pre-v8 delete-of-newest
		// after folding) down to it — otherwise a re-mined obs at a reused-then-freed
		// rowid range would land <= the watermark and be silently skipped (#125 at the
		// migration boundary). Loss-free: surviving obs are all <= this max, so already
		// folded. Covers every per-lens review_rowid:<lens> and the global review_obs_rowid.
		`UPDATE meta
		    SET value = CAST((SELECT COALESCE(MAX(seq), 0) FROM observations) AS TEXT)
		  WHERE (key LIKE 'review_rowid:%' OR key = 'review_obs_rowid')
		    AND CAST(value AS INTEGER) > (SELECT COALESCE(MAX(seq), 0) FROM observations)`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("rebuild observations: %w", err)
		}
	}
	return nil
}

// schemaV1 is the full current schema. `raw` is the append-only ground-truth
// transcript (surrogate rowid; seq is a plain ordinal, not a uniqueness
// constraint, matching the old JSONL append). observations carry a monotonic
// seq (INTEGER PRIMARY KEY AUTOINCREMENT — never reused after a delete, so the
// per-lens review fold cursor never skips a re-mined obs, issue #125) and dedup on
// obs_id UNIQUE (content hash) — the idempotency guarantee the worker relied on. staged is the
// in-session buffer, unique per (session, obs_id) so a re-recorded observation
// collapses. facets/facet_versions model the bi-temporal profile relationally.
// progress is the distillation queue/watermark. meta holds small scalar
// bookkeeping (review offsets).
const schemaV1 = `
CREATE TABLE IF NOT EXISTS raw (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  session TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  ts      TEXT    NOT NULL DEFAULT '',
  role    TEXT    NOT NULL DEFAULT '',
  effort  TEXT    NOT NULL DEFAULT '',
  text    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_raw_session ON raw(session, id);

CREATE TABLE IF NOT EXISTS session_meta (
  session  TEXT PRIMARY KEY,
  cwd      TEXT NOT NULL DEFAULT '',
  started  TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS observations (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  obs_id      TEXT    NOT NULL UNIQUE,
  ts          TEXT    NOT NULL DEFAULT '',
  session     TEXT    NOT NULL DEFAULT '',
  lens        TEXT    NOT NULL DEFAULT '',
  dimension   TEXT    NOT NULL DEFAULT '',
  observation TEXT    NOT NULL DEFAULT '',
  evidence    TEXT    NOT NULL DEFAULT '',
  poignancy   INTEGER NOT NULL DEFAULT 0,
  source      TEXT    NOT NULL DEFAULT '',
  embedding   BLOB
);
CREATE INDEX IF NOT EXISTS idx_obs_lens ON observations(lens);

CREATE TABLE IF NOT EXISTS staged (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  session TEXT NOT NULL,
  obs_id  TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_staged_session ON staged(session);
CREATE UNIQUE INDEX IF NOT EXISTS idx_staged_unique ON staged(session, obs_id);

CREATE TABLE IF NOT EXISTS facets (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  lens      TEXT NOT NULL,
  dimension TEXT NOT NULL,
  key       TEXT NOT NULL,
  last_seen TEXT NOT NULL DEFAULT '',
  UNIQUE(lens, dimension, key)
);

CREATE TABLE IF NOT EXISTS facet_versions (
  facet_id    INTEGER NOT NULL REFERENCES facets(id) ON DELETE CASCADE,
  pos         INTEGER NOT NULL,
  value       TEXT NOT NULL DEFAULT '',
  valid_from  TEXT NOT NULL DEFAULT '',
  valid_to    TEXT NOT NULL DEFAULT '',
  recorded_at TEXT NOT NULL DEFAULT '',
  because_of  TEXT NOT NULL DEFAULT '[]',
  confidence  REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (facet_id, pos)
);

CREATE TABLE IF NOT EXISTS progress (
  session      TEXT    NOT NULL,
  lens         TEXT    NOT NULL DEFAULT 'default',
  distilled    INTEGER NOT NULL DEFAULT 0,
  retries      INTEGER NOT NULL DEFAULT 0,
  distilled_at TEXT    NOT NULL DEFAULT '',
  next_attempt TEXT    NOT NULL DEFAULT '',
  drift_at     TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (session, lens)
);
-- The progress hot-path indexes (issue #73-S3) are NOT here: they reference the
-- lens column and would fail on a v4 DB whose progress table still predates it
-- (schemaV1 runs before migrateProgressPerLens rebuilds the table), and the v5
-- rebuild's DROP TABLE would drop them anyway. They are created as their own
-- idempotent step in migrate() AFTER the rebuild -- see addProgressIndexes.

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);
`

// legacyL0Rename upgrades a pre-rename database (raw layer shipped as `l0`) to the
// current `raw` name, in place, preserving the data. Applied by migrate() only
// when a legacy `l0` exists and `raw` does not. ALTER TABLE RENAME carries the
// data and the index's table reference; we drop+recreate the index so its NAME
// follows the rename too.
const legacyL0Rename = `
ALTER TABLE l0 RENAME TO raw;
DROP INDEX IF EXISTS idx_l0_session;
CREATE INDEX IF NOT EXISTS idx_raw_session ON raw(session, id);
`

// encodeEmbedding packs a float32 vector into little-endian bytes for a BLOB
// column. nil/empty -> nil (stored as SQL NULL).
func encodeEmbedding(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeEmbedding is the inverse of encodeEmbedding.
func decodeEmbedding(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
