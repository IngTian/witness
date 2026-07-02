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
// misreading them. It is 3 because two earlier dev-only versions existed: the
// staged-uniqueness index from v2 is folded into schemaV1 (a plain CREATE ... IF
// NOT EXISTS), and the v2->v3 `l0`->`raw` table rename is applied as a guarded
// legacy step in migrate() for any DB that still has the old table.
const schemaVersion = 3

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
		"PRAGMA journal_mode=WAL",   // concurrent readers + one writer across processes
		"PRAGMA busy_timeout=5000",  // wait on a contended write rather than erroring
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
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		tx.Rollback()
		return fmt.Errorf("set user_version: %w", err)
	}
	return tx.Commit()
}

// schemaV1 is the full current schema. `raw` is the append-only ground-truth
// transcript (surrogate rowid; seq is a plain ordinal, not a uniqueness
// constraint, matching the old JSONL append). observations dedup on obs_id
// (content hash) — the idempotency guarantee the worker relied on. staged is the
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
  session TEXT PRIMARY KEY,
  cwd     TEXT NOT NULL DEFAULT '',
  started TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS observations (
  obs_id      TEXT PRIMARY KEY,
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
  session      TEXT PRIMARY KEY,
  distilled    INTEGER NOT NULL DEFAULT 0,
  retries      INTEGER NOT NULL DEFAULT 0,
  distilled_at TEXT    NOT NULL DEFAULT '',
  next_attempt TEXT    NOT NULL DEFAULT ''
);

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
