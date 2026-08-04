package store

import (
	"database/sql"
	"encoding/json"
)

// obsIO is the L1 observation concern: the append-only `observations` table (the
// worker's mined + passed-through-active output) plus the in-session `staged` buffer
// that feeds it. A DB leaf — holds only the shared *sql.DB. The distillation
// watermark that gates re-mining lives on the queue concern; obsIO owns the rows
// themselves.
type obsIO struct{ db *sql.DB }

// --- active observation staging (in-session via MCP) ------------------------

// StageObservation records an active (in-session) observation with no quantity
// cap. The session's worker drains it at session end, passing it through verbatim
// (authoritative). Duplicate (session, obs_id) rows collapse to one.
func (o *obsIO) StageObservation(ob Observation) error {
	_, err := o.StageObservationCapped(ob, 0)
	return err
}

// StageObservationCapped stages an active observation, enforcing a per-session
// quantity cap ATOMICALLY (limit <= 0 means unlimited). The count check and the
// insert are a single statement, so two concurrent MCP processes can't both pass
// a separate count check and race past the cap. INSERT OR IGNORE collapses a
// duplicate (session, obs_id) — via idx_staged_unique — so a re-recorded
// observation is a no-op, not a quota-burning extra row. Returns whether a row was actually
// inserted (false = at the cap, or a duplicate).
func (o *obsIO) StageObservationCapped(ob Observation, limit int) (bool, error) {
	ob.Source = "active"
	payload, err := json.Marshal(ob)
	if err != nil {
		return false, err
	}
	res, err := o.db.Exec(
		`INSERT OR IGNORE INTO staged(session, obs_id, payload)
		 SELECT ?, ?, ?
		 WHERE (? <= 0 OR (SELECT COUNT(*) FROM staged WHERE session = ?) < ?)`,
		ob.Session, ob.ID, string(payload), limit, ob.Session, limit)
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
func (o *obsIO) DrainStaged(session string) (obs []Observation, throughID int64, err error) {
	rows, err := o.db.Query(`SELECT id, payload FROM staged WHERE session = ? ORDER BY id`, session)
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
		var ob Observation
		if err := json.Unmarshal([]byte(payload), &ob); err != nil {
			continue // one corrupt staged row shouldn't sink the drain
		}
		obs = append(obs, ob)
		if id > throughID {
			throughID = id
		}
	}
	return obs, throughID, rows.Err()
}

// StagedCount returns how many active observations are currently staged for a
// session (used to bound how many an in-session agent can record).
func (o *obsIO) StagedCount(session string) int {
	var n int
	_ = o.db.QueryRow(`SELECT COUNT(*) FROM staged WHERE session = ?`, session).Scan(&n)
	return n
}

// StagedExists reports whether a specific (session, obs_id) is already staged. It
// disambiguates the two reasons StageObservationCapped can decline to insert: a
// benign duplicate (this exact obs is already staged) vs. hitting the per-session
// cap. Without it, a duplicate recorded while a session happens to be AT the cap
// was mislabeled as a "too many observations" error (a count>=limit check can't
// tell the cases apart).
func (o *obsIO) StagedExists(session, obsID string) bool {
	var one int
	err := o.db.QueryRow(`SELECT 1 FROM staged WHERE session = ? AND obs_id = ? LIMIT 1`, session, obsID).Scan(&one)
	return err == nil
}

// ClearStagedThrough removes staged rows up to and including throughID — called
// ONLY after the worker has committed them to L1 and advanced the watermark, so a
// crash before the clear just re-drains them (and obsID dedup drops the re-write).
// On a FAILED pass the worker skips this, so the active obs survive for retry.
func (o *obsIO) ClearStagedThrough(session string, throughID int64) {
	if throughID <= 0 {
		return
	}
	_, _ = o.db.Exec(`DELETE FROM staged WHERE session = ? AND id <= ?`, session, throughID)
}

// --- observations ---------------------------------------------------------

