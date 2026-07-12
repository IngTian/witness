package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// --- raw capture --------------------------------------------------------------

// AppendRaw records one raw turn-half. Append-only: seq is a stored ordinal, not
// a uniqueness key, so a resumed session that re-numbers from 0 still appends
// (matching the original JSONL semantics). Never blocks the session on failure.
func (s *Store) AppendRaw(r RawRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO raw(session, seq, ts, role, effort, text) VALUES (?, ?, ?, ?, ?, ?)`,
		r.Session, r.Seq, r.TS, r.Role, r.Effort, r.Text)
	return err
}

// ApplyRawImport atomically commits a raw import batch and its importer-owned
// watermark. replace=true rebuilds the session's L0 and resets the distillation
// progress, which is required for mutable sources that can insert or edit prior
// turns after an earlier import.
func (s *Store) ApplyRawImport(meta SessionMeta, records []RawRecord, stateKey, stateValue string, replace bool) (err error) {
	tx, err := s.db.Begin()
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
	for _, r := range records {
		if _, err = stmt.Exec(r.Session, r.Seq, r.TS, r.Role, r.Effort, r.Text); err != nil {
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
func (s *Store) ReadRaw(session string) ([]RawRecord, error) {
	rows, err := s.db.Query(
		`SELECT ts, session, seq, role, effort, text FROM raw WHERE session = ? ORDER BY id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawRecord
	for rows.Next() {
		var r RawRecord
		if err := rows.Scan(&r.TS, &r.Session, &r.Seq, &r.Role, &r.Effort, &r.Text); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RawCount returns the number of raw records in a session. This is the unit the
// distillation watermark counts in — the same unit MarkDistilled stores, since
// the worker writes len(ReadRaw). (No line-vs-record skew like the old JSONL.)
func (s *Store) RawCount(session string) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM raw WHERE session = ?`, session).Scan(&n)
	return n
}

func (s *Store) LastRawTS() string {
	var ts string
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(ts), '') FROM raw`).Scan(&ts)
	return ts
}

func (s *Store) LastDistilledRawTS() string {
	var ts string
	_ = s.db.QueryRow(`
		SELECT COALESCE(MAX(r.ts), '')
		  FROM raw r
		  JOIN progress p ON p.session = r.session
		 WHERE r.seq < p.distilled`).Scan(&ts)
	return ts
}

// NextSeq returns the next per-session ordinal (== current record count).
func (s *Store) NextSeq(session string) int { return s.RawCount(session) }

// RecordMeta writes session meta once (idempotent: only on first sight).
//
// NOTE: retained-but-currently-unused on the Claude Code path — CC capture never
// calls this, so session_meta.cwd stays empty for CC sessions (only the OpenCode
// import/capture path, via ApplyRawImport, populates it). The `cwd` column was
// introduced for repo-scoped lenses ("which repo is this session in → apply that
// repo's lens"), but that idea was deliberately dropped: lenses are global and
// nothing is read from a repo, so a cloned repo can't inject a prompt (see
// internal/lens/lens.go). Kept for the OpenCode writer + a possible future
// consumer; nothing reads cwd downstream today (ReadMeta has no callers).
func (s *Store) RecordMeta(m SessionMeta) {
	_, _ = s.db.Exec(
		`INSERT OR IGNORE INTO session_meta(session, cwd, started) VALUES (?, ?, ?)`,
		m.Session, m.Cwd, m.Started)
}

// ReadMeta returns a session's meta (zero value if absent). Currently has no
// callers — see the RecordMeta note above (cwd/session_meta is retained for the
// OpenCode writer and possible future use, not read on any live path).
func (s *Store) ReadMeta(session string) SessionMeta {
	m := SessionMeta{Session: session}
	_ = s.db.QueryRow(`SELECT cwd, started FROM session_meta WHERE session = ?`, session).
		Scan(&m.Cwd, &m.Started)
	return m
}

// --- active observation staging (in-session via MCP) ------------------------

