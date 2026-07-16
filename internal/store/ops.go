package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// SampleSessions returns up to n session ids ordered by total raw text size,
// LARGEST first (tie-broken by id for a stable order). Used by `witness lens try`
// to pick a representative sample for a read-only prompt preview: size-descending
// is deterministic across prompt edits (so v1-vs-v2 runs compare the SAME sessions)
// and deliberately surfaces the meatiest, most chunk-prone sessions — the ones a
// lens prompt is most likely to mishandle. Read-only; touches no watermark.
func (s *Store) SampleSessions(n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	rows, err := s.db.Query(
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
func (s *Store) SampleRecentSessions(n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	rows, err := s.db.Query(
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
func (s *Store) RawChars(session string) int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(text)), 0) FROM raw WHERE session = ?`, session).Scan(&n)
	return n
}

// LastDistilledRawTS is the timestamp of the latest raw turn any lens has distilled
// past — a cosmetic "how fresh is distillation" indicator for status/profile.
// Progress is now per-(session,lens), so collapse it to one watermark per session
// (MAX(distilled) = the furthest any lens reached, which the always-on default lens
// usually leads) before the JOIN, so multiple lens rows don't multiply the raw join.
func (s *Store) LastDistilledRawTS() string {
	var ts string
	_ = s.db.QueryRow(`
		SELECT COALESCE(MAX(r.ts), '')
		  FROM raw r
		  JOIN (SELECT session, MAX(distilled) AS distilled FROM progress GROUP BY session) p
		    ON p.session = r.session
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

// SetSessionPlatform records which platform OWNS a session (the per-session axis,
// issue #21) — distinct from the global distillation runner. Upsert, since a CC
// session has no session_meta row until now: INSERT the row if absent, else set
// only the platform column (leaving cwd/started untouched). Best-effort at the
// capture/import boundary; ForSession falls back to the id prefix when unset, so a
// missed write degrades to the same answer rather than misclassifying.
func (s *Store) SetSessionPlatform(session, platform string) {
	if session == "" || platform == "" {
		return
	}
	_, _ = s.db.Exec(
		`INSERT INTO session_meta(session, platform) VALUES (?, ?)
		 ON CONFLICT(session) DO UPDATE SET platform = excluded.platform`,
		session, platform)
}

// SessionPlatform returns the recorded owning platform for a session, or "" if no
// row exists or it was never classified. ForSession layers the prefix/default rule
// on top of this — this method only reports the persisted column.
func (s *Store) SessionPlatform(session string) string {
	var p string
	_ = s.db.QueryRow(`SELECT platform FROM session_meta WHERE session = ?`, session).Scan(&p)
	return p
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

// StagedExists reports whether a specific (session, obs_id) is already staged. It
// disambiguates the two reasons StageObservationCapped can decline to insert: a
// benign duplicate (this exact obs is already staged) vs. hitting the per-session
// cap. Without it, a duplicate recorded while a session happens to be AT the cap
// was mislabeled as a "too many observations" error (a count>=limit check can't
// tell the cases apart).
func (s *Store) StagedExists(session, obsID string) bool {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM staged WHERE session = ? AND obs_id = ? LIMIT 1`, session, obsID).Scan(&one)
	return err == nil
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
//
// The watermark is per-(session, lens) (issue #55): each lens tracks how far it has
// mined a session independently, so enabling a NEW lens backfills only that lens
// over history without re-mining the always-on `default` (or any other) lens. Every
// queue op below takes a lens; the #49-C2 raw-generation CAS is preserved, now also
// matching on lens.

// DistilledCount returns the watermark for one (session, lens): how many L0 records
// this lens has already mined for the session. 0 if absent (a lens that has never
// touched this session — e.g. a just-enabled lens — reads 0, so its whole history
// is pending).
func (s *Store) DistilledCount(session, lens string) int {
	var n int
	_ = s.db.QueryRow(`SELECT distilled FROM progress WHERE session = ? AND lens = ?`, session, lens).Scan(&n)
	return n
}

// MarkDistilled records the watermark (count of L0 records this lens has mined) and
// stamps when it advanced (for review cadence). Written LAST in Process so a
// crash mid-distill re-runs the delta; obsID dedup keeps that from duplicating.
func (s *Store) MarkDistilled(session, lens string, count int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO progress(session, lens, distilled, distilled_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(session, lens) DO UPDATE SET distilled = excluded.distilled, distilled_at = excluded.distilled_at`,
		session, lens, count, now)
	return err
}

// MaxRawID returns the highest raw.id for a session — the session's raw
// "generation". raw.id is INTEGER PRIMARY KEY AUTOINCREMENT, so an id is NEVER
// reused after its row is deleted (that's the AUTOINCREMENT guarantee); a replace-
// import (DELETE + re-INSERT) therefore always yields a strictly higher max id
// than what a worker read before the replace. That property is what makes the
// watermark CAS below sound. Returns 0 for a session with no raw rows.
func (s *Store) MaxRawID(session string) int64 {
	var id int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM raw WHERE session = ?`, session).Scan(&id)
	return id
}

// MarkDistilledIfCurrent advances the watermark ONLY if the session's raw is still
// the exact generation the worker mined — proven by the highest raw.id it read
// (rawHighID) still being present. It reports whether the write happened.
//
// Why this exists (issue #49 C2): MarkDistilled is a blind upsert of a COUNT
// captured at the start of a mine. That count assumes raw is append-only. It isn't
// always: an OpenCode history rewrite/edit takes the replace path (DELETE raw +
// DELETE progress + re-INSERT the new turns), and `witness cleanup` deletes raw too
// — both under a DIFFERENT lock than the worker. If such a delete lands mid-mine,
// a trailing blind MarkDistilled would RESURRECT the just-deleted progress row with
// a stale count, silently marking never-mined turns as done. The guard closes that:
// after a replace the old rawHighID no longer exists, the UPDATE/INSERT matches no
// row, nothing is written, progress stays absent, and the pending query re-offers
// the session so it re-mines the new generation from scratch. The wasted mine is
// discarded — correctness over the (rare, racy) wasted work.
//
// Runtime-agnostic: it guards EVERY session (Claude Code + OpenCode) with one code
// path. On the append-only Claude path the mined rawHighID always still exists, so
// the guard always passes — free insurance there, an active fix on the OpenCode/
// cleanup paths that can delete raw under a mine.
func (s *Store) MarkDistilledIfCurrent(session, lens string, count int, rawHighID int64) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	// The guard: only write when a raw row with the mined generation's high id still
	// exists for this session. rawHighID==0 means the mine saw an empty session
	// (nothing mined) — still gate on "no raw exists now" so a concurrent import that
	// added rows isn't clobbered. The generation is a property of the session's raw,
	// not the lens, so the guard is unchanged in spirit — only the row it writes/
	// conflicts on is now keyed by (session, lens).
	res, err := s.db.Exec(
		`INSERT INTO progress(session, lens, distilled, distilled_at)
		 SELECT ?, ?, ?, ?
		  WHERE (? = 0 AND NOT EXISTS (SELECT 1 FROM raw WHERE session = ?))
		     OR EXISTS (SELECT 1 FROM raw WHERE session = ? AND id = ?)
		 ON CONFLICT(session, lens) DO UPDATE SET distilled = excluded.distilled, distilled_at = excluded.distilled_at`,
		session, lens, count, now, rawHighID, session, session, rawHighID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RetryCount returns how many times distilling this (session, lens) has failed in a
// row.
func (s *Store) RetryCount(session, lens string) int {
	var n int
	_ = s.db.QueryRow(`SELECT retries FROM progress WHERE session = ? AND lens = ?`, session, lens).Scan(&n)
	return n
}

// IncRetry bumps the (session, lens) failure count and returns the new value.
func (s *Store) IncRetry(session, lens string) int {
	_, _ = s.db.Exec(
		`INSERT INTO progress(session, lens, retries) VALUES (?, ?, 1)
		 ON CONFLICT(session, lens) DO UPDATE SET retries = retries + 1`, session, lens)
	return s.RetryCount(session, lens)
}

// ResetRetry clears the (session, lens) failure count and any backoff (a clean
// distill).
func (s *Store) ResetRetry(session, lens string) {
	_, _ = s.db.Exec(`UPDATE progress SET retries = 0, next_attempt = '' WHERE session = ? AND lens = ?`, session, lens)
}

// SetNextAttempt records the earliest time this (session, lens) should be retried.
// The pending query excludes the pair until then, so a failing lens backs off
// instead of being hammered every trigger — and a healthy sibling lens on the same
// session is unaffected (independent watermarks).
func (s *Store) SetNextAttempt(session, lens string, at time.Time) error {
	v := at.UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO progress(session, lens, next_attempt) VALUES (?, ?, ?)
		 ON CONFLICT(session, lens) DO UPDATE SET next_attempt = excluded.next_attempt`, session, lens, v)
	return err
}

// LensBackedOff reports whether a (session, lens) pair is currently sleeping out a
// retry backoff (its next_attempt is set and still in the future at `now`). This is
// the SAME per-lens gate PendingSessions applies, exposed for the MINING path so the
// worker can skip a backed-off lens even when a healthy sibling lens re-offers the
// session (issue #55: the offer is session-granular, so a session stays pending as
// long as ANY active lens is behind — but each lens's OWN backoff must still park it,
// or a failing lens gets re-hammered on every sibling-driven drain). An empty/absent
// next_attempt reads as not-backed-off, so this is cheap and defaults open.
func (s *Store) LensBackedOff(session, lens string, now time.Time) bool {
	var next string
	_ = s.db.QueryRow(
		`SELECT COALESCE(next_attempt, '') FROM progress WHERE session = ? AND lens = ?`,
		session, lens).Scan(&next)
	if next == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, next)
	if err != nil {
		return false // an unparseable stamp must not permanently park a lens
	}
	return at.After(now)
}

