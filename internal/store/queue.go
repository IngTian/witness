package store

import (
	"database/sql"
	"strings"
	"time"
)

// queue is the distillation-queue concern: the per-(session, lens) `progress`
// watermark, retry/backoff bookkeeping, the pending-work query, and the doctor Stats
// snapshot. A DB leaf — holds only the shared *sql.DB.
//
// The watermark is per-(session, lens) (issue #55): each lens tracks how far it has
// mined a session independently, so enabling a NEW lens backfills only that lens
// over history without re-mining the always-on `default` (or any other) lens. Every
// queue op takes a lens; the #49-C2 raw-generation CAS is preserved, now also
// matching on lens. Stats.LastReview reads the `meta` review stamp via the shared
// metaGet primitive rather than the metaKV type, keeping this concern a leaf.
type queue struct{ db *sql.DB }

// DistilledCount returns the watermark for one (session, lens): how many L0 records
// this lens has already mined for the session. 0 if absent (a lens that has never
// touched this session — e.g. a just-enabled lens — reads 0, so its whole history
// is pending).
func (q *queue) DistilledCount(session, lens string) int {
	var n int
	_ = q.db.QueryRow(`SELECT distilled FROM progress WHERE session = ? AND lens = ?`, session, lens).Scan(&n)
	return n
}

// HasLegacyDefaultData reports whether this archive ever distilled under the old
// always-on "default" lens — i.e. a `progress` row exists for lens 'default' (#44
// slice 1a migration gate). It is the precise "is this a pre-1a archive?" signal the
// default-seed migration keys on: a FRESH install has no progress rows (→ false, so
// install/init decides whether to scaffold default, not the migration), and a
// library-mode archive that only ever ran its own domain lens has progress only under
// that lens (→ false, so the migration correctly does NOT force person-growth default
// onto it). An archive with real default history → true → seed+enable default so the
// install keeps working exactly as before.
func (q *queue) HasLegacyDefaultData() bool {
	var n int
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM progress WHERE lens = ?`, LensDefault).Scan(&n)
	return n > 0
}

// IsEmptyArchive reports whether this archive has never captured or distilled
// anything: no L0 raw rows AND no per-(session,lens) progress watermarks. It is the
// "brand-new archive" signal the first-open default-seed keys on together with an
// empty lens registry — a fresh personal install (install opens the store to bind the
// runner before any capture) reads empty here, whereas a library-mode archive that has
// already ingested records + mined its own domain lens does NOT (so default is never
// forced onto it). Read-only.
func (q *queue) IsEmptyArchive() bool {
	var n int
	_ = q.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM raw) + (SELECT COUNT(*) FROM progress)`).Scan(&n)
	return n == 0
}

// PendingInputChars returns the character size of a session's LARGEST undistilled
// per-lens delta among the ACTIVE lenses: SUM(LENGTH(text)) over the raw rows past
// the MINIMUM watermark those lenses hold on the session (an absent (session,lens)
// row = watermark 0). The engine sizes mining concurrency against this so many tiny
// sessions run wide but a few 200K-token ones don't all mine at once and OOM (issue
// #56 B2).
//
// Why the MIN over active lenses, treating absent as 0: MineSession mines each active
// lens over ITS OWN delta (raw[DistilledCount(session,lens):], sequentially), and
// DistilledCount reads 0 for a lens with no progress row. So the biggest single
// runner input for this session is raw past the most-behind active lens's watermark —
// which is exactly this MIN. That makes it a sound UPPER bound on the per-lens input
// this session will feed a runner:
//   - day-one backfill: no progress rows → MIN 0 → the whole session (the 566-turn
//     giant the issue describes);
//   - `lens backfill codereview` after default is caught up: codereview has no row →
//     its 0 pulls the MIN to 0 → still the whole session, so the per-lens backfill of
//     a giant is throttled too (the issue calls this path out explicitly);
//   - steady state (small deltas on caught-up sessions): MIN is high → just the new
//     tail, so ordinary re-distills are never over-throttled.
//
// A stale watermark on a since-DISABLED lens is excluded (it's not in `lenses`), so it
// can't skew the estimate — matching which lenses MineSession will actually run.
//
// `lenses` follows the same active-set contract as PendingSessions (empty → default).
// LENGTH counts UTF-8 characters (consistent with the OpenCode chunker's char
// budget), a fine proxy for the runner child's footprint. Read-only; 0 for a
// fully-distilled or absent session.
func (q *queue) PendingInputChars(session string, lenses []string) int {
	if emptyLensSet(lenses) {
		return 0 // no active lens → nothing this session would feed a runner
	}
	lensValues, lensArgs := lensValuesClause(activeLensList(lenses))
	args := append([]any{}, lensArgs...)
	args = append(args, session, session)
	var n int
	_ = q.db.QueryRow(
		`WITH active_lens(lens) AS (`+lensValues+`)
		 SELECT COALESCE(SUM(LENGTH(text)), 0) FROM (
		   SELECT text FROM raw WHERE session = ? ORDER BY id
		   LIMIT -1 OFFSET (
		     SELECT COALESCE(MIN(COALESCE(p.distilled, 0)), 0)
		       FROM active_lens x
		       LEFT JOIN progress p ON p.session = ? AND p.lens = x.lens
		   )
		 )`, args...).Scan(&n)
	return n
}