// StageObservation records an active (in-session) observation with no quantity
// cap. The session's worker drains it at session end, passing it through verbatim
// (authoritative). Duplicate (session, obs_id) rows collapse to one.
func (s *Store) StageObservation(o Observation) error {
	_, err := s.StageObservationCapped(o, 0)
	return err
}

// StageObservationCapped stages an active observation, enforcing a per-session
// quantity cap ATOMICALLY (limit <= 0 means unlimited). The count check and the
// insert are a single statement, so two concurrent MCP processes can't both pass
// a separate count check and race past the cap. INSERT OR IGNORE collapses a
// duplicate (session, obs_id) — via idx_staged_unique — so a re-recorded
// observation is a no-op, not a quota-burning extra row. Returns whether a row was actually
// inserted (false = at the cap, or a duplicate).
func (s *Store) StageObservationCapped(o Observation, limit int) (bool, error) {
	o.Source = "active"
	payload, err := json.Marshal(o)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO staged(session, obs_id, payload)
		 SELECT ?, ?, ?
		 WHERE (? <= 0 OR (SELECT COUNT(*) FROM staged WHERE session = ?) < ?)`,
		o.Session, o.ID, string(payload), limit, o.Session, limit)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DrainStaged returns staged active observations for a session along with the
// highest staged id read (throughID). The caller clears exactly those rows via
// ClearStagedThrough AFTER committing them — bounding by id leaves rows staged
// concurrently (by a separate MCP process) during the pass for the next drain.
func (s *Store) DrainStaged(session string) (obs []Observation, throughID int64, err error) {
	rows, err := s.db.Query(`SELECT id, payload FROM staged WHERE session = ? ORDER BY id`, session)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, 0, err
		}
		var o Observation
		if err := json.Unmarshal([]byte(payload), &o); err != nil {
			continue // one corrupt staged row shouldn't sink the drain
		}
		obs = append(obs, o)
		if id > throughID {
			throughID = id
		}
	}
	return obs, throughID, rows.Err()
}

// StagedCount returns how many active observations are currently staged for a
// session (used to bound how many an in-session agent can record).
func (s *Store) StagedCount(session string) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM staged WHERE session = ?`, session).Scan(&n)
	return n
}

// ClearStagedThrough removes staged rows up to and including throughID — called
// ONLY after the worker has committed them to L1 and advanced the watermark, so a
// crash before the clear just re-drains them (and obsID dedup drops the re-write).
// On a FAILED pass the worker skips this, so the active obs survive for retry.
func (s *Store) ClearStagedThrough(session string, throughID int64) {
	if throughID <= 0 {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM staged WHERE session = ? AND id <= ?`, session, throughID)
}

// --- the distillation queue (progress / watermark / retries) -----------------

// DistilledCount returns the watermark: how many L0 records of a session the
// worker has already distilled. 0 if absent.
func (s *Store) DistilledCount(session string) int {
	var n int
	_ = s.db.QueryRow(`SELECT distilled FROM progress WHERE session = ?`, session).Scan(&n)
	return n
}

// MarkDistilled records the watermark (count of L0 records distilled so far) and
// stamps when it advanced (for review cadence). Written LAST in Process so a
// crash mid-distill re-runs the delta; obsID dedup keeps that from duplicating.
func (s *Store) MarkDistilled(session string, count int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO progress(session, distilled, distilled_at) VALUES (?, ?, ?)
		 ON CONFLICT(session) DO UPDATE SET distilled = excluded.distilled, distilled_at = excluded.distilled_at`,
		session, count, now)
	return err
}

// RetryCount returns how many times distilling this session has failed in a row.
func (s *Store) RetryCount(session string) int {
	var n int
	_ = s.db.QueryRow(`SELECT retries FROM progress WHERE session = ?`, session).Scan(&n)
	return n
}

// IncRetry bumps the failure count and returns the new value.
func (s *Store) IncRetry(session string) int {
	_, _ = s.db.Exec(
		`INSERT INTO progress(session, retries) VALUES (?, 1)
		 ON CONFLICT(session) DO UPDATE SET retries = retries + 1`, session)
	return s.RetryCount(session)
}

