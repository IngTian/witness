package store

import (
	"database/sql"
)

// rawIO is the L0 ground-truth concern: the append-only `raw` transcript plus the
// read-side size/sampling/pruning helpers over it. A DB leaf — holds only the shared
// *sql.DB. The distillation watermark that COUNTS raw records lives on the queue
// concern; rawIO owns the transcript itself and the raw-generation id (MaxRawID) the
// watermark CAS gates on.
type rawIO struct{ db *sql.DB }

// --- raw capture --------------------------------------------------------------

// AppendRaw records one raw turn-half. Append-only: seq is a stored ordinal, not
// a uniqueness key, so a resumed session that re-numbers from 0 still appends
// (matching the original JSONL semantics). Never blocks the session on failure.
func (r *rawIO) AppendRaw(rec RawRecord) error {
	_, err := r.db.Exec(
		`INSERT INTO raw(session, seq, ts, role, effort, text) VALUES (?, ?, ?, ?, ?, ?)`,
		rec.Session, rec.Seq, rec.TS, rec.Role, rec.Effort, rec.Text)
	return err
}

// ApplyRawImport atomically commits a raw import batch and its importer-owned
// watermark. replace=true rebuilds the session's L0 and resets the distillation
// progress, which is required for mutable sources that can insert or edit prior
// turns after an earlier import.
func (r *rawIO) ApplyRawImport(meta SessionMeta, records []RawRecord, stateKey, stateValue string, replace bool) (err error) {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if replace {
		if _, err = tx.Exec(`DELETE FROM raw WHERE session = ?`, meta.Session); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM progress WHERE session = ?`, meta.Session); err != nil {
			return err
		}
	}
	if meta.Session != "" {
		if _, err = tx.Exec(
			`INSERT INTO session_meta(session, cwd, started) VALUES (?, ?, ?)
			 ON CONFLICT(session) DO UPDATE SET cwd = excluded.cwd, started = excluded.started`,
			meta.Session, meta.Cwd, meta.Started); err != nil {
			return err
		}
	}
	stmt, err := tx.Prepare(`INSERT INTO raw(session, seq, ts, role, effort, text) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, rec := range records {
		if _, err = stmt.Exec(rec.Session, rec.Seq, rec.TS, rec.Role, rec.Effort, rec.Text); err != nil {
			return err
		}
	}
	if stateKey != "" {
		if _, err = tx.Exec(
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, stateKey, stateValue); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReadRaw returns a session's full raw log in capture order.
func (r *rawIO) ReadRaw(session string) ([]RawRecord, error) {
	rows, err := r.db.Query(
		`SELECT ts, session, seq, role, effort, text FROM raw WHERE session = ? ORDER BY id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawRecord
	for rows.Next() {
		var rec RawRecord
		if err := rows.Scan(&rec.TS, &rec.Session, &rec.Seq, &rec.Role, &rec.Effort, &rec.Text); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ReadRawSnapshot returns a session's full raw log (capture order) AND the highest
// raw.id in that same result set — read in ONE query, so the content/count and the
// raw "generation" id are a single atomic snapshot.
//
// Why this exists (issue #67-1): the mine used to read content via ReadRaw and the
// generation id via MaxRawID as TWO separate statements. MaxOpenConns(1) serializes
// each statement but NOT the pair — the connection is released between them. A replace-
// import (ApplyRawImport replace=true: DELETE raw + re-INSERT, under a DIFFERENT lock
// than the worker) or `witness cleanup` committing in that gap pairs an OLD-generation
// count/content with the NEW generation's high id. The CAS in MarkDistilledIfCurrent
// then finds that new id live, passes its guard, and blind-advances the watermark over
// turns that were never mined — a silent lost delta. Reading both from one result set
// makes rawHighID provably the max id of exactly the rows returned, so the pair can
// never straddle a generation boundary. Because rows are ORDER BY id ASC, the max is
// the last row's id; an empty session yields (nil, 0, nil), matching MaxRawID's
// COALESCE(MAX(id), 0) == 0.
func (r *rawIO) ReadRawSnapshot(session string) (recs []RawRecord, rawHighID int64, err error) {
	rows, err := r.db.Query(
		`SELECT id, ts, session, seq, role, effort, text FROM raw WHERE session = ? ORDER BY id`, session)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var rec RawRecord
		if err := rows.Scan(&id, &rec.TS, &rec.Session, &rec.Seq, &rec.Role, &rec.Effort, &rec.Text); err != nil {
			return nil, 0, err
		}
		recs = append(recs, rec)
		if id > rawHighID {
			rawHighID = id
		}
	}
	return recs, rawHighID, rows.Err()
}

// RawCount returns the number of raw records in a session. This is the unit the
// distillation watermark counts in — the same unit MarkDistilled stores, since
// the worker writes len(ReadRaw). (No line-vs-record skew like the old JSONL.)
func (r *rawIO) RawCount(session string) int {
	var n int
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM raw WHERE session = ?`, session).Scan(&n)
	return n
}

func (r *rawIO) LastRawTS() string {
	var ts string
	_ = r.db.QueryRow(`SELECT COALESCE(MAX(ts), '') FROM raw`).Scan(&ts)
	return ts
}

// SampleSessions returns up to n session ids ordered by total raw text size,
// LARGEST first (tie-broken by id for a stable order). Used by `witness lens try`
// to pick a representative sample for a read-only prompt preview: size-descending
// is deterministic across prompt edits (so v1-vs-v2 runs compare the SAME sessions)
// and deliberately surfaces the meatiest, most chunk-prone sessions — the ones a
// lens prompt is most likely to mishandle. Read-only; touches no watermark.
func (r *rawIO) SampleSessions(n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	rows, err := r.db.Query(
		`SELECT session FROM raw GROUP BY session ORDER BY SUM(LENGTH(text)) DESC, session LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sess string
		if err := rows.Scan(&sess); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SampleRecentSessions returns up to n session ids ordered by their most recent raw
// turn, NEWEST first (tie-broken by session for a stable order). The `--recent`
// counterpart to SampleSessions's size-ordering: a lens author often wants to preview
// a prompt against what they were just working on, not the biggest sessions in the
// archive. Read-only; touches no watermark.
//
// A session whose raw rows all carry an empty ts (the ts column is TEXT NOT NULL with
// an empty-string default, and an import with unknown/zeroed timestamps can yield "")
// has MAX(ts)="", which sorts
// LAST under DESC — i.e. treated as least-recent. That is the intended reading: a
// session with no known time can't claim to be recent. It only matters for the rare
// all-empty-ts session, and only affects which sample a read-only preview picks.
func (r *rawIO) SampleRecentSessions(n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	rows, err := r.db.Query(
		`SELECT session FROM raw GROUP BY session ORDER BY MAX(ts) DESC, session LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sess string
		if err := rows.Scan(&sess); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// RawChars is the total CHARACTER length of a session's raw text — the same size
// metric SampleSessions orders by (SQLite LENGTH() counts characters, not bytes, so
// this is chars, not bytes; the naming is deliberate to avoid the ~3× under-report a
// "bytes" label would imply for multibyte/CJK transcripts). Exposed so a preview can
// show how large (and thus how chunk-prone) a session is. Returns 0 for an unknown
// session (errors swallowed, matching RawCount — a size readout is cosmetic).
func (r *rawIO) RawChars(session string) int64 {
	var n int64
	_ = r.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(text)), 0) FROM raw WHERE session = ?`, session).Scan(&n)
	return n
}

// LastDistilledRawTS is the timestamp of the latest raw turn any lens has distilled
// past — a cosmetic "how fresh is distillation" indicator for status/profile.
// Progress is now per-(session,lens), so collapse it to one watermark per session
// (MAX(distilled) = the furthest any lens reached, which the always-on default lens
// usually leads) before the JOIN, so multiple lens rows don't multiply the raw join.
func (r *rawIO) LastDistilledRawTS() string {
	var ts string
	_ = r.db.QueryRow(`
		SELECT COALESCE(MAX(r.ts), '')
		  FROM raw r
		  JOIN (SELECT session, MAX(distilled) AS distilled FROM progress GROUP BY session) p
		    ON p.session = r.session
		 WHERE r.seq < p.distilled`).Scan(&ts)
	return ts
}

// NextSeq returns the next per-session ordinal (== current record count).
func (r *rawIO) NextSeq(session string) int { return r.RawCount(session) }

// MaxRawID returns the highest raw.id for a session — the session's raw
// "generation". raw.id is INTEGER PRIMARY KEY AUTOINCREMENT, so an id is NEVER
// reused after its row is deleted (that's the AUTOINCREMENT guarantee); a replace-
// import (DELETE + re-INSERT) therefore always yields a strictly higher max id
// than what a worker read before the replace. That property is what makes the
// watermark CAS below sound. Returns 0 for a session with no raw rows.
func (r *rawIO) MaxRawID(session string) int64 {
	var id int64
	_ = r.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM raw WHERE session = ?`, session).Scan(&id)
	return id
}

// RawPruneStats reports how many sessions and raw records PruneSessionsBefore
// would remove for the given cutoff (an RFC3339 timestamp): every session whose
// most recent L0 record predates it. Read-only — for the cleanup preview.
func (r *rawIO) RawPruneStats(cutoff string) (sessions, records int, err error) {
	err = r.db.QueryRow(`
		SELECT COUNT(DISTINCT session), COUNT(*)
		  FROM raw
		 WHERE session IN (SELECT session FROM raw GROUP BY session HAVING MAX(ts) < ?)`,
		cutoff).Scan(&sessions, &records)
	return
}

// PruneSessionsBefore deletes the raw transcript (L0) plus the queue/meta rows of
// every session whose most recent L0 record predates cutoff (RFC3339). Derived
// L1/L2 (observations, facets) are deliberately KEPT — they're the durable
// archive; only the bulky raw logs are reclaimed. Purging the whole session
// (raw + progress) is watermark-safe: the count-based distill watermark can never
// point at deleted rows. Returns sessions and records removed.
func (r *rawIO) PruneSessionsBefore(cutoff string) (sessions, records int, err error) {
	tx, err := r.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Collect eligible sessions first (then close the cursor) — with MaxOpenConns(1)
	// we must not hold a read cursor open while issuing the DELETEs on the same tx.
	rows, err := tx.Query(`SELECT session FROM raw GROUP BY session HAVING MAX(ts) < ?`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	var stale []string
	for rows.Next() {
		var sess string
		if err = rows.Scan(&sess); err != nil {
			rows.Close()
			return 0, 0, err
		}
		stale = append(stale, sess)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, sess := range stale {
		res, e := tx.Exec(`DELETE FROM raw WHERE session = ?`, sess)
		if e != nil {
			err = e
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		records += int(n)
		if _, e := tx.Exec(`DELETE FROM progress WHERE session = ?`, sess); e != nil {
			err = e
			return 0, 0, err
		}
		if _, e := tx.Exec(`DELETE FROM session_meta WHERE session = ?`, sess); e != nil {
			err = e
			return 0, 0, err
		}
		// Per-session state parked in `meta` under a "<namespace>:<session>" key (today
		// only opencode's "opencode_import_keys:<session>") would otherwise be orphaned
		// when the session's raw/progress rows go — a slow leak of dead meta rows.
		// Suffix-match ":"+session with substr (NOT LIKE: opencode session ids contain
		// "_", a LIKE wildcard, which would over-match). Stays generic — the store never
		// hardcodes the opencode key constant.
		needle := ":" + sess
		if _, e := tx.Exec(
			`DELETE FROM meta WHERE substr(key, length(key) - length(?) + 1) = ?`, needle, needle); e != nil {
			err = e
			return 0, 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(stale), records, nil
}
