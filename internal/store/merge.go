package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Merge folds ANOTHER witness archive into this one — a one-time CONSOLIDATION (retiring a
// laptop, moving to a machine that already has its own archive), not a sync. Restoring by copying
// a backup into the data dir replaces the destination; this keeps both.
//
// WHY IT IS NOT A SYNC. There are no tombstones (cleanup and delete_observation leave no record),
// so a session or observation deleted on one side comes back from the other; and two machines
// that keep capturing between merges each grow a genuinely diverged chain for every facet both
// touched, which this resolves by archiving one copy — one more archived copy per facet per round.
// Repeated one-way merges are SAFE (every rule below is idempotent on natural keys), but a two-way
// sync needs tombstones and a three-way facet merge, which this deliberately is not.
//
// THE SHAPE. The incoming database is never opened in place: Open writes into a data dir (config,
// a lens migration, the default-lens seed), and a backup must stay a backup. It is copied to a
// temp file, migrated THERE, and everything that has to change on the incoming side — renaming a
// diverged session, dropping observations this archive already mined — is done to that copy. The
// copy is then ATTACHed and the whole merge runs in ONE transaction on a pinned connection, so it
// lands completely or not at all, and a dry run is the same transaction rolled back.
//
// The model is never called. Facets are carried across with their full histories, so both sides
// keep their real dates; duplicates the two archives minted under DIFFERENT keys are left for
// `witness compact` to merge by meaning, which is a model's judgment and so is not done here.
// Same-key duplicates are resolved in Go: same key, same lens prompt, same person is the strongest
// duplicate signal there is, and compaction cannot merge a pair (a merge must absorb two facets).
func (s *Store) Merge(ctx context.Context, incomingDB string, apply bool, now time.Time) (*MergeReport, error) {
	if _, err := os.Stat(incomingDB); err != nil {
		return nil, fmt.Errorf("read %s: %w", incomingDB, err)
	}
	// A live database cannot be copied consistently without its WAL — that torn copy is the exact
	// corruption `witness export` exists to avoid. Refuse it rather than merge a partial archive.
	if _, err := os.Stat(incomingDB + "-wal"); err == nil {
		return nil, ErrMergeLiveArchive
	}

	tmpDir, err := os.MkdirTemp("", "witness-merge-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	copyPath := filepath.Join(tmpDir, "incoming.db")
	if err := copyFile0600(incomingDB, copyPath); err != nil {
		return nil, fmt.Errorf("copy %s: %w", incomingDB, err)
	}
	from, err := peekUserVersion(copyPath)
	if err != nil {
		return nil, fmt.Errorf("%s is not a witness database: %w", incomingDB, err)
	}
	if from > schemaVersion {
		return nil, fmt.Errorf("%s was written by a newer witness (schema v%d; this one reads up to v%d) — upgrade witness first",
			incomingDB, from, schemaVersion)
	}

	rep := &MergeReport{Source: incomingDB, IncomingSchema: from}
	inc, err := s.readIncoming(copyPath, rep)
	if err != nil {
		return nil, err
	}
	base, err := s.readBaseSide()
	if err != nil {
		return nil, err
	}
	if err := s.mergeTx(ctx, copyPath, inc, base, rep, apply, now); err != nil {
		return nil, err
	}
	return rep, nil
}

// ErrMergeLiveArchive is returned for an incoming database that has a -wal file beside it: a
// running archive's data directory rather than a snapshot.
var ErrMergeLiveArchive = errors.New("that is a live data directory (it has a witness.db-wal), not a snapshot; " +
	"export it on the machine that owns it — `witness export --all <newdir>` — and merge that")

// MergeReport is what a merge did, or — for a dry run — would do. Every count comes from the same
// transaction the apply would commit, so the dry run cannot disagree with it.
type MergeReport struct {
	Source         string
	IncomingSchema int      // incoming user_version BEFORE its temp copy was migrated
	FirstTS        string   // incoming raw time range, so a stale or wrong export is obvious
	LastTS         string   //
	Cwds           []string // the incoming archive's most common working dirs: "is this mine?"
	Lenses         []string // lens names present in the incoming data

	Sessions     MergeSessions
	RawRows      int
	Observations MergeObservations
	Staged       int
	Facets       MergeFacets
}

// MergeSessions classifies every incoming session against this archive.
type MergeSessions struct {
	Incoming    int
	New         int // not here at all
	Equal       int // identical transcript already here
	LongerThere int // the incoming transcript extends ours: its tail is appended
	LongerHere  int // ours extends the incoming one: nothing to do
	Appended    int // raw rows appended across the LongerThere sessions
	// Diverged sessions share an id but not a transcript — two machines ingesting the same named
	// `file:` feed, typically. Ours keeps the id; the incoming one is renamed so its transcript AND
	// what was mined from it arrive together, instead of its observations landing under our
	// transcript.
	Diverged []RenamedSession
}

// RenamedSession is a diverged incoming session and the id it now has here.
type RenamedSession struct{ From, To string }

// MergeObservations counts L1.
type MergeObservations struct {
	Reviewed    int // already folded into the incoming facets: arrive BELOW the review cursor
	Unreviewed  int // folded on neither side: arrive above it, for the next review
	AlreadyHere int
	// SharedMined: observations of a session THIS archive already mined for that lens. Mining is
	// not deterministic, so the incoming copy is a paraphrase with different obs_ids — union them
	// and every such moment is counted twice, permanently, with no column to tell the copies apart.
	SharedMined int
}

// MergeFacets counts L2.
type MergeFacets struct {
	Incoming      int
	New           int // no facet here with that key or shared history
	Shared        int // same history already here (a restore, or a re-run): nothing new
	FastForwarded int // same history, but the incoming side recorded later versions: adopted
	SameKey       []SameKeyFacet
}

// SameKeyFacet is a key both archives minted with DIFFERENT histories. One keeps the key; the
// other moves to LoserKey with its whole history, archived into the winner if both were active.
type SameKeyFacet struct {
	Lens, Dimension, Key, LoserKey string
	IncomingWon                    bool
}

// Empty reports whether the merge changes nothing at all — the second run of the same merge.
func (r *MergeReport) Empty() bool {
	return r.Sessions.New == 0 && r.Sessions.LongerThere == 0 && len(r.Sessions.Diverged) == 0 &&
		r.RawRows == 0 && r.Observations.Reviewed == 0 && r.Observations.Unreviewed == 0 &&
		r.Staged == 0 && r.Facets.New == 0 && r.Facets.FastForwarded == 0 && len(r.Facets.SameKey) == 0
}

// mergeSide is one archive's state as read BEFORE the merge transaction. Observations and facets
// change only under WorkerLock (the caller holds it), so reading them outside the transaction is
// consistent; raw rows can still arrive from a capture hook, so raw is read inside it.
type mergeSide struct {
	head    int64            // MAX(observations.seq): the decay clock's "now" on this side
	lastTS  string           // MAX(observations.ts): how recent this side's evidence runs
	cursors map[string]int64 // review_rowid:<lens>
	facets  []Facet
	obsTS   map[string]string // obs_id -> ts, for evidence time
}

func (s *Store) readBaseSide() (*mergeSide, error) {
	side, err := readSide(s.db)
	if err != nil {
		return nil, fmt.Errorf("read this archive: %w", err)
	}
	return side, nil
}

func readSide(db *sql.DB) (*mergeSide, error) {
	side := &mergeSide{cursors: map[string]int64{}, obsTS: map[string]string{}}
	if err := db.QueryRow(`SELECT COALESCE(MAX(seq), 0), COALESCE(MAX(NULLIF(ts, '')), '') FROM observations`).
		Scan(&side.head, &side.lastTS); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT key, value FROM meta WHERE key LIKE 'review_rowid:%'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		side.cursors[strings.TrimPrefix(k, "review_rowid:")] = n
	}
	rows.Close()
	ts, err := db.Query(`SELECT obs_id, ts FROM observations`)
	if err != nil {
		return nil, err
	}
	for ts.Next() {
		var id, t string
		if err := ts.Scan(&id, &t); err != nil {
			ts.Close()
			return nil, err
		}
		side.obsTS[id] = t
	}
	ts.Close()
	if side.facets, err = (&facetIO{db: db}).ReadFacets(); err != nil {
		return nil, err
	}
	return side, nil
}

// readIncoming migrates the temp copy and reads the incoming side, plus the report header.
func (s *Store) readIncoming(copyPath string, rep *MergeReport) (*mergeSide, error) {
	db, err := openDB(copyPath) // migrates the COPY only
	if err != nil {
		return nil, fmt.Errorf("open incoming copy: %w", err)
	}
	defer db.Close()
	side, err := readSide(db)
	if err != nil {
		return nil, fmt.Errorf("read incoming archive: %w", err)
	}
	if err := db.QueryRow(`SELECT COALESCE(MIN(NULLIF(ts,'')),''), COALESCE(MAX(NULLIF(ts,'')),'') FROM raw`).
		Scan(&rep.FirstTS, &rep.LastTS); err != nil {
		return nil, err
	}
	cw, err := db.Query(`SELECT cwd FROM session_meta WHERE cwd <> '' GROUP BY cwd ORDER BY COUNT(*) DESC, cwd LIMIT 3`)
	if err != nil {
		return nil, err
	}
	for cw.Next() {
		var c string
		_ = cw.Scan(&c)
		rep.Cwds = append(rep.Cwds, c)
	}
	cw.Close()
	// Lens names arrive as DATA here (observations, facets, progress) — not through RegisterLens —
	// so they get the same name rule here, before anything is written.
	ln, err := db.Query(`SELECT lens FROM observations UNION SELECT lens FROM facets UNION SELECT lens FROM progress`)
	if err != nil {
		return nil, err
	}
	for ln.Next() {
		var l string
		_ = ln.Scan(&l)
		if l == "" {
			continue
		}
		if err := ValidLensName(l); err != nil {
			ln.Close()
			return nil, fmt.Errorf("incoming archive holds data for an invalid lens: %w", err)
		}
		rep.Lenses = append(rep.Lenses, l)
	}
	ln.Close()
	sort.Strings(rep.Lenses)
	return side, nil
}

const obsCols = "obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding"

func prefixCols(alias string) string {
	parts := strings.Split(obsCols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// mergeTx is the whole merge inside one transaction on a pinned connection with the incoming copy
// ATTACHed as `inc`.
func (s *Store) mergeTx(ctx context.Context, copyPath string, inc, base *mergeSide, rep *MergeReport, apply bool, now time.Time) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// ATTACH is not allowed inside a transaction, so it precedes the BEGIN and the DETACH follows
	// the end of it (defers run last-registered-first: rollback, then detach, then close).
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS inc`, copyPath); err != nil {
		return fmt.Errorf("attach incoming copy: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE inc`)
	tx, err := beginImmediateOn(ctx, conn)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	kinds, err := classifySessions(ctx, tx, rep)
	if err != nil {
		return fmt.Errorf("classify sessions: %w", err)
	}
	if err := dropSharedMined(ctx, tx, rep); err != nil {
		return fmt.Errorf("skip already-mined sessions: %w", err)
	}
	if err := mergeL0(ctx, tx, kinds, rep); err != nil {
		return fmt.Errorf("merge transcripts: %w", err)
	}
	extraBase, extraInc, err := s.mergeObservations(ctx, tx, inc, base, rep)
	if err != nil {
		return fmt.Errorf("merge observations: %w", err)
	}
	var m int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM main.observations`).Scan(&m); err != nil {
		return err
	}
	merged := mergeFacets(base, inc, m, extraBase, extraInc, now, &rep.Facets)
	if err := writeFacetsTx(tx, merged); err != nil {
		return fmt.Errorf("write facets: %w", err)
	}
	for _, q := range []string{`DROP TABLE IF EXISTS temp.merge_kind`, `DROP TABLE IF EXISTS temp.merge_moved`,
		`DROP TABLE IF EXISTS temp.merge_inc_cur`, `DROP TABLE IF EXISTS temp.merge_base_cur`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if !apply {
		return nil // the deferred rollback discards everything; the report stands
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// Session kinds.
const (
	kindNew         = "new"
	kindEqual       = "equal"
	kindLongerThere = "longer_there"
	kindLongerHere  = "longer_here"
)

type rawTurn struct{ role, text string }

func readTurns(ctx context.Context, tx *sql.Tx, schema, session string) ([]rawTurn, error) {
	rows, err := tx.QueryContext(ctx, `SELECT role, text FROM `+schema+`.raw WHERE session = ? ORDER BY id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rawTurn
	for rows.Next() {
		var t rawTurn
		if err := rows.Scan(&t.role, &t.text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// classifySessions compares every incoming transcript with ours and renames the diverged ones in
// the incoming copy. Identity is the ordered (role, text) list: `seq` is a plain ordinal with no
// uniqueness, and `id` is a per-archive surrogate.
func classifySessions(ctx context.Context, tx *sql.Tx, rep *MergeReport) (map[string]string, error) {
	sessions, err := querySessions(ctx, tx, `SELECT DISTINCT session FROM inc.raw ORDER BY session`)
	if err != nil {
		return nil, err
	}
	rep.Sessions.Incoming = len(sessions)
	kinds := map[string]string{}
	var diverged []string
	for _, sess := range sessions {
		k, err := compareSession(ctx, tx, sess)
		if err != nil {
			return nil, err
		}
		if k == "" {
			diverged = append(diverged, sess)
			continue
		}
		kinds[sess] = k
	}
	for _, sess := range diverged {
		to, err := renameIncomingSession(ctx, tx, sess)
		if err != nil {
			return nil, err
		}
		// Classified again under its new id: on a second run of the same merge the renamed copy is
		// already here, and must read as equal — not diverge into yet another id, and not be
		// reported as a change.
		k, err := compareSession(ctx, tx, to)
		if err != nil {
			return nil, err
		}
		if k == "" {
			return nil, fmt.Errorf("session %s diverges from ours even after renaming to %s", sess, to)
		}
		if k == kindNew {
			rep.Sessions.Diverged = append(rep.Sessions.Diverged, RenamedSession{From: sess, To: to})
		}
		kinds[to] = k
	}
	for _, k := range kinds {
		switch k {
		case kindNew:
			rep.Sessions.New++
		case kindEqual:
			rep.Sessions.Equal++
		case kindLongerThere:
			rep.Sessions.LongerThere++
		case kindLongerHere:
			rep.Sessions.LongerHere++
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE merge_kind(session TEXT PRIMARY KEY, kind TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	for sess, k := range kinds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO temp.merge_kind(session, kind) VALUES (?, ?)`, sess, k); err != nil {
			return nil, err
		}
	}
	return kinds, nil
}

// compareSession returns the kind of an incoming session, or "" if it diverges from ours.
func compareSession(ctx context.Context, tx *sql.Tx, sess string) (string, error) {
	theirs, err := readTurns(ctx, tx, "inc", sess)
	if err != nil {
		return "", err
	}
	ours, err := readTurns(ctx, tx, "main", sess)
	if err != nil {
		return "", err
	}
	if len(ours) == 0 {
		return kindNew, nil
	}
	n := min(len(ours), len(theirs))
	for i := 0; i < n; i++ {
		if ours[i] != theirs[i] {
			return "", nil
		}
	}
	switch {
	case len(theirs) == len(ours):
		return kindEqual, nil
	case len(theirs) > len(ours):
		return kindLongerThere, nil
	default:
		return kindLongerHere, nil
	}
}

func querySessions(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// renameIncomingSession gives a diverged incoming session an id of its own, in the incoming COPY,
// so its transcript and everything mined from it arrive together. The suffix is a hash of the
// transcript itself, not of the backup file, so merging a re-export of the same archive produces
// the same id — and the second run finds the renamed copy already here instead of minting another.
//
// Its importer keys are dropped: they describe the session under its native id, and no importer
// will ever see the renamed one.
func renameIncomingSession(ctx context.Context, tx *sql.Tx, sess string) (string, error) {
	turns, err := readTurns(ctx, tx, "inc", sess)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	for _, t := range turns {
		io.WriteString(h, t.role)
		h.Write([]byte{0})
		io.WriteString(h, t.text)
		h.Write([]byte{0})
	}
	to := sess + "@" + hex.EncodeToString(h.Sum(nil))[:8]
	for _, table := range []string{"raw", "session_meta", "progress", "observations", "staged"} {
		if _, err := tx.ExecContext(ctx, `UPDATE inc.`+table+` SET session = ? WHERE session = ?`, to, sess); err != nil {
			return "", fmt.Errorf("rename in %s: %w", table, err)
		}
	}
	for _, ns := range PerSessionMetaNamespaces {
		if _, err := tx.ExecContext(ctx, `DELETE FROM inc.meta WHERE key = ?`, ns+":"+sess); err != nil {
			return "", err
		}
	}
	return to, nil
}

// dropSharedMined removes, from the incoming copy, the MINED observations and progress of every
// (session, lens) this archive already mined. Mining is not deterministic: the incoming copy of a
// session both machines mined is a paraphrase with different obs_ids, so a union would count every
// such moment twice — permanently, since L1 is append-only and carries no source column. This is
// the one merge defect nothing can undo afterwards, so it is prevented here.
//
// "Already mined here" is EITHER a progress mark OR mined rows: cleanup removes a session's
// progress along with its transcript but keeps the observations, and those still count.
//
// Ours wins, not the larger count: our progress stays, so if the incoming transcript is longer its
// tail is appended (mergeL0) and mined here. Active observations (recorded in-session through MCP)
// are never mined and never paraphrased, so they are kept and deduped by obs_id like anything else.
func dropSharedMined(ctx context.Context, tx *sql.Tx, rep *MergeReport) error {
	const minedHere = `(EXISTS (SELECT 1 FROM main.progress p WHERE p.session = %[1]s.session AND p.lens = %[1]s.lens AND p.distilled > 0)
	   OR EXISTS (SELECT 1 FROM main.observations m WHERE m.session = %[1]s.session AND m.lens = %[1]s.lens AND m.source <> 'active'))`
	// Only PARAPHRASES go — rows whose obs_id we do not hold. An IDENTICAL row (a restore: same
	// session, same text, same id) is harmless, since the insert dedupes it, and it must stay: it is
	// the record that the incoming side already folded that observation. Deleting it too made our
	// copy look unreviewed, so mergeObservations moved it above the cursor and it was folded twice.
	res, err := tx.ExecContext(ctx, `DELETE FROM inc.observations WHERE source <> 'active' AND `+
		fmt.Sprintf(minedHere, "inc.observations")+
		` AND NOT EXISTS (SELECT 1 FROM main.observations same WHERE same.obs_id = inc.observations.obs_id)`)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	rep.Observations.SharedMined = int(n)
	_, err = tx.ExecContext(ctx, `DELETE FROM inc.progress WHERE `+fmt.Sprintf(minedHere, "inc.progress"))
	return err
}

// mergeL0 merges transcripts, session metadata, mining progress, importer keys, the staged buffer
// and the content-keyed emergent marks.
//
// The meta table is an ALLOW-list: only the per-session importer keys and emerge_seen marks cross
// over. Everything else there is the other machine's state, and copying it does damage:
//
//	review_rowid:*                 recomputed by mergeObservations; incoming values are only read
//	review_ts, review_obs_rowid    this archive's own review trigger
//	runner_bound                   flips how the runner is chosen on THIS machine
//	opencode_sync_time_updated_ms  a cursor into THIS machine's OpenCode db: a copied value makes
//	                               our own OpenCode history silently skip on the next sync
//	worker_*                       a live process: a copied worker_stop_requested stops our worker,
//	                               a copied worker_pid is what `distill stop` would signal
//	mine_drift_*, default_lens_migrated_v1a, profile_sig:*  local bookkeeping; profile_sig is stale
//	                               anyway once the facets change, so the portrait regenerates
func mergeL0(ctx context.Context, tx *sql.Tx, kinds map[string]string, rep *MergeReport) error {
	// raw: new sessions whole, in their original order (seq is copied — LastDistilledRawTS
	// compares it); a longer incoming transcript contributes only the tail we lack.
	res, err := tx.ExecContext(ctx, `INSERT INTO main.raw(session, seq, ts, role, effort, text)
		SELECT r.session, r.seq, r.ts, r.role, r.effort, r.text FROM inc.raw r
		JOIN temp.merge_kind k ON k.session = r.session AND k.kind = ? ORDER BY r.id`, kindNew)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	rep.RawRows = int(n)
	for sess, k := range kinds {
		if k != kindLongerThere {
			continue
		}
		var have int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.raw WHERE session = ?`, sess).Scan(&have); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO main.raw(session, seq, ts, role, effort, text)
			SELECT session, seq, ts, role, effort, text FROM inc.raw WHERE session = ? ORDER BY id LIMIT -1 OFFSET ?`, sess, have)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		rep.RawRows += int(n)
		rep.Sessions.Appended += int(n)
	}

	// session_meta: ours wins. ApplyRawImport and SetSessionPlatform both OVERWRITE, so they are
	// deliberately not reused for this.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.session_meta(session, cwd, started, platform)
		SELECT session, cwd, started, platform FROM inc.session_meta`); err != nil {
		return err
	}

	// progress: ours wins where we have mined; theirs fills in what we never mined. The rows for
	// sessions we already mined were dropped from the copy by dropSharedMined. Dropping progress
	// instead would make every incoming session pending for every lens — re-mined at model cost,
	// and, mining being non-deterministic, minting duplicate observations.
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.progress(session, lens, distilled, retries, distilled_at, next_attempt, drift_at)
		SELECT session, lens, distilled, retries, distilled_at, next_attempt, drift_at FROM inc.progress WHERE true
		ON CONFLICT(session, lens) DO UPDATE SET distilled = excluded.distilled, retries = excluded.retries,
		  distilled_at = excluded.distilled_at, next_attempt = excluded.next_attempt, drift_at = excluded.drift_at
		WHERE progress.distilled = 0 AND excluded.distilled > 0`); err != nil {
		return err
	}

	// Importer keys. Every importer requires RawCount(session) == len(keys); a mismatch makes the
	// next import REPLACE the session — deleting its raw and progress and re-mining it. So the keys
	// must describe exactly the rows that end up here.
	for _, ns := range PerSessionMetaNamespaces {
		rows, err := tx.QueryContext(ctx, `SELECT key, value FROM inc.meta WHERE key LIKE ? ESCAPE '\'`,
			strings.ReplaceAll(ns, "_", `\_`)+":%")
		if err != nil {
			return err
		}
		type kv struct{ k, v string }
		var keys []kv
		for rows.Next() {
			var e kv
			if err := rows.Scan(&e.k, &e.v); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, e)
		}
		rows.Close()
		for _, e := range keys {
			switch kinds[strings.TrimPrefix(e.k, ns+":")] {
			case kindLongerHere:
				// Ours has rows theirs never saw; their keys would cover fewer rows than we hold.
				continue
			case kindEqual:
				_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.meta(key, value) VALUES (?, ?)`, e.k, e.v)
			default: // new, longer_there, or a session with keys but no transcript
				_, err = tx.ExecContext(ctx, `INSERT INTO main.meta(key, value) VALUES (?, ?)
					ON CONFLICT(key) DO UPDATE SET value = excluded.value`, e.k, e.v)
			}
			if err != nil {
				return err
			}
		}
	}

	// staged: observations recorded in-session and not yet drained to L1. Each is the only copy.
	res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.staged(session, obs_id, payload)
		SELECT session, obs_id, payload FROM inc.staged ORDER BY id`)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	rep.Staged = int(n)

	// emerge_seen marks are keyed by a hash of member obs_ids — content, not position — so a
	// union is exact. Dropping one would only cost a re-verification.
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.meta(key, value)
		SELECT key, value FROM inc.meta WHERE key LIKE 'emerge\_seen:%' ESCAPE '\'`)
	return err
}

// mergeObservations unions L1 while keeping every lens's review cursor EXACT: afterwards, the rows
// at or below each lens's cursor are exactly those already folded into facets on their own side,
// and the rows above it are exactly those folded on neither.
//
// That needs the incoming FOLDED rows to sit below the cursor and every UNFOLDED row above it.
// Sequence numbers only grow (AUTOINCREMENT never reuses one), so:
//
//	O1  insert the incoming folded rows                          -> seqs up to g1
//	O2  re-append our unfolded rows and the incoming unfolded    -> seqs above g1, in TIME order
//	    rows together, interleaved by ts — so the next review
//	    reads them in the order they happened, not "ours, then
//	    theirs" (an old June row read after August could pass for
//	    a contradiction and reopen a value the August rows had moved past)
//	O3  set every lens's cursor to g1
//
// Get it wrong and it fails silently and expensively either way: the cursor too low re-reviews the
// whole incoming corpus and counts its evidence twice; too high and rows are never reviewed.
// Nothing refers to an observation by seq except cursors and decay clocks (citations are obs_ids),
// so renumbering our unfolded rows is safe. An observation folded on the incoming side but not yet
// here stays put — it is already in the incoming facets, and moving it above the cursor would fold
// it a second time.
//
// It returns the two decay-clock corrections mergeFacets needs: how many rows the OTHER side brought
// that are newer than each side's own last evidence.
func (s *Store) mergeObservations(ctx context.Context, tx *sql.Tx, inc, base *mergeSide, rep *MergeReport) (extraBase, extraInc int64, err error) {
	for _, q := range []string{
		`CREATE TEMP TABLE merge_inc_cur(lens TEXT PRIMARY KEY, cur INTEGER NOT NULL)`,
		`CREATE TEMP TABLE merge_base_cur(lens TEXT PRIMARY KEY, cur INTEGER NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, 0, err
		}
	}
	for table, cur := range map[string]map[string]int64{"merge_inc_cur": inc.cursors, "merge_base_cur": base.cursors} {
		for l, c := range cur {
			if _, err := tx.ExecContext(ctx, `INSERT INTO temp.`+table+`(lens, cur) VALUES (?, ?)`, l, c); err != nil {
				return 0, 0, err
			}
		}
	}
	const incFolded = `o.seq <= COALESCE((SELECT cur FROM temp.merge_inc_cur c WHERE c.lens = o.lens), 0)`

	// Decay-clock corrections, counted BEFORE anything is inserted.
	if base.lastTS != "" {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inc.observations o WHERE o.ts > ?
			AND NOT EXISTS (SELECT 1 FROM main.observations m WHERE m.obs_id = o.obs_id)`, base.lastTS).Scan(&extraBase); err != nil {
			return 0, 0, err
		}
	}
	if inc.lastTS != "" {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.observations WHERE ts > ?`, inc.lastTS).Scan(&extraInc); err != nil {
			return 0, 0, err
		}
	}
	var already int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inc.observations o
		WHERE EXISTS (SELECT 1 FROM main.observations m WHERE m.obs_id = o.obs_id)`).Scan(&already); err != nil {
		return 0, 0, err
	}
	rep.Observations.AlreadyHere = already

	// O1
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.observations(`+obsCols+`)
		SELECT `+prefixCols("o")+` FROM inc.observations o WHERE `+incFolded+` ORDER BY o.seq`)
	if err != nil {
		return 0, 0, err
	}
	n1, _ := res.RowsAffected()
	rep.Observations.Reviewed = int(n1)

	if n1 == 0 {
		// Nothing folded arrives, so no cursor has to move and none of our rows needs renumbering —
		// which is also what keeps a second run of the same merge a no-op.
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.observations(`+obsCols+`)
			SELECT `+prefixCols("o")+` FROM inc.observations o WHERE NOT (`+incFolded+`) ORDER BY o.ts, o.seq`)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		rep.Observations.Unreviewed = int(n)
		return extraBase, extraInc, nil
	}

	var g1 int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM main.observations`).Scan(&g1); err != nil {
		return 0, 0, err
	}
	// O2: our unfolded rows (below the incoming block, above our own cursor), minus any the
	// incoming side already folded.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE merge_moved AS
		SELECT `+prefixCols("m")+`, m.seq AS oldseq FROM main.observations m
		WHERE m.seq <= ? AND m.seq > COALESCE((SELECT cur FROM temp.merge_base_cur c WHERE c.lens = m.lens), 0)
		  AND m.obs_id NOT IN (SELECT o.obs_id FROM inc.observations o WHERE `+incFolded+`)`, base.head); err != nil {
		return 0, 0, err
	}
	var moved int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM temp.merge_moved`).Scan(&moved); err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM main.observations WHERE seq IN (SELECT oldseq FROM temp.merge_moved)`); err != nil {
		return 0, 0, err
	}
	res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.observations(`+obsCols+`)
		SELECT `+obsCols+` FROM (
		  SELECT `+obsCols+`, 0 AS src, oldseq AS ord FROM temp.merge_moved
		  UNION ALL
		  SELECT `+prefixCols("o")+`, 1, o.seq FROM inc.observations o
		  WHERE NOT (`+incFolded+`) AND o.obs_id NOT IN (SELECT obs_id FROM temp.merge_moved)
		) ORDER BY ts, src, ord`)
	if err != nil {
		return 0, 0, err
	}
	n2, _ := res.RowsAffected()
	rep.Observations.Unreviewed = int(n2 - moved)

	// O3: every lens's cursor to g1. A lens with no cursor on either side had every row unfolded,
	// so all of them are above g1 now and g1 is still exact for it.
	lenses, err := querySessions(ctx, tx, `SELECT DISTINCT lens FROM main.observations`)
	if err != nil {
		return 0, 0, err
	}
	for _, l := range lenses {
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, reviewRowidKey(l), strconv.FormatInt(g1, 10)); err != nil {
			return 0, 0, err
		}
	}
	return extraBase, extraInc, nil
}

// --- facets -----------------------------------------------------------------------------------

var mergeSuffix = regexp.MustCompile(`__m\d+$`)

// keyFamily strips a merge suffix, so "k", "k__m1" and "k__m2" are recognised as one facet's
// copies — without it, a second merge or a merge back the other way would treat its own earlier
// copy as a stranger and store the same history again.
func keyFamily(key string) string { return mergeSuffix.ReplaceAllString(key, "") }

func versionIdentity(a, b FacetVersion) bool {
	return a.Value == b.Value && a.ValidFrom == b.ValidFrom && a.RecordedAt == b.RecordedAt
}

// Chain relations between two version histories.
const (
	chainUnrelated = iota
	chainEqual
	chainBaseShorter // ours is a prefix of theirs: theirs recorded later versions
	chainIncShorter  // theirs is a prefix of ours
)

func chainRelation(base, inc []FacetVersion) int {
	n := min(len(base), len(inc))
	if n == 0 {
		return chainUnrelated
	}
	for i := 0; i < n; i++ {
		if !versionIdentity(base[i], inc[i]) {
			return chainUnrelated
		}
	}
	switch {
	case len(base) == len(inc):
		return chainEqual
	case len(base) < len(inc):
		return chainBaseShorter
	default:
		return chainIncShorter
	}
}

// evidenceTime is when a version was last EVIDENCED: the latest timestamp among the observations it
// cites, on its own side. It is the only freshness signal that means the same thing in both
// archives. A decay clock does not: it counts one archive's observations, so a clock 10 behind on a
// light laptop and one 400 behind on a busy desktop say nothing about which value is newer — and an
// archive migrated from before decay existed has every clock stamped at its head, which would make
// a stale backup win every comparison. LastSeen does not either: compaction sets it to "now".
func evidenceTime(obsTS map[string]string, v *FacetVersion) string {
	best := ""
	for _, id := range v.BecauseOf {
		if t := obsTS[id]; t > best {
			best = t
		}
	}
	if best == "" {
		return v.RecordedAt
	}
	return best
}

// rebaseClocks moves one side's decay clocks into the merged archive's seq space. Decay is
// "observations since last confirmed", measured as head - clock. Each clock keeps its distance from
// its own side's head, PLUS the observations the other side brought that are newer than this
// side's latest evidence: those were real chances to confirm the facet, and none did. Without that
// term an old laptop's facets would sit 10 observations from the head of a merged archive holding
// thousands of newer ones — confident forever, the problem #173 exists to fix. When the two
// archives ran at the same time the term is ~0 and this reduces to keeping each side's distance.
//
// 0 stays 0 ("no clock"). The clamp at 1 keeps a clock from reaching 0 by arithmetic, which would
// silently mean "never decays".
func rebaseClocks(facets []Facet, sideHead, mergedHead, extra int64) {
	for i := range facets {
		for j := range facets[i].Versions {
			v := &facets[i].Versions[j]
			if v.ConfidenceSeq <= 0 {
				continue
			}
			d := sideHead - v.ConfidenceSeq
			if d < 0 {
				d = 0
			}
			c := mergedHead - (d + extra)
			if c < 1 {
				c = 1
			}
			v.ConfidenceSeq = c
		}
	}
}

func unionIDs(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, id := range append(append([]string{}, a...), b...) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func facetID(lens, dim, key string) string { return lens + "|" + dim + "|" + key }

// mergeFacets merges the incoming L2 into ours. Rules, per incoming facet:
//
//  1. SHARED HISTORY — some facet of ours in the same key family has a chain that is equal to, a
//     prefix of, or extended by the incoming one (a restore, a re-run, a merge back). Nothing new is
//     stored. If theirs is longer it is adopted (fast-forward: they recorded a positive change we
//     have not seen). At each shared version, confidence and its clock are taken TOGETHER from the
//     side whose evidence is newer — taking one side's confidence with the other's clock is how a
//     downgrade gets lost (the #175 failure) — and citations are unioned. Archive state comes from
//     the side that acted on the facet last.
//  2. NEW — no facet of ours with that key: stored as it was, lineage and all.
//  3. SAME KEY, DIVERGED — both archives minted the key independently. Active beats archived; then
//     the copy with newer evidence keeps the key; ties go to ours. The other copy moves to
//     <key>__mN with its whole history, and if both were active it is archived INTO the winner,
//     whose current version gains its citations — exactly what compaction does for a survivor.
//
// Lineage is a text reference ("dimension/key") resolved within a lens, so a rename rewrites the
// references on the renamed copy's side only: its own absorbed facets keep pointing at it.
func mergeFacets(base, inc *mergeSide, mergedHead, extraBase, extraInc int64, now time.Time, rep *MergeFacets) []Facet {
	nowStr := now.UTC().Format(time.RFC3339)
	baseFacets := cloneFacets(base.facets)
	incFacets := cloneFacets(inc.facets)
	rebaseClocks(baseFacets, base.head, mergedHead, extraBase)
	rebaseClocks(incFacets, inc.head, mergedHead, extraInc)
	rep.Incoming = len(incFacets)

	result := map[string]*Facet{}
	fromBase := map[string]bool{}
	family := map[string][]string{} // lens|dim|family -> ids of OUR facets
	for i := range baseFacets {
		f := &baseFacets[i]
		id := facetID(f.Lens, f.Dimension, f.Key)
		result[id], fromBase[id] = f, true
		fam := facetID(f.Lens, f.Dimension, keyFamily(f.Key))
		family[fam] = append(family[fam], id)
	}
	incKeys := map[string]bool{}
	for i := range incFacets {
		incKeys[facetID(incFacets[i].Lens, incFacets[i].Dimension, incFacets[i].Key)] = true
	}
	sort.SliceStable(incFacets, func(i, j int) bool {
		a, b := incFacets[i], incFacets[j]
		return facetID(a.Lens, a.Dimension, a.Key) < facetID(b.Lens, b.Dimension, b.Key)
	})

	baseRename := map[string]string{} // lens|dim|oldKey -> newKey, for OUR side's references
	incRename := map[string]string{}  // the same, for the incoming side's references
	type archiveAct struct{ loser, winner string }
	var archives []archiveAct

	freeKey := func(lens, dim, key string) string {
		root := keyFamily(key)
		for n := 1; ; n++ {
			k := root + "__m" + strconv.Itoa(n)
			id := facetID(lens, dim, k)
			if result[id] == nil && !incKeys[id] {
				return k
			}
		}
	}

nextIncoming:
	for i := range incFacets {
		f := &incFacets[i]
		id := facetID(f.Lens, f.Dimension, f.Key)

		// 1. Shared history anywhere in the key family.
		fam := facetID(f.Lens, f.Dimension, keyFamily(f.Key))
		cands := append([]string{}, family[fam]...)
		sort.Strings(cands)
		for _, cid := range cands {
			b := result[cid]
			if b == nil || !fromBase[cid] {
				continue
			}
			if rel := chainRelation(b.Versions, f.Versions); rel != chainUnrelated {
				adoptShared(b, f, rel, base.obsTS, inc.obsTS)
				if rel == chainBaseShorter {
					rep.FastForwarded++
				} else {
					rep.Shared++
				}
				continue nextIncoming
			}
		}

		// 2. New.
		b, clash := result[id]
		if !clash {
			result[id], fromBase[id] = f, false
			rep.New++
			continue
		}

		// 3. Same key, diverged.
		incWins := false
		if b.Active() != f.Active() {
			incWins = f.Active()
		} else {
			incWins = currentEvidence(inc.obsTS, f) > currentEvidence(base.obsTS, b)
		}
		newKey := freeKey(f.Lens, f.Dimension, f.Key)
		newID := facetID(f.Lens, f.Dimension, newKey)
		bothActive := b.Active() && f.Active()
		if incWins {
			b.Key = newKey
			result[newID], fromBase[newID] = b, true
			result[id], fromBase[id] = f, false
			baseRename[id] = newKey
			family[fam] = append(family[fam], newID)
		} else {
			f.Key = newKey
			result[newID], fromBase[newID] = f, false
			incRename[id] = newKey
		}
		if bothActive {
			archives = append(archives, archiveAct{loser: newID, winner: id})
		}
		rep.SameKey = append(rep.SameKey, SameKeyFacet{Lens: f.Lens, Dimension: f.Dimension,
			Key: keyAt(id), LoserKey: newKey, IncomingWon: incWins})
	}

	// Lineage rewrite, each side's references by its own rename map.
	for _, fc := range result {
		dim, key, ok := ParseFacetRef(fc.MergedInto)
		if !ok {
			continue
		}
		renames := incRename
		if fromBase[facetID(fc.Lens, fc.Dimension, fc.Key)] {
			renames = baseRename
		}
		if nk, ok := renames[facetID(fc.Lens, dim, key)]; ok {
			fc.MergedInto = FacetRef(dim, nk)
		}
	}
	// Same-key losers that were both active, archived into their winner — after the rewrite, so the
	// new link is not itself rewritten.
	for _, a := range archives {
		loser, winner := result[a.loser], result[a.winner]
		loser.ArchivedAt = nowStr
		loser.MergedInto = FacetRef(winner.Dimension, winner.Key)
		loser.ArchiveReason = "the same key existed in a merged archive (witness merge, " + nowStr[:10] + ")"
		if wc, lc := winner.Current(), loser.Current(); wc != nil && lc != nil {
			wc.BecauseOf = unionIDs(wc.BecauseOf, lc.BecauseOf)
		}
	}

	ids := make([]string, 0, len(result))
	for id := range result {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Facet, 0, len(ids))
	for _, id := range ids {
		out = append(out, *result[id])
	}
	return out
}

func keyAt(id string) string { return id[strings.LastIndex(id, "|")+1:] }

func currentEvidence(obsTS map[string]string, f *Facet) string {
	if cur := f.Current(); cur != nil {
		return evidenceTime(obsTS, cur)
	}
	if n := len(f.Versions); n > 0 {
		return evidenceTime(obsTS, &f.Versions[n-1])
	}
	return ""
}

// adoptShared folds an incoming facet whose history ours shares into ours (rule 1).
func adoptShared(b *Facet, f *Facet, rel int, baseTS, incTS map[string]string) {
	n := min(len(b.Versions), len(f.Versions))
	for i := 0; i < n; i++ {
		bv, iv := &b.Versions[i], &f.Versions[i]
		if evidenceTime(incTS, iv) > evidenceTime(baseTS, bv) {
			bv.Confidence, bv.ConfidenceSeq = iv.Confidence, iv.ConfidenceSeq
		}
		bv.BecauseOf = unionIDs(bv.BecauseOf, iv.BecauseOf)
		if bv.ValidTo == "" && iv.ValidTo != "" {
			bv.ValidTo = iv.ValidTo
		}
		if strings.TrimSpace(bv.Claim) == "" {
			bv.Claim = iv.Claim
		}
	}
	if rel == chainBaseShorter {
		b.Versions = append(b.Versions, f.Versions[n:]...)
	}
	// Archive state from the side that acted on the facet LAST: a facet one side compacted away
	// stays archived unless the other side reasserted it afterwards. Without this a merge back
	// would quietly undo one side's compaction.
	if b.ArchivedAt != f.ArchivedAt || b.MergedInto != f.MergedInto {
		if max(f.ArchivedAt, f.LastSeen) > max(b.ArchivedAt, b.LastSeen) {
			b.ArchivedAt, b.MergedInto, b.ArchiveReason = f.ArchivedAt, f.MergedInto, f.ArchiveReason
		}
	}
	if f.LastSeen > b.LastSeen {
		b.LastSeen = f.LastSeen
	}
}

func cloneFacets(in []Facet) []Facet {
	out := make([]Facet, len(in))
	for i, f := range in {
		f.Versions = append([]FacetVersion(nil), f.Versions...)
		for j := range f.Versions {
			f.Versions[j].BecauseOf = append([]string(nil), f.Versions[j].BecauseOf...)
		}
		out[i] = f
	}
	return out
}

// --- files ------------------------------------------------------------------------------------

func copyFile0600(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// peekUserVersion reads a database's schema version WITHOUT migrating it, so a database from a
// NEWER witness is refused rather than read through column lists that silently drop what it added.
func peekUserVersion(path string) (int, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	var hasRaw int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('raw','l0')`).Scan(&hasRaw); err != nil {
		return 0, err
	}
	if hasRaw == 0 {
		return 0, errors.New("no witness tables")
	}
	return v, nil
}
