package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ObservationID is the L1 dedup identity: the same (session, lens, text) always mints the same
// id, which is what lets an active observation and a mined one collapse, and what emerge_seen
// marks key on. The one home for the scheme — the worker, the MCP server and the CLI all mint
// through here, and existing archives hold ids in exactly this form, so it never changes.
func ObservationID(session, lens, text string) string {
	h := sha1.Sum([]byte(session + "|" + lens + "|" + text))
	return "obs_" + fmt.Sprintf("%x", h[:6])
}

// obsIO is the L1 observation concern: the append-only `observations` table (the
// worker's mined + passed-through-active output) plus the in-session `staged` buffer
// that feeds it. A DB leaf — holds only the shared *sql.DB. The distillation
// watermark that gates re-mining lives on the queue concern; obsIO owns the rows
// themselves.
type obsIO struct{ db *sql.DB }

// ObservationHighWater is the highest observation seq in the archive — the "now" of the
// decay clock (#173). Confidence decays on READ, by the observations elapsed since a facet was
// last confirmed, so every surface that shows a confidence to a person or a model needs this to
// show the CURRENT one: FacetVersion.DecayedConfidence(ObservationHighWater()). The stored
// Confidence is the value as of the last confirmation, which is a record, not a reading.
func (o *obsIO) ObservationHighWater() (int64, error) {
	var hw int64
	err := o.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM observations`).Scan(&hw)
	return hw, err
}

// --- active observation staging (in-session via MCP) ------------------------

// StageObservation is a TEST FIXTURE: StageObservationCapped with both caps off. Production
// staging (the MCP record_observation handler) always passes its caps. Exported because tests
// in other packages seed staged rows through it. Duplicate (session, obs_id) rows collapse
// to one.
func (o *obsIO) StageObservation(ob Observation) error {
	_, err := o.StageObservationCapped(ob, 0, 0)
	return err
}

// StageObservationCapped stages an active observation, enforcing a per-session
// quantity cap ATOMICALLY (limit <= 0 means unlimited). The count check and the
// insert are a single statement, so two concurrent MCP processes can't both pass
// a separate count check and race past the cap. INSERT OR IGNORE collapses a
// duplicate (session, obs_id) — via idx_staged_unique — so a re-recorded
// observation is a no-op, not a quota-burning extra row. Returns whether a row was actually
// inserted (false = at either cap, or a duplicate).
//
// totalLimit is applied in ADDITION to the per-session limit, and bounds the whole staged
// table. The per-session cap alone is evadable: the session id comes from the agent, so a
// runaway or confused one that varies it (per-turn ids, a hallucinated id, an incrementing
// counter) gets a fresh budget every call and can stage without bound — each row carrying up to
// 2000 chars of observation plus 2000 of evidence, all durable in L1 and all fed to the review
// model. A total cap cannot be evaded by choosing a different key, because it has no key.
//
// Both bounds live in the SAME atomic statement as the insert, so concurrent MCP processes
// cannot each pass a check and race past it. limit/totalLimit <= 0 disable the respective bound.
func (o *obsIO) StageObservationCapped(ob Observation, limit, totalLimit int) (bool, error) {
	ob.Source = "active"
	payload, err := json.Marshal(ob)
	if err != nil {
		return false, err
	}
	res, err := o.db.Exec(
		`INSERT OR IGNORE INTO staged(session, obs_id, payload)
		 SELECT ?, ?, ?
		 WHERE (? <= 0 OR (SELECT COUNT(*) FROM staged WHERE session = ?) < ?)
		   AND (? <= 0 OR (SELECT COUNT(*) FROM staged) < ?)`,
		ob.Session, ob.ID, string(payload),
		limit, ob.Session, limit,
		totalLimit, totalLimit)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StagedTotal is the count across ALL sessions — the quantity the total cap bounds. Used to tell
// "hit the global cap" apart from "hit this session's cap" so the error names the right one.
func (o *obsIO) StagedTotal() int {
	var n int
	_ = o.db.QueryRow(`SELECT COUNT(*) FROM staged`).Scan(&n)
	return n
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

// AppendObservations appends the worker's combined output. obs_id is UNIQUE (seq has
// been the primary key since v8), so INSERT OR IGNORE makes the whole write idempotent:
// re-drained active obs and identical re-mines after a crash are dropped, not duplicated.
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
// supported way to correct the profile. Durable against re-mining: the per-session
// watermark won't re-mine an already-distilled delta, and obs_id dedup would catch it
// even if it did.
//
// It moves no review cursor, on purpose. It used to clear the lens's so the next review would
// "re-derive from what's left" — which re-folded the whole lens (about 650 model calls on a
// 20k-observation lens, a day holding the worker lock) and still could not drop the facet:
// evidence only grows (mergeIDs) and absence never retracts. Instead it records the id on the
// pending-delete list in the same transaction, and the next review, under the worker lock, strips
// exactly those ids from every facet and retires a facet left citing nothing
// (Reviewer.pruneDeletedEvidence). Doing that here would race the reviewer, which holds the whole
// facet table in memory and writes it back as a replace. The list, not "every cited id that is not
// an observation", is what makes a delete a delete: an id the model invented, or one `witness
// merge` skipped because this archive holds the same evidence under its own id, is no delete.
func (o *obsIO) DeleteObservation(obsID string) (bool, error) {
	tx, err := o.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM observations WHERE obs_id = ?`, obsID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	// '$[#]' appends. Upsert, so the first delete creates the list.
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, json_array(?))
		ON CONFLICT(key) DO UPDATE SET value = json_insert(value, '$[#]', ?)`, deletedObsPendingKey, obsID, obsID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// deletedObsPendingKey is the meta row holding, as a JSON array, the obs_ids DeleteObservation
// removed that no review has pruned from the facets yet.
const deletedObsPendingKey = "deleted_obs_pending"

// PendingDeletedObservations is the pending-delete list (see DeleteObservation); empty when there is
// none, which is every review's common case.
func (o *obsIO) PendingDeletedObservations() ([]string, error) {
	return queryStrings(context.Background(), o.db,
		`SELECT DISTINCT value FROM json_each((SELECT value FROM meta WHERE key = ?))`, deletedObsPendingKey)
}

// (ReadObservationsLite and ReadObservationsSince used to live here — 96 lines, both with zero
// production callers.
//
// Lite skipped embedding decode for scans that never used the vectors; its caller was the reviewer,
// which moved to the incremental delta read and no longer scans the whole corpus at all.
// ReadObservationsSince was the #16 incremental fold read, superseded by ReadObservationsSinceOrdered
// when the windowed fold needed SEQ order rather than ts order — advancing a watermark over a
// ts-ordered window can skip a low-seq/high-ts observation, which is exactly the bug the Ordered
// variant exists to prevent. Since's only remaining caller was a test that existed solely to test it.
//
// Both were still declared on the ReviewStore interface, so they were kept compiling by the interface
// rather than by any user.)

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