// ResetRetry clears the failure count and any backoff (a clean distill).
func (s *Store) ResetRetry(session string) {
	_, _ = s.db.Exec(`UPDATE progress SET retries = 0, next_attempt = '' WHERE session = ?`, session)
}

// SetNextAttempt records the earliest time this session should be retried.
// PendingSessions excludes a session until then, so a failing session backs off
// instead of being hammered every trigger.
func (s *Store) SetNextAttempt(session string, at time.Time) error {
	v := at.UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO progress(session, next_attempt) VALUES (?, ?)
		 ON CONFLICT(session) DO UPDATE SET next_attempt = excluded.next_attempt`, session, v)
	return err
}

// NextBackoffAttempt returns the earliest future retry time, if any session is
// currently sleeping due to a mining failure.
func (s *Store) NextBackoffAttempt(now time.Time) (time.Time, bool) {
	var v string
	_ = s.db.QueryRow(`SELECT COALESCE(MIN(next_attempt), '') FROM progress WHERE next_attempt != '' AND next_attempt > ?`, now.UTC().Format(time.RFC3339)).Scan(&v)
	if v == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// PendingSessions returns sessions whose L0 has grown past the distillation
// watermark, or that have staged active observations waiting to be drained.
// Keying on the watermark (not a mere marker) means a RESUMED session whose log
// gains new turns is picked up again.
func (s *Store) PendingSessions() ([]string, error) {
	return s.PendingSessionsUpdatedBetween(time.Time{}, time.Time{})
}

// PendingSessionsUpdatedBetween applies an optional inclusive range to each
// session's most recent raw timestamp. Zero bounds are open. Sessions with no
// timestamp remain eligible only when no range is requested.
func (s *Store) PendingSessionsUpdatedBetween(since, until time.Time) ([]string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	sinceValue := ""
	if !since.IsZero() {
		sinceValue = since.UTC().Format(time.RFC3339Nano)
	}
	untilValue := ""
	if !until.IsZero() {
		untilValue = until.UTC().Format(time.RFC3339Nano)
	}
	rows, err := s.db.Query(
		`WITH candidates AS (
		   SELECT l.session
		     FROM (SELECT session, COUNT(*) AS c FROM raw GROUP BY session) l
		     LEFT JOIN progress p ON p.session = l.session
		    WHERE l.c > COALESCE(p.distilled, 0)
		      AND (COALESCE(p.next_attempt, '') = '' OR COALESCE(p.next_attempt, '') <= ?)
		   UNION
		   SELECT st.session
		     FROM staged st
		     LEFT JOIN progress p ON p.session = st.session
		    WHERE COALESCE(st.session, '') != ''
		      AND (COALESCE(p.next_attempt, '') = '' OR COALESCE(p.next_attempt, '') <= ?)
		  ), updates AS (
		   SELECT session, MAX(julianday(ts)) AS updated_at
		     FROM raw
		    GROUP BY session
		  )
		 SELECT c.session
		   FROM candidates c
		   LEFT JOIN updates u ON u.session = c.session
		  WHERE (? = '' OR u.updated_at >= julianday(?))
		    AND (? = '' OR u.updated_at <= julianday(?))
		  ORDER BY c.session`, now, now, sinceValue, sinceValue, untilValue, untilValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []string
	for rows.Next() {
		var session string
		if err := rows.Scan(&session); err != nil {
			return nil, err
		}
		pending = append(pending, session)
	}
	return pending, rows.Err()
}

// Stats is a snapshot of the archive + queue health, for `witness doctor` — so a
// user can see distillation is keeping up (or stuck) instead of it failing silently.
type Stats struct {
	Sessions     int    // distinct sessions captured
	RawRecords   int    // raw turn-halves
	Observations int    // L1
	Facets       int    // L2
	Pending      int    // sessions with undistilled turns, eligible now
	BackedOff    int    // sessions waiting out a retry backoff (mining is failing)
	LastReview   string // RFC3339 of the last reviewer run ("" = never)
}

// Stats gathers the doctor snapshot in a few cheap indexed queries.
func (s *Store) Stats() Stats {
	now := time.Now().UTC().Format(time.RFC3339)
	var st Stats
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT session) FROM raw`).Scan(&st.Sessions)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM raw`).Scan(&st.RawRecords)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&st.Observations)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM facets`).Scan(&st.Facets)
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM (
		   SELECT l.session
		     FROM (SELECT session, COUNT(*) c FROM raw GROUP BY session) l
		     LEFT JOIN progress p ON p.session = l.session
		    WHERE l.c > COALESCE(p.distilled,0)
		      AND (COALESCE(p.next_attempt,'') = '' OR COALESCE(p.next_attempt,'') <= ?)
		   UNION
		   SELECT st.session
		     FROM staged st
		     LEFT JOIN progress p ON p.session = st.session
		    WHERE COALESCE(st.session, '') != ''
		      AND (COALESCE(p.next_attempt,'') = '' OR COALESCE(p.next_attempt,'') <= ?))`, now, now).Scan(&st.Pending)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM progress WHERE next_attempt != '' AND next_attempt > ?`, now).Scan(&st.BackedOff)
	st.LastReview = s.metaStr("review_ts")
	return st
}