// ResetLensWatermark drops every progress row for one lens, so all sessions read
// as pending FOR THAT LENS on the next drain — the enable-a-new-lens backfill path
// (issue #55). Other lenses' watermarks (rows with a different lens) are untouched,
// so `default` is never re-mined. Returns rows removed.
func (s *Store) ResetLensWatermark(lens string) (int, error) {
	res, err := s.db.Exec(`DELETE FROM progress WHERE lens = ?`, lens)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteLensData removes one lens's derived L1 observations and L2 facets (for a
// `lens rebuild`: re-mine a lens from scratch after its prompt changed). Raw L0 is
// the durable record and is NOT touched. facet_versions cascade via the ON DELETE
// CASCADE foreign key. Runs in one transaction so a rebuild never leaves half a
// lens's derived data behind. Returns (observations, facets) removed.
func (s *Store) DeleteLensData(lens string) (obs, facets int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	res, err := tx.Exec(`DELETE FROM observations WHERE lens = ?`, lens)
	if err != nil {
		return 0, 0, err
	}
	no, _ := res.RowsAffected()
	res, err = tx.Exec(`DELETE FROM facets WHERE lens = ?`, lens)
	if err != nil {
		return 0, 0, err
	}
	nf, _ := res.RowsAffected()
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return int(no), int(nf), nil
}

// NextBackoffAttempt returns the earliest future retry time among ACTIVE lenses, if
// any (session,lens) is currently sleeping due to a mining failure. Filtering by
// the active-lens set matters now that the watermark is per-lens (issue #55): a
// backoff row stranded on a since-disabled lens must not schedule a useless wakeup
// (nor be counted as outstanding work) — the disabled lens is no longer mined, so
// its old backoff is inert. `lenses` follows the same contract as PendingSessions.
func (s *Store) NextBackoffAttempt(lenses []string, now time.Time) (time.Time, bool) {
	lensValues, lensArgs := lensValuesClause(activeLensList(lenses))
	args := append([]any{}, lensArgs...)
	args = append(args, now.UTC().Format(time.RFC3339))
	var v string
	_ = s.db.QueryRow(
		`WITH active_lens(lens) AS (`+lensValues+`)
		 SELECT COALESCE(MIN(p.next_attempt), '')
		   FROM progress p JOIN active_lens x ON x.lens = p.lens
		  WHERE p.next_attempt != '' AND p.next_attempt > ?`, args...).Scan(&v)
	if v == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// PendingSessions returns sessions for which SOME active lens has L0 grown past
// that lens's watermark, or that have staged active observations waiting to be
// drained. Keying on the watermark (not a mere marker) means a RESUMED session
// whose log gains new turns is picked up again — and a newly-enabled lens makes
// every historical session pending FOR THAT LENS without disturbing lenses already
// caught up. `lenses` is the active-lens name set (default + enabled); it is passed
// in rather than derived here so the store stays out of lens/config loading and a
// config-enabled-but-unloadable lens can be excluded by the caller (else the query
// would keep offering a session no worker can actually mine — a no-progress cycle).
// An empty list falls back to `["default"]` (the always-on lens).
func (s *Store) PendingSessions(lenses []string) ([]string, error) {
	return s.PendingSessionsUpdatedBetween(lenses, time.Time{}, time.Time{})
}

// PendingSessionsUpdatedBetween applies an optional inclusive range to each
// session's most recent raw timestamp. Zero bounds are open. Sessions with no
// timestamp remain eligible only when no range is requested. A session is pending
// when ANY active lens is behind on it (raw count > that lens's watermark) and that
// lens's (session,lens) backoff has elapsed — a per-lens backoff no longer parks
// the whole session, only that lens's share of it.
func (s *Store) PendingSessionsUpdatedBetween(lenses []string, since, until time.Time) ([]string, error) {
	lenses = activeLensList(lenses)
	now := time.Now().UTC().Format(time.RFC3339)
	sinceValue := ""
	if !since.IsZero() {
		sinceValue = since.UTC().Format(time.RFC3339Nano)
	}
	untilValue := ""
	if !until.IsZero() {
		untilValue = until.UTC().Format(time.RFC3339Nano)
	}
	// Build the active-lens VALUES list and the arg vector. The lens list feeds a
	// CROSS JOIN so each session is paired with every active lens; the LEFT JOIN then
	// finds that specific (session,lens) watermark.
	lensValues, lensArgs := lensValuesClause(lenses)
	args := []any{}
	args = append(args, lensArgs...) // active_lens CTE (referenced by both branches)
	args = append(args, now)         // raw-branch per-lens backoff gate
	args = append(args, now)         // staged-branch per-lens backoff gate
	args = append(args, sinceValue, sinceValue, untilValue, untilValue)
	rows, err := s.db.Query(
		`WITH active_lens(lens) AS (`+lensValues+`),
		  candidates AS (
		   SELECT l.session
		     FROM (SELECT session, COUNT(*) AS c FROM raw GROUP BY session) l
		     CROSS JOIN active_lens x
		     LEFT JOIN progress p ON p.session = l.session AND p.lens = x.lens
		    WHERE l.c > COALESCE(p.distilled, 0)
		      AND (COALESCE(p.next_attempt, '') = '' OR COALESCE(p.next_attempt, '') <= ?)
		   UNION
		   -- Staged (active-obs) branch: a session with staged obs needs a drain even
		   -- if no lens has new raw turns. Gate it per-lens against the SAME active_lens
		   -- set (not a hardcoded 'default'): offer the session if ANY active lens's
		   -- (session,lens) pair is ready — no progress row, or its backoff has elapsed.
		   -- Draining staged obs is lens-independent, so "some active lens can run" is
		   -- the right, lens-equal readiness condition; if EVERY active lens is backed
		   -- off there is nothing to do this pass, matching the raw branch.
		   SELECT st.session
		     FROM staged st
		     CROSS JOIN active_lens x
		     LEFT JOIN progress p ON p.session = st.session AND p.lens = x.lens
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
		  ORDER BY c.session`, args...)
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

// activeLensList normalizes the caller's active-lens set: empty falls back to the
// always-on default lens. Keeps the queue queries well-formed even when a caller
// (a test, or a degenerate config) passes nothing.
func activeLensList(lenses []string) []string {
	if len(lenses) == 0 {
		return []string{LensDefault}
	}
	return lenses
}

// lensValuesClause builds a `VALUES (?),(?),...` fragment plus its args for binding
// an in-memory lens list into a CTE. Parameterized (never string-interpolated) so a
// lens name can't inject SQL.
func lensValuesClause(lenses []string) (string, []any) {
	rows := make([]string, len(lenses))
	args := make([]any, len(lenses))
	for i, ln := range lenses {
		rows[i] = "(?)"
		args[i] = ln
	}
	return "VALUES " + strings.Join(rows, ","), args
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

// Stats gathers the doctor snapshot in a few cheap indexed queries. `lenses` is the
// active-lens name set (same contract as PendingSessions): Pending counts distinct
// sessions for which some active lens is behind, so the doctor count matches what
// the worker will actually drain. BackedOff counts distinct sessions that have at
// least one (session,lens) currently sleeping on a retry backoff.
func (s *Store) Stats(lenses []string) Stats {
	now := time.Now().UTC().Format(time.RFC3339)
	var st Stats
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT session) FROM raw`).Scan(&st.Sessions)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM raw`).Scan(&st.RawRecords)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&st.Observations)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM facets`).Scan(&st.Facets)
	lensValues, lensArgs := lensValuesClause(activeLensList(lenses))
	args := append([]any{}, lensArgs...)
	args = append(args, now, now)
	_ = s.db.QueryRow(
		`WITH active_lens(lens) AS (`+lensValues+`)
		 SELECT COUNT(*) FROM (
		   SELECT l.session
		     FROM (SELECT session, COUNT(*) c FROM raw GROUP BY session) l
		     CROSS JOIN active_lens x
		     LEFT JOIN progress p ON p.session = l.session AND p.lens = x.lens
		    WHERE l.c > COALESCE(p.distilled,0)
		      AND (COALESCE(p.next_attempt,'') = '' OR COALESCE(p.next_attempt,'') <= ?)
		   UNION
		   SELECT st.session
		     FROM staged st
		     CROSS JOIN active_lens x
		     LEFT JOIN progress p ON p.session = st.session AND p.lens = x.lens
		    WHERE COALESCE(st.session, '') != ''
		      AND (COALESCE(p.next_attempt,'') = '' OR COALESCE(p.next_attempt,'') <= ?))`, args...).Scan(&st.Pending)
	// BackedOff counts distinct sessions with an ACTIVE lens currently sleeping on a
	// retry. Filter to active lenses (like Pending): a backoff row left on a disabled
	// lens is inert (never mined), so counting it would make `distill start --all` and
	// `lens backfill` falsely report "incomplete" when every active lens is caught up.
	backoffValues, backoffArgs := lensValuesClause(activeLensList(lenses))
	bargs := append([]any{}, backoffArgs...)
	bargs = append(bargs, now)
	_ = s.db.QueryRow(
		`WITH active_lens(lens) AS (`+backoffValues+`)
		 SELECT COUNT(DISTINCT p.session)
		   FROM progress p JOIN active_lens x ON x.lens = p.lens
		  WHERE p.next_attempt != '' AND p.next_attempt > ?`, bargs...).Scan(&st.BackedOff)
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

// ImportLock serializes a platform's import from its external source. The importer
// is watermark-based, but concurrent importers can otherwise read the same count
// and append the same text rows twice. name identifies the source (the platform
// owns it, so the store stays platform-agnostic) and keys a per-source lock file.
func (s *Store) ImportLock(name string) (unlock func(), ok bool) {
	return s.lockFile("." + name + "-sync.lock")
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
