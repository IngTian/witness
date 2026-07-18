package store

import (
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// metaKV is the small-scalar bookkeeping concern: the `meta` key/value table (review
// offsets, importer watermarks, the runner-bound flag, drift counters) and the
// non-content `session_meta` facts (cwd/started/platform). It is a leaf — it holds
// only the shared *sql.DB and shares no state with any other concern — so it can be
// exercised (and, at the Phase-B seam, mocked) on its own.
type metaKV struct{ db *sql.DB }

// metaGet reads one `meta` value (empty string if absent). A free function, not a
// method, because several concerns need this exact primitive (metaKV's own readers,
// the queue's Stats.LastReview, the config layer's review cadence) without taking a
// dependency on the metaKV type — keeping every concern a leaf.
func metaGet(db *sql.DB, key string) string {
	var v string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

// metaGetInt reads a `meta` value parsed as an int64 (0 if absent/unparseable).
func metaGetInt(db *sql.DB, key string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(metaGet(db, key)), 10, 64)
	return n
}

// metaSet upserts one `meta` value. The shared write primitive behind SetMetaString.
func metaSet(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// MetaString exposes small scalar bookkeeping to importers that need their own
// durable watermarks without owning schema migrations.
func (m *metaKV) MetaString(key string) string { return metaGet(m.db, key) }

// SetMetaString stores a small scalar watermark under key.
func (m *metaKV) SetMetaString(key, value string) error { return metaSet(m.db, key, value) }

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
func (m *metaKV) RecordMeta(sm SessionMeta) {
	_, _ = m.db.Exec(
		`INSERT OR IGNORE INTO session_meta(session, cwd, started) VALUES (?, ?, ?)`,
		sm.Session, sm.Cwd, sm.Started)
}

// ReadMeta returns a session's meta (zero value if absent). Currently has no
// callers — see the RecordMeta note above (cwd/session_meta is retained for the
// OpenCode writer and possible future use, not read on any live path).
func (m *metaKV) ReadMeta(session string) SessionMeta {
	sm := SessionMeta{Session: session}
	_ = m.db.QueryRow(`SELECT cwd, started FROM session_meta WHERE session = ?`, session).
		Scan(&sm.Cwd, &sm.Started)
	return sm
}

// SetSessionPlatform records which platform OWNS a session (the per-session axis,
// issue #21) — distinct from the default distillation runner. Upsert, since a CC
// session has no session_meta row until now: INSERT the row if absent, else set
// only the platform column (leaving cwd/started untouched). Best-effort at the
// capture/import boundary; ForSession falls back to the id prefix when unset, so a
// missed write degrades to the same answer rather than misclassifying.
func (m *metaKV) SetSessionPlatform(session, platform string) {
	if session == "" || platform == "" {
		return
	}
	_, _ = m.db.Exec(
		`INSERT INTO session_meta(session, platform) VALUES (?, ?)
		 ON CONFLICT(session) DO UPDATE SET platform = excluded.platform`,
		session, platform)
}

// SessionPlatform returns the recorded owning platform for a session, or "" if no
// row exists or it was never classified. ForSession layers the prefix/default rule
// on top of this — this method only reports the persisted column.
func (m *metaKV) SessionPlatform(session string) string {
	var p string
	_ = m.db.QueryRow(`SELECT platform FROM session_meta WHERE session = ?`, session).Scan(&p)
	return p
}

// Drift-surfacing meta keys (issue #57). A distillation pass records prose_drift —
// a lens whose model returned no JSON observation array, likely a below-floor triage
// model — so `witness doctor` and a backfill's completion line can surface it. These
// live in the existing worker_*/review_* meta namespace (no schema, no collision).
// The total is monotonic across the archive's life; the last_* stamps give doctor a
// "when/where" so a raised model reads as "0 recent", not "broken forever".
const (
	metaDriftTotal       = "mine_drift_total"
	metaDriftLastTS      = "mine_drift_last_ts"
	metaDriftLastSession = "mine_drift_last_session"
	metaDriftLastLens    = "mine_drift_last_lens"
)

// RecordDrift atomically adds n to the drift counter and stamps the most recent drift
// (ts/session/lens), in ONE transaction so a concurrent reader never sees the counter
// bumped without its stamps. Called by the sole L1 writer (CommitMining), so there is
// no cross-process race on the counter beyond what the single-writer model already
// serializes. Best-effort at the call site: a failure here never fails the commit.
func (m *metaKV) RecordDrift(n int, session, lens string) error {
	if n <= 0 {
		return nil
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	// Atomic increment: read-modify-write inside the tx would still be correct under
	// MaxOpenConns(1), but expressing it as one UPDATE keeps it a single statement and
	// robust if the connection model ever loosens.
	if _, err := tx.Exec(
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = CAST(meta.value AS INTEGER) + excluded.value`,
		metaDriftTotal, strconv.Itoa(n)); err != nil {
		tx.Rollback()
		return err
	}
	stamps := [][2]string{
		{metaDriftLastTS, time.Now().UTC().Format(time.RFC3339)},
		{metaDriftLastSession, session},
		{metaDriftLastLens, lens},
	}
	for _, kv := range stamps {
		if _, err := tx.Exec(
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DriftTotal is the monotonic count of prose_drift events recorded so far (0 if never).
func (m *metaKV) DriftTotal() int {
	n, _ := strconv.Atoi(metaGet(m.db, metaDriftTotal))
	return n
}

// DriftLast returns the timestamp and lens of the most recently recorded drift event
// (both "" if none) — for doctor's "last drift" line so a monotonic counter reads as
// dated, not perpetually-broken.
func (m *metaKV) DriftLast() (ts, lens string) {
	return metaGet(m.db, metaDriftLastTS), metaGet(m.db, metaDriftLastLens)
}