// WorkerLock serializes worker runs (leader election, not L0/L2 mutual exclusion).
// Returns an unlock func and whether the lock was acquired; if not, another
// worker is already draining the queue. Kept as a filesystem flock (independent
// of the DB) so it works the same regardless of storage backend.
func (s *Store) WorkerLock() (unlock func(), ok bool) {
	return s.lockFile(".worker.lock")
}

// OpenCodeSyncLock serializes imports from OpenCode's session database. The
// importer is watermark-based, but concurrent importers can otherwise read the
// same count and append the same text rows twice.
func (s *Store) OpenCodeSyncLock() (unlock func(), ok bool) {
	return s.lockFile(".opencode-sync.lock")
}

func (s *Store) lockFile(name string) (unlock func(), ok bool) {
	path := filepath.Join(s.Root, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, false
	}
	// flockFile takes the OS-specific exclusive, non-blocking lock (flock(2) on
	// Unix, LockFileEx on Windows) and owns closing f. Split by GOOS so the binary
	// builds on both — see flock_unix.go / flock_windows.go.
	return flockFile(f)
}

// --- observations ---------------------------------------------------------

// AppendObservations appends the worker's combined output. obs_id is the PRIMARY
// KEY, so INSERT OR IGNORE makes the whole write idempotent: re-drained active
// obs and identical re-mines after a crash are dropped, not duplicated.
func (s *Store) AppendObservations(obs []Observation) error {
	if len(obs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO observations
		   (obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, o := range obs {
		if _, err := stmt.Exec(o.ID, o.TS, o.Session, o.Lens, o.Dimension, o.Observation,
			o.Evidence, o.Poignancy, o.Source, encodeEmbedding(o.Embedding)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeleteObservation removes one L1 observation by obs_id, reporting whether a row
// was actually deleted (false = no such id, not an error). Facets are reviewer-
// owned and read-only to humans; pruning a wrong/stale observation here is the
// supported way to correct the profile — the next review re-derives from what's
// left. Durable against re-mining: the per-session watermark won't re-mine an
// already-distilled delta, and obs_id dedup would catch it even if it did.
func (s *Store) DeleteObservation(obsID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM observations WHERE obs_id = ?`, obsID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RawPruneStats reports how many sessions and raw records PruneSessionsBefore
// would remove for the given cutoff (an RFC3339 timestamp): every session whose
// most recent L0 record predates it. Read-only — for the cleanup preview.
func (s *Store) RawPruneStats(cutoff string) (sessions, records int, err error) {
	err = s.db.QueryRow(`
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
func (s *Store) PruneSessionsBefore(cutoff string) (sessions, records int, err error) {
	tx, err := s.db.Begin()
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
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(stale), records, nil
}

// ReadObservationsLite is ReadObservations without decoding embeddings — for
// scans that never use the vectors (the reviewer, which slims them off anyway),
// avoiding loading 384 float32s per row across the whole corpus.
func (s *Store) ReadObservationsLite(lens string) ([]Observation, error) {
	q := `SELECT obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source
	        FROM observations`
	var args []any
	if lens != "" {
		q += ` WHERE lens = ?`
		args = append(args, lens)
	}
	q += ` ORDER BY rowid`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.TS, &o.Session, &o.Lens, &o.Dimension, &o.Observation,
			&o.Evidence, &o.Poignancy, &o.Source); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ReadObservations returns all L1 observations (optionally one lens), in insertion
// order (rowid), embeddings decoded.
func (s *Store) ReadObservations(lens string) ([]Observation, error) {
	q := `SELECT obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding
	        FROM observations`
	var args []any
	if lens != "" {
		q += ` WHERE lens = ?`
		args = append(args, lens)
	}
	q += ` ORDER BY rowid`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		var emb []byte
		if err := rows.Scan(&o.ID, &o.TS, &o.Session, &o.Lens, &o.Dimension, &o.Observation,
			&o.Evidence, &o.Poignancy, &o.Source, &emb); err != nil {
			return nil, err
		}
		o.Embedding = decodeEmbedding(emb)
		out = append(out, o)
	}
	return out, rows.Err()
}

// --- facets (reviewer is sole writer) -------------------------------------

// ReadFacets loads the L2 profile (all facets across all lenses), in a
// deterministic order (lens, dimension, key) so the profile doesn't churn.
func (s *Store) ReadFacets() ([]Facet, error) {
	rows, err := s.db.Query(
		`SELECT f.id, f.lens, f.dimension, f.key, f.last_seen,
		        v.value, v.valid_from, v.valid_to, v.recorded_at, v.because_of, v.confidence
		   FROM facets f
		   LEFT JOIN facet_versions v ON v.facet_id = f.id
		  ORDER BY f.lens, f.dimension, f.key, v.pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Facet
	var curID int64 = -1
	for rows.Next() {
		var (
			id                                int64
			lens, dim, key, lastSeen          string
			value, vFrom, vTo, recAt, because *string
			confidence                        *float64
		)
		if err := rows.Scan(&id, &lens, &dim, &key, &lastSeen,
			&value, &vFrom, &vTo, &recAt, &because, &confidence); err != nil {
			return nil, err
		}
		if id != curID {
			out = append(out, Facet{Lens: lens, Dimension: dim, Key: key, LastSeen: lastSeen})
			curID = id
		}
		if value == nil { // LEFT JOIN with no versions
			continue
		}
		fv := FacetVersion{
			Value: *value, ValidFrom: deref(vFrom), ValidTo: deref(vTo),
			RecordedAt: deref(recAt), Confidence: derefF(confidence),
		}
		if because != nil {
			_ = json.Unmarshal([]byte(*because), &fv.BecauseOf)
		}
		f := &out[len(out)-1]
		f.Versions = append(f.Versions, fv)
	}
	return out, rows.Err()
}

// WriteFacets atomically replaces the L2 profile. Only the reviewer calls this.
// The whole rewrite runs in one transaction; foreign_keys=ON cascades the old
// versions when their facets are deleted.
func (s *Store) WriteFacets(facets []Facet) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM facets`); err != nil {
		tx.Rollback()
		return err
	}
	fStmt, err := tx.Prepare(`INSERT INTO facets(lens, dimension, key, last_seen) VALUES (?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer fStmt.Close()
	vStmt, err := tx.Prepare(
		`INSERT INTO facet_versions(facet_id, pos, value, valid_from, valid_to, recorded_at, because_of, confidence)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer vStmt.Close()
	for _, f := range facets {
		res, err := fStmt.Exec(f.Lens, f.Dimension, f.Key, f.LastSeen)
		if err != nil {
			tx.Rollback()
			return err
		}
		id, _ := res.LastInsertId()
		for pos, v := range f.Versions {
			because, _ := json.Marshal(v.BecauseOf)
			if v.BecauseOf == nil {
				because = []byte("[]")
			}
			if _, err := vStmt.Exec(id, pos, v.Value, v.ValidFrom, v.ValidTo, v.RecordedAt,
				string(because), v.Confidence); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