// DriftAt returns the RFC3339 stamp of the last prose-drift for one (session, lens),
// or "" if that pair never drifted / has since re-mined cleanly (#69 Part 2). This is
// the point-read accessor for the persisted per-(session,lens) drift; Stats.Drifted
// aggregates it. Absent row also reads "".
func (q *queue) DriftAt(session, lens string) string {
	var at string
	_ = q.db.QueryRow(`SELECT COALESCE(drift_at, '') FROM progress WHERE session = ? AND lens = ?`, session, lens).Scan(&at)
	return at
}

// MarkDistilled records the watermark (count of L0 records this lens has mined) and
// stamps when it advanced (for review cadence). Written LAST in Process so a
// crash mid-distill re-runs the delta; obsID dedup keeps that from duplicating.
func (q *queue) MarkDistilled(session, lens string, count int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := q.db.Exec(
		`INSERT INTO progress(session, lens, distilled, distilled_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(session, lens) DO UPDATE SET distilled = excluded.distilled, distilled_at = excluded.distilled_at`,
		session, lens, count, now)
	return err
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
func (q *queue) MarkDistilledIfCurrent(session, lens string, count int, rawHighID int64) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	// The guard: only write when a raw row with the mined generation's high id still
	// exists for this session. rawHighID==0 means the mine saw an empty session
	// (nothing mined) — still gate on "no raw exists now" so a concurrent import that
	// added rows isn't clobbered. The generation is a property of the session's raw,
	// not the lens, so the guard is unchanged in spirit — only the row it writes/
	// conflicts on is now keyed by (session, lens).
	res, err := q.db.Exec(
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
func (q *queue) RetryCount(session, lens string) int {
	var n int
	_ = q.db.QueryRow(`SELECT retries FROM progress WHERE session = ? AND lens = ?`, session, lens).Scan(&n)
	return n
}

// IncRetry bumps the (session, lens) failure count and returns the new value.
func (q *queue) IncRetry(session, lens string) int {
	_, _ = q.db.Exec(
		`INSERT INTO progress(session, lens, retries) VALUES (?, ?, 1)
		 ON CONFLICT(session, lens) DO UPDATE SET retries = retries + 1`, session, lens)
	return q.RetryCount(session, lens)
}

// ResetRetry clears the (session, lens) failure count, any backoff, AND any stale
// prose-drift stamp (a clean distill). Clearing drift_at here is the recovery path
// for #69 Part 2: once a lens re-mines this session successfully, the previously
// recorded drift no longer reflects reality, so it stops counting toward
// Stats.Drifted. (The commit path re-stamps drift_at AFTER this reset when the fresh
// mine itself drifted — see worker.go — so a still-drifting lens is not cleared.)
// No caller depends on ResetRetry leaving drift_at untouched.
func (q *queue) ResetRetry(session, lens string) {
	_, _ = q.db.Exec(`UPDATE progress SET retries = 0, next_attempt = '', drift_at = '' WHERE session = ? AND lens = ?`, session, lens)
}

// SetNextAttempt records the earliest time this (session, lens) should be retried.
// The pending query excludes the pair until then, so a failing lens backs off
// instead of being hammered every trigger — and a healthy sibling lens on the same
// session is unaffected (independent watermarks).
func (q *queue) SetNextAttempt(session, lens string, at time.Time) error {
	v := at.UTC().Format(time.RFC3339)
	_, err := q.db.Exec(
		`INSERT INTO progress(session, lens, next_attempt) VALUES (?, ?, ?)
		 ON CONFLICT(session, lens) DO UPDATE SET next_attempt = excluded.next_attempt`, session, lens, v)
	return err
}

// SetDrift stamps the (session, lens) row with the current time (RFC3339) to record
// that this pass drifted — the model returned no observation array for this lens
// (issue #69 Part 2). Persisting it lets Stats.Drifted / doctor report which
// sessions currently sit at a drift, surviving worker restarts, where the meta
// counter (RecordDrift) only tracks a lifetime event total. Upsert-safe like
// SetNextAttempt so it records drift whether or not a progress row exists yet
// (a drift-only row reads distilled=0, i.e. identical to absent for the pending
// query — so creating one never falsely marks turns as mined). ResetRetry clears
// this on a clean re-mine, so a stamp reflects the LAST outcome, not history.
func (q *queue) SetDrift(session, lens string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := q.db.Exec(
		`INSERT INTO progress(session, lens, drift_at) VALUES (?, ?, ?)
		 ON CONFLICT(session, lens) DO UPDATE SET drift_at = excluded.drift_at`, session, lens, now)
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
func (q *queue) LensBackedOff(session, lens string, now time.Time) bool {
	var next string
	_ = q.db.QueryRow(
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
func (q *queue) ResetLensWatermark(lens string) (int, error) {
	res, err := q.db.Exec(`DELETE FROM progress WHERE lens = ?`, lens)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// LensDataCounts reports how many L1 observations + L2 facets a lens currently has,
// without touching them — the read-only preview a destructive `lens backfill --fresh`
// shows before calling DeleteLensData, so the user sees exactly what a confirm will drop.
func (q *queue) LensDataCounts(lens string) (obs, facets int) {
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE lens = ?`, lens).Scan(&obs)
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM facets WHERE lens = ?`, lens).Scan(&facets)
	return obs, facets
}

// ActiveObservationCount reports how many of a lens's L1 observations were RECORDED
// in-session (source='active', via the MCP record_observation tool / `observations
// record`) rather than mined by the worker. These are NOT reproducible by a re-mine —
// they were passed through verbatim from the agent and have no L0 turn the miner would
// re-extract them from — so a `lens backfill --fresh` (which drops all of a lens's obs)
// destroys them irrecoverably. The CLI counts these to warn before a --fresh drop, so
// the "safe, re-derivable from L0" story is only told when it is actually true (zero
// active obs). Read-only.
func (q *queue) ActiveObservationCount(lens string) int {
	var n int
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE lens = ? AND source = 'active'`, lens).Scan(&n)
	return n
}

// DeleteLensData removes one lens's derived L1 observations and L2 facets (for a
// `lens backfill --fresh`: re-mine a lens from scratch after its prompt changed). Raw L0
// is the durable record and is NOT touched. facet_versions cascade via the ON DELETE
// CASCADE foreign key. Runs in one transaction so a `--fresh` drop never leaves half a
// lens's derived data behind. Returns (observations, facets) removed.
//
// NOTE: "re-mine from scratch" fully restores only MINED observations (source='mined'),
// which the worker re-extracts from the untouched L0. In-session ACTIVE observations
// (source='active') are NOT re-created by a re-mine and are lost — callers that offer
// this destructively (the --fresh path) must warn on ActiveObservationCount > 0 first.
func (q *queue) DeleteLensData(lens string) (obs, facets int, err error) {
	tx, err := q.db.Begin()
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
func (q *queue) NextBackoffAttempt(lenses []string, now time.Time) (time.Time, bool) {
	if emptyLensSet(lenses) {
		return time.Time{}, false // no active lens → no outstanding retry to wait for
	}
	lensValues, lensArgs := lensValuesClause(activeLensList(lenses))
	args := append([]any{}, lensArgs...)
	args = append(args, now.UTC().Format(time.RFC3339))
	var v string
	_ = q.db.QueryRow(
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
func (q *queue) PendingSessions(lenses []string) ([]string, error) {
	return q.PendingSessionsUpdatedBetween(lenses, time.Time{}, time.Time{})
}

// PendingSessionsUpdatedBetween applies an optional inclusive range to each
// session's most recent raw timestamp. Zero bounds are open. Sessions with no
// timestamp remain eligible only when no range is requested. A session is pending
// when ANY active lens is behind on it (raw count > that lens's watermark) and that
// lens's (session,lens) backoff has elapsed — a per-lens backoff no longer parks
// the whole session, only that lens's share of it.
func (q *queue) PendingSessionsUpdatedBetween(lenses []string, since, until time.Time) ([]string, error) {
	lenses = activeLensList(lenses)
	if len(lenses) == 0 {
		return nil, nil // no active lens → nothing is pending (drain no-ops)
	}
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
	rows, err := q.db.Query(
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

// activeLensList normalizes the caller's active-lens set. Since #44 slice 1a the
// default lens is an ORDINARY registered lens with no always-on privilege, so an
// install may legitimately have ZERO active lenses (the user disabled/registered
// none) — that is not degenerate, it just means "nothing to distill." This no
// longer injects a fallback lens; it returns the list as-is (a nil/empty list stays
// empty). Every consumer must guard the empty case BEFORE building a lens CTE,
// because lensValuesClause(nil) would emit a bare `VALUES ` (invalid SQL) — see
// emptyLensSet and its callers.
func activeLensList(lenses []string) []string { return lenses }

// emptyLensSet reports whether the active-lens set is empty. When true, every
// queue query that cross-joins against the active lenses has no lens to match, so
// the answer is trivially empty/zero and the caller returns WITHOUT touching the DB
// — avoiding the malformed `VALUES ` clause lensValuesClause would produce on nil.
func emptyLensSet(lenses []string) bool { return len(activeLensList(lenses)) == 0 }

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
	Drifted      int    // distinct sessions with a lens currently sitting at a prose-drift (#69 Part 2)
	LastReview   string // RFC3339 of the last reviewer run ("" = never)
}

// Stats gathers the doctor snapshot in a few cheap indexed queries. `lenses` is the
// active-lens name set (same contract as PendingSessions): Pending counts distinct
// sessions for which some active lens is behind, so the doctor count matches what
// the worker will actually drain. BackedOff counts distinct sessions that have at
// least one (session,lens) currently sleeping on a retry backoff.
func (q *queue) Stats(lenses []string) Stats {
	now := time.Now().UTC().Format(time.RFC3339)
	var st Stats
	_ = q.db.QueryRow(`SELECT COUNT(DISTINCT session) FROM raw`).Scan(&st.Sessions)
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM raw`).Scan(&st.RawRecords)
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&st.Observations)
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM facets`).Scan(&st.Facets)
	// Pending + BackedOff cross-join against the active-lens set. With ZERO active
	// lenses (default now an ordinary lens that can be absent, #44 slice 1a) there is
	// nothing to distill, so both are trivially 0 — and we must NOT build the lens CTE
	// (lensValuesClause(nil) → invalid `VALUES `). The unconditional counts above and
	// Drifted/LastReview below still reflect the archive regardless of active lenses.
	if !emptyLensSet(lenses) {
		lensValues, lensArgs := lensValuesClause(activeLensList(lenses))
		args := append([]any{}, lensArgs...)
		args = append(args, now, now)
		_ = q.db.QueryRow(
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
		_ = q.db.QueryRow(
			`WITH active_lens(lens) AS (`+backoffValues+`)
			 SELECT COUNT(DISTINCT p.session)
			   FROM progress p JOIN active_lens x ON x.lens = p.lens
			  WHERE p.next_attempt != '' AND p.next_attempt > ?`, bargs...).Scan(&st.BackedOff)
	}
	// Drifted: distinct sessions with ANY lens currently sitting at a prose-drift
	// stamp (#69 Part 2). Unlike Pending/BackedOff this is NOT scoped to the active
	// lens set — a drift is a persisted fact about a past mine of that (session,lens),
	// so it counts wherever it was recorded; a clean re-mine clears it via ResetRetry.
	// A plain COUNT(DISTINCT) is cheap at this scale (no dedicated index — the drift_at
	// != '' predicate is a rare-true filter over the small progress table).
	_ = q.db.QueryRow(`SELECT COUNT(DISTINCT session) FROM progress WHERE drift_at != ''`).Scan(&st.Drifted)
	st.LastReview = metaGet(q.db, "review_ts")
	return st
}