// AppendObservations appends the worker's combined output. obs_id is the PRIMARY
// KEY, so INSERT OR IGNORE makes the whole write idempotent: re-drained active
// obs and identical re-mines after a crash are dropped, not duplicated.
func (o *obsIO) AppendObservations(obs []Observation) error {
	if len(obs) == 0 {
		return nil
	}
	tx, err := o.db.Begin()
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
	for _, ob := range obs {
		if _, err := stmt.Exec(ob.ID, ob.TS, ob.Session, ob.Lens, ob.Dimension, ob.Observation,
			ob.Evidence, ob.Poignancy, ob.Source, encodeEmbedding(ob.Embedding)); err != nil {
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
// It also RESETS that lens's incremental review watermark, so the promise the CLI and
// the MCP tool both make — "the profile re-derives from what's left on the next review" —
// is actually kept. The fold is incremental (it reads only obs with seq > review_rowid,
// the #16 fix), so a pruned observation that had ALREADY been folded would otherwise sit
// below the cursor forever: the next review would see an empty delta, never invoke the
// model for that lens, and leave the facet it grounded asserting a claim whose only
// evidence (because_of) no longer exists in the archive. Clearing the cursor makes the
// next review re-fold that lens from scratch. That is bounded work, not a timeout risk —
// the fold is windowed by ReviewMaxChars (#123).
func (o *obsIO) DeleteObservation(obsID string) (bool, error) {
	tx, err := o.db.Begin()
	if err != nil {
		return false, err
	}
	// Capture the lens BEFORE the delete; afterwards the row is gone.
	var lens string
	switch err := tx.QueryRow(`SELECT lens FROM observations WHERE obs_id = ?`, obsID).Scan(&lens); {
	case err == sql.ErrNoRows:
		tx.Rollback()
		return false, nil // no such id — not an error, just no hit
	case err != nil:
		tx.Rollback()
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM observations WHERE obs_id = ?`, obsID)
	if err != nil {
		tx.Rollback()
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return false, err
	}
	if n == 0 {
		tx.Rollback()
		return false, nil
	}
	// Same transaction as the delete: the archive must never be left with the row gone
	// but the stale cursor still in place (that is the bug this closes).
	if _, err := tx.Exec(`DELETE FROM meta WHERE key = ?`, reviewRowidKey(lens)); err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ReadObservationsLite is ReadObservations without decoding embeddings — for
// scans that never use the vectors (the reviewer, which slims them off anyway),
// avoiding loading 384 float32s per row across the whole corpus.
func (o *obsIO) ReadObservationsLite(lens string) ([]Observation, error) {
	q := `SELECT obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source
	        FROM observations`
	var args []any
	if lens != "" {
		q += ` WHERE lens = ?`
		args = append(args, lens)
	}
	q += ` ORDER BY seq`
	rows, err := o.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var ob Observation
		if err := rows.Scan(&ob.ID, &ob.TS, &ob.Session, &ob.Lens, &ob.Dimension, &ob.Observation,
			&ob.Evidence, &ob.Poignancy, &ob.Source); err != nil {
			return nil, err
		}
		out = append(out, ob)
	}
	return out, rows.Err()
}

// ReadObservationsSince is the incremental-fold read (issue #16): the observations
// for one lens with seq > sinceRowid, embeddings stripped (like ReadObservationsLite),
// in VALID-TIME order (ts, then seq as a stable tiebreak). The seq cursor is the
// monotonic "new since last review" frontier (advanced by StampReviewLens); the ts
// sort is what lets the reviewer fold "the stance as it was at that moment," since
// under the parallel commit-as-ready drain (#56 B3) seq order is NOT valid-time
// order. The window is one small ReviewEvery batch, so the ts-sort is trivial — this
// is what bounds the reviewer input by "what's new" instead of the whole corpus.
func (o *obsIO) ReadObservationsSince(lens string, sinceRowid int64) ([]Observation, error) {
	rows, err := o.db.Query(
		`SELECT obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source
		   FROM observations
		  WHERE lens = ? AND seq > ?
		  ORDER BY ts, seq`, lens, sinceRowid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var ob Observation
		if err := rows.Scan(&ob.ID, &ob.TS, &ob.Session, &ob.Lens, &ob.Dimension, &ob.Observation,
			&ob.Evidence, &ob.Poignancy, &ob.Source); err != nil {
			return nil, err
		}
		out = append(out, ob)
	}
	return out, rows.Err()
}

// ReadObservationsSinceOrdered is the WINDOWED-fold read (issue #123): obs for one lens
// with seq > since, in SEQ order, each carrying its seq (in Observation.Rowid). Seq order
// (not the ts order of ReadObservationsSince) is load-bearing for the windowed fold:
// windows are then contiguous seq ranges, so advancing the per-lens watermark to a
// window's max seq does not skip a low-seq/high-ts obs that a ts-ordered read would have
// placed in a later window. seq is INTEGER PRIMARY KEY AUTOINCREMENT (db.go, #125), so it
// is strictly monotonic and NEVER reused — a delete-of-newest + re-mine gives the new obs
// a seq above the watermark, where the fold still reads it (the implicit-rowid version
// this replaced could reuse a freed rowid and silently skip it). Embeddings stripped,
// like the sibling read.
func (o *obsIO) ReadObservationsSinceOrdered(lens string, since int64) ([]Observation, error) {
	rows, err := o.db.Query(
		`SELECT seq, obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source
		   FROM observations
		  WHERE lens = ? AND seq > ?
		  ORDER BY seq`, lens, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var ob Observation
		if err := rows.Scan(&ob.Rowid, &ob.ID, &ob.TS, &ob.Session, &ob.Lens, &ob.Dimension, &ob.Observation,
			&ob.Evidence, &ob.Poignancy, &ob.Source); err != nil {
			return nil, err
		}
		out = append(out, ob)
	}
	return out, rows.Err()
}

// unreviewedDeltaSince counts observations for a lens with seq past sinceRowid — the
// primitive behind the Store.UnreviewedDelta convenience (which supplies the lens's
// current watermark). Surfaced in `witness doctor` so a frozen fold is visible (#123).
func (o *obsIO) unreviewedDeltaSince(lens string, sinceRowid int64) int {
	var n int
	_ = o.db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE lens = ? AND seq > ?`, lens, sinceRowid).Scan(&n)
	return n
}

// ReadObservations returns all L1 observations (optionally one lens), in insertion
// order (seq), embeddings decoded.
func (o *obsIO) ReadObservations(lens string) ([]Observation, error) {
	q := `SELECT obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding
	        FROM observations`
	var args []any
	if lens != "" {
		q += ` WHERE lens = ?`
		args = append(args, lens)
	}
	q += ` ORDER BY seq`
	rows, err := o.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var ob Observation
		var emb []byte
		if err := rows.Scan(&ob.ID, &ob.TS, &ob.Session, &ob.Lens, &ob.Dimension, &ob.Observation,
			&ob.Evidence, &ob.Poignancy, &ob.Source, &emb); err != nil {
			return nil, err
		}
		ob.Embedding = decodeEmbedding(emb)
		out = append(out, ob)
	}
	return out, rows.Err()
}
