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
	"reflect"
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
// Re-merging the same source, and merging back, are handled (every rule below is idempotent on
// natural keys and recognises its own earlier output), but a real two-way sync needs tombstones
// and a three-way facet merge, which this deliberately is not.
//
// THE SHAPE. The incoming database is never opened in place: Open writes into a data dir (config,
// a lens migration, the default-lens seed), and a backup must stay a backup. It is copied to a
// temp file, migrated THERE, and everything that has to change on the incoming side — renaming a
// diverged session, dropping observations this archive already mined — is done to that copy. The
// copy is then ATTACHed and the whole merge runs in ONE transaction on a pinned connection, so it
// lands completely or not at all, and a dry run is the same transaction rolled back. Every step is
// linear in the archive sizes: the write lock is held for well under a second at tens of thousands
// of observations, inside the 5s busy_timeout a concurrent capture hook waits.
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
	inc, err := readIncoming(ctx, copyPath, rep)
	if err != nil {
		return nil, err
	}
	if err := s.mergeTx(ctx, copyPath, inc, rep, apply, now); err != nil {
		return nil, err
	}
	return rep, nil
}

// ErrMergeLiveArchive is returned for an incoming database that has a -wal file beside it: a
// running archive's data directory rather than a snapshot.
var ErrMergeLiveArchive = errors.New("that is a live data directory (it has a witness.db-wal), not a snapshot; " +
	"export it on the machine that owns it — `witness export --all <newdir>` — and merge that")

// mergeArchiveReasonPrefix marks a facet the MERGE archived (a same-key loser). It is bookkeeping,
// not a judgment about the facet, so a later merge never adopts it from the other side: adopting
// it is how a merge back archived the winning facet into itself and left the trait with no active
// copy at all.
const mergeArchiveReasonPrefix = "the same key existed in a merged archive"

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
	ProgressRows int // mining-progress rows added or upgraded
	MetaKeys     int // importer keys and emergent marks written
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
	Duplicates  int // incoming sessions that were a second copy of one transcript (a merge back)
	// Diverged sessions share an id but not a transcript — two machines ingesting the same named
	// `file:` feed, typically. Ours keeps the id; the incoming one gets an id of its own so its
	// transcript AND what was mined from it arrive together, instead of its observations landing
	// under our transcript.
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
	// Orphaned: observations the other side mined from a transcript it has since cleaned up, of a
	// session whose transcript WE hold and never mined for that lens. They would arrive with no
	// progress, so the lens would mine the session here again beside them; ours is mined instead.
	Orphaned int
}

// MergeFacets counts L2.
type MergeFacets struct {
	Incoming      int
	New           int // no facet here with that key or shared history
	Shared        int // same history already here (a restore, or a re-run): nothing new
	Updated       int // same history, but the other side's later judgement (a downgrade, a retirement, a claim) was adopted
	FastForwarded int // same history, and the incoming side recorded later versions: adopted
	SameKey       []SameKeyFacet
}

// SameKeyFacet is a key both archives minted with DIFFERENT histories. One keeps the key; the
// other moves to LoserKey with its whole history, archived into the winner if both were active.
type SameKeyFacet struct {
	Lens, Dimension, Key, LoserKey string
	IncomingWon                    bool
}

// Empty reports whether the merge changes nothing at all — the second run of the same merge.
// Every kind of write counts: a facet-only delta (the other machine's compaction, a downgrade, a
// claim pass) is a real change, and reporting it as "nothing to merge" let the user discard it.
func (r *MergeReport) Empty() bool {
	return r.Sessions.New == 0 && r.Sessions.LongerThere == 0 && len(r.Sessions.Diverged) == 0 &&
		r.RawRows == 0 && r.ProgressRows == 0 && r.MetaKeys == 0 &&
		r.Observations.Reviewed == 0 && r.Observations.Unreviewed == 0 && r.Staged == 0 &&
		r.Facets.New == 0 && r.Facets.Updated == 0 && r.Facets.FastForwarded == 0 && len(r.Facets.SameKey) == 0
}

// mergeSide is one archive's state. The incoming side is read from the private temp copy; this
// archive's side is read INSIDE the merge transaction, so a concurrent write (a delete_observation
// resetting a review cursor) either lands before the read or waits for the commit, instead of being
// silently overwritten by it.
type mergeSide struct {
	head    int64            // MAX(observations.seq): the decay clock's "now" on this side
	cursors map[string]int64 // review_rowid:<lens>
	facets  []Facet
	obsTS   map[string]string // obs_id -> ts, for evidence time
}

func readSide(ctx context.Context, q rowQuerier) (*mergeSide, error) {
	side := &mergeSide{cursors: map[string]int64{}, obsTS: map[string]string{}}
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM observations`).Scan(&side.head); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM meta WHERE key LIKE 'review\_rowid:%' ESCAPE '\'`)
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
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	ts, err := q.QueryContext(ctx, `SELECT obs_id, ts FROM observations`)
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
	if err := closeRows(ts); err != nil {
		return nil, err
	}
	if side.facets, err = readFacetsFrom(ctx, q, false); err != nil {
		return nil, err
	}
	return side, nil
}

// closeRows ends an iteration and reports any error it stopped on, so a scan cut short cannot pass
// for a complete read — a partial cursor map would move every row of ours above the new cursor.
func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// readIncoming migrates the temp copy and reads the incoming side, plus the report header.
func readIncoming(ctx context.Context, copyPath string, rep *MergeReport) (*mergeSide, error) {
	db, err := openDB(copyPath) // migrates the COPY only
	if err != nil {
		return nil, fmt.Errorf("open incoming copy: %w", err)
	}
	defer db.Close()
	side, err := readSide(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read incoming archive: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MIN(NULLIF(ts,'')),''), COALESCE(MAX(NULLIF(ts,'')),'') FROM raw`).
		Scan(&rep.FirstTS, &rep.LastTS); err != nil {
		return nil, err
	}
	if rep.Cwds, err = queryStrings(ctx, db, `SELECT cwd FROM session_meta WHERE cwd <> '' GROUP BY cwd ORDER BY COUNT(*) DESC, cwd LIMIT 3`); err != nil {
		return nil, err
	}
	// Lens names arrive as DATA here (observations, facets, progress) — not through RegisterLens —
	// so they get the same name rule here, before anything is written.
	lenses, err := queryStrings(ctx, db, `SELECT lens FROM observations UNION SELECT lens FROM facets UNION SELECT lens FROM progress`)
	if err != nil {
		return nil, err
	}
	for _, l := range lenses {
		if l == "" {
			continue
		}
		if err := ValidLensName(l); err != nil {
			return nil, fmt.Errorf("incoming archive holds data for an invalid lens: %w", err)
		}
		rep.Lenses = append(rep.Lenses, l)
	}
	sort.Strings(rep.Lenses)
	return side, nil
}

func queryStrings(ctx context.Context, q rowQuerier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	return out, closeRows(rows)
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
func (s *Store) mergeTx(ctx context.Context, copyPath string, inc *mergeSide, rep *MergeReport, apply bool, now time.Time) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// ATTACH is not allowed inside a transaction, so it precedes the BEGIN and the DETACH follows
	// the end of it (defers run last-registered-first: rollback, then detach, then close). TEMP
	// tables are created INSIDE the transaction, so a rollback — the dry run, or any error — takes
	// them with it, and the pooled connection goes back clean.
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

	base, err := readSide(ctx, tx)
	if err != nil {
		return fmt.Errorf("read this archive: %w", err)
	}
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
	// Decay-clock corrections, read before any observation moves: for each side, the timestamps of
	// the rows the OTHER side contributes (see rebaseClocks).
	arrivingForBase, err := queryStrings(ctx, tx, `SELECT o.ts FROM inc.observations o
		WHERE o.ts <> '' AND NOT EXISTS (SELECT 1 FROM main.observations m WHERE m.obs_id = o.obs_id) ORDER BY o.ts`)
	if err != nil {
		return err
	}
	arrivingForInc, err := queryStrings(ctx, tx, `SELECT m.ts FROM main.observations m
		WHERE m.ts <> '' AND NOT EXISTS (SELECT 1 FROM inc.observations o WHERE o.obs_id = m.obs_id) ORDER BY m.ts`)
	if err != nil {
		return err
	}
	if err := mergeObservations(ctx, tx, inc, base, rep); err != nil {
		return fmt.Errorf("merge observations: %w", err)
	}
	var m int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM main.observations`).Scan(&m); err != nil {
		return err
	}
	merged := mergeFacets(base, inc, m, arrivingForBase, arrivingForInc, now, &rep.Facets)
	if err := writeFacetsTx(tx, merged); err != nil {
		return fmt.Errorf("write facets: %w", err)
	}
	if !apply {
		return nil // the deferred rollback discards everything, temp tables included; the report stands
	}
	for _, t := range []string{"merge_kind", "merge_mined", "merge_moved", "merge_inc_cur", "merge_base_cur"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+t); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// --- sessions ---------------------------------------------------------------------------------

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
	var out []rawTurn
	for rows.Next() {
		var t rawTurn
		if err := rows.Scan(&t.role, &t.text); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	return out, closeRows(rows)
}

// turnRelation is how an incoming transcript relates to one of ours: equal, a prefix of ours
// (longer here), extended by theirs (longer there), or "" — diverged.
func turnRelation(ours, theirs []rawTurn) string {
	if len(ours) == 0 {
		return ""
	}
	n := min(len(ours), len(theirs))
	for i := 0; i < n; i++ {
		if ours[i] != theirs[i] {
			return ""
		}
	}
	switch {
	case len(theirs) == len(ours):
		return kindEqual
	case len(theirs) > len(ours):
		return kindLongerThere
	default:
		return kindLongerHere
	}
}

var renamedSuffix = regexp.MustCompile(`@[0-9a-f]{8}$`)

// sessionFamily strips the suffix a merge gives a diverged session, so "feed" and "feed@3c755455"
// are recognised as one session's copies.
func sessionFamily(s string) string { return renamedSuffix.ReplaceAllString(s, "") }

func transcriptHash8(turns []rawTurn) string {
	h := sha1.New()
	for _, t := range turns {
		io.WriteString(h, t.role)
		h.Write([]byte{0})
		io.WriteString(h, t.text)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// classifySessions matches every incoming transcript to ours. Identity is the ordered
// (role, text) list: `seq` is a plain ordinal with no uniqueness, and `id` is a per-archive
// surrogate.
//
// Matching is by FAMILY, not exact id: an incoming session is compared with our session of the
// same id AND with every copy an earlier merge made of it ("<id>@<hash>"). Exact-id matching broke
// two ordinary cases:
//
//   - a diverged feed that kept growing on the other machine arrived in FULL under a new id on
//     every re-merge, because its new length gave it a new hash — so its shared turns were held,
//     and later mined, two and three times;
//   - merging back carried our own transcript home under its renamed id, so we held it twice, and
//     a third round folded two transcripts into one session.
//
// So: if any member of the family is equal to, a prefix of, or extended by the incoming transcript,
// that member is the match and the incoming session is renamed to it in the copy. Only when nothing
// matches and our exact id is taken by a different transcript is a fresh id minted. An incoming
// session that turns out to be a second copy of one already claimed (a merge back can carry both)
// is dropped from the copy.
func classifySessions(ctx context.Context, tx *sql.Tx, rep *MergeReport) (map[string]string, error) {
	incoming, err := queryStrings(ctx, tx, `SELECT DISTINCT session FROM inc.raw ORDER BY session`)
	if err != nil {
		return nil, err
	}
	ours, err := queryStrings(ctx, tx, `SELECT DISTINCT session FROM main.raw`)
	if err != nil {
		return nil, err
	}
	rep.Sessions.Incoming = len(incoming)
	family := map[string][]string{}
	for _, o := range ours {
		family[sessionFamily(o)] = append(family[sessionFamily(o)], o)
	}
	held := map[string]bool{}
	for _, s := range incoming {
		held[s] = true
	}

	kinds := map[string]string{}
	for _, s := range incoming {
		theirs, err := readTurns(ctx, tx, "inc", s)
		if err != nil {
			return nil, err
		}
		// Our exact id first, then the family root, then its other copies, in a stable order.
		cands := []string{s, sessionFamily(s)}
		rest := append([]string{}, family[sessionFamily(s)]...)
		sort.Strings(rest)
		cands = append(cands, rest...)

		target, kind, oursHasS := "", "", false
		seen := map[string]bool{}
		for _, c := range cands {
			if seen[c] {
				continue
			}
			seen[c] = true
			mine, err := readTurns(ctx, tx, "main", c)
			if err != nil {
				return nil, err
			}
			if c == s && len(mine) > 0 {
				oursHasS = true
			}
			if k := turnRelation(mine, theirs); k != "" {
				target, kind = c, k
				break
			}
		}
		minted := false
		if target == "" {
			target, kind = s, kindNew
			if oursHasS {
				target, minted = sessionFamily(s)+"@"+transcriptHash8(theirs), true
			}
		}
		if target != s && held[target] {
			// The copy already holds that id. If it is the same transcript, this is a duplicate.
			other, err := readTurns(ctx, tx, "inc", target)
			if err != nil {
				return nil, err
			}
			if reflect.DeepEqual(other, theirs) {
				if err := dropIncomingSession(ctx, tx, s); err != nil {
					return nil, err
				}
				delete(held, s)
				rep.Sessions.Duplicates++
				continue
			}
			target, kind, minted = sessionFamily(s)+"@"+transcriptHash8(theirs), kindNew, true
			if held[target] { // same hash: the same transcript under a third id
				if err := dropIncomingSession(ctx, tx, s); err != nil {
					return nil, err
				}
				delete(held, s)
				rep.Sessions.Duplicates++
				continue
			}
		}
		if target != s {
			if err := renameIncomingSession(ctx, tx, s, target); err != nil {
				return nil, err
			}
			delete(held, s)
			held[target] = true
		}
		kinds[target] = kind
		if minted {
			rep.Sessions.Diverged = append(rep.Sessions.Diverged, RenamedSession{From: s, To: target})
		}
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

var mergeSessionTables = []string{"raw", "session_meta", "progress", "observations", "staged"}

// renameIncomingSession moves an incoming session to another id, in the incoming COPY, so its
// transcript and everything mined from it arrive together. Staged rows carry the session inside
// their JSON payload too, and DrainStaged trusts the payload — renaming only the column filed the
// other machine's in-session observation under OUR transcript. Importer keys are dropped: they
// describe the session under its native id, and no importer will ever see the renamed one.
func renameIncomingSession(ctx context.Context, tx *sql.Tx, from, to string) error {
	for _, table := range mergeSessionTables {
		q := `UPDATE inc.` + table + ` SET session = ? WHERE session = ?`
		if table == "staged" {
			q = `UPDATE inc.staged SET session = ?1, payload = json_set(payload, '$.session', ?1) WHERE session = ?2`
		}
		if _, err := tx.ExecContext(ctx, q, to, from); err != nil {
			return fmt.Errorf("rename in %s: %w", table, err)
		}
	}
	return dropImporterKeys(ctx, tx, from)
}

func dropIncomingSession(ctx context.Context, tx *sql.Tx, sess string) error {
	for _, table := range mergeSessionTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM inc.`+table+` WHERE session = ?`, sess); err != nil {
			return fmt.Errorf("drop duplicate from %s: %w", table, err)
		}
	}
	return dropImporterKeys(ctx, tx, sess)
}

func dropImporterKeys(ctx context.Context, tx *sql.Tx, sess string) error {
	for _, ns := range PerSessionMetaNamespaces {
		if _, err := tx.ExecContext(ctx, `DELETE FROM inc.meta WHERE key = ?`, ns+":"+sess); err != nil {
			return err
		}
	}
	return nil
}

// dropSharedMined keeps the merge from importing a session twice. Three rules, each removing rows
// from the incoming COPY:
//
//  1. PARAPHRASES. For every (session, lens) this archive already mined, the incoming MINED rows
//     whose obs_id we do not hold. Mining is not deterministic, so they are a second wording of
//     moments we already have; a union would count each twice, permanently, since L1 is append-only
//     and carries no source column. An IDENTICAL row (a restore) stays: the insert dedupes it, and
//     it is the record that the incoming side already folded that observation.
//  2. THEIR PROGRESS, wherever we mined the session: our watermark wins, and if their transcript is
//     longer its tail is appended (mergeL0) and mined here. Where our cleanup reclaimed the
//     transcript but kept its observations, mergeL0 marks the returning transcript mined at its
//     full length instead — NOT at their count, which may be partial: keeping a partial count of
//     theirs left our tail pending, re-mined beside the observations we already hold.
//  3. THEIR ORPHANS. Mined rows from a transcript the other side has since cleaned up (no raw, no
//     progress there), of a session whose transcript we hold and never mined for that lens. They
//     would arrive with no progress beside our transcript, so the lens would mine it here and
//     duplicate them; ours is mined instead.
//
// "Already mined here" is a progress mark OR mined rows (cleanup removes progress, keeps L1). It is
// materialised ONCE into an indexed set: as a correlated subquery over observations, which has no
// (session, lens) index, it scanned the whole lens per incoming row — quadratic, ~15 minutes at
// 75k observations a side, with the archive's write lock held and every capture hook in that window
// losing its turn to SQLITE_BUSY.
func dropSharedMined(ctx context.Context, tx *sql.Tx, rep *MergeReport) error {
	for _, q := range []string{
		`CREATE TEMP TABLE merge_mined(session TEXT NOT NULL, lens TEXT NOT NULL, PRIMARY KEY(session, lens)) WITHOUT ROWID`,
		`INSERT OR IGNORE INTO temp.merge_mined(session, lens)
		   SELECT session, lens FROM main.progress WHERE distilled > 0
		   UNION SELECT session, lens FROM main.observations WHERE source <> 'active'`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	const minedHere = `EXISTS (SELECT 1 FROM temp.merge_mined k WHERE k.session = %[1]s.session AND k.lens = %[1]s.lens)`
	const notHeld = `NOT EXISTS (SELECT 1 FROM main.observations same WHERE same.obs_id = inc.observations.obs_id)`

	res, err := tx.ExecContext(ctx, `DELETE FROM inc.observations WHERE source <> 'active' AND `+
		fmt.Sprintf(minedHere, "inc.observations")+` AND `+notHeld)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	rep.Observations.SharedMined = int(n)

	if _, err := tx.ExecContext(ctx, `DELETE FROM inc.progress WHERE `+fmt.Sprintf(minedHere, "inc.progress")); err != nil {
		return err
	}

	res, err = tx.ExecContext(ctx, `DELETE FROM inc.observations WHERE source <> 'active'
		AND NOT EXISTS (SELECT 1 FROM inc.progress p WHERE p.session = inc.observations.session AND p.lens = inc.observations.lens)
		AND NOT EXISTS (SELECT 1 FROM inc.raw r WHERE r.session = inc.observations.session)
		AND EXISTS (SELECT 1 FROM main.raw r WHERE r.session = inc.observations.session)
		AND NOT EXISTS (SELECT 1 FROM main.progress p WHERE p.session = inc.observations.session AND p.lens = inc.observations.lens)
		AND `+notHeld)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	rep.Observations.Orphaned = int(n)
	return nil
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

	// progress: ours wins where we have mined; theirs fills in what we never mined, or upgrades a
	// row of ours still at 0 (pending). Dropping theirs instead would make every incoming session
	// pending for every lens — re-mined at model cost and, mining being non-deterministic, minting
	// duplicate observations.
	res, err = tx.ExecContext(ctx, `INSERT INTO main.progress(session, lens, distilled, retries, distilled_at, next_attempt, drift_at)
		SELECT session, lens, distilled, retries, distilled_at, next_attempt, drift_at FROM inc.progress WHERE true
		ON CONFLICT(session, lens) DO UPDATE SET distilled = excluded.distilled, retries = excluded.retries,
		  distilled_at = excluded.distilled_at, next_attempt = excluded.next_attempt, drift_at = excluded.drift_at
		WHERE progress.distilled = 0 AND excluded.distilled > 0`)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	rep.ProgressRows = int(n)
	// A transcript that just arrived for a session we mined before our cleanup reclaimed it: our
	// observations of it are still here, so mark it mined at its length. Left unmarked it would be
	// mined a second time beside them.
	res, err = tx.ExecContext(ctx, `INSERT INTO main.progress(session, lens, distilled)
		SELECT k.session, k.lens, (SELECT COUNT(*) FROM main.raw r WHERE r.session = k.session)
		FROM temp.merge_mined k JOIN temp.merge_kind mk ON mk.session = k.session AND mk.kind = ?
		WHERE NOT EXISTS (SELECT 1 FROM main.progress p WHERE p.session = k.session AND p.lens = k.lens)`, kindNew)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	rep.ProgressRows += int(n)

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
		if err := closeRows(rows); err != nil {
			return err
		}
		for _, e := range keys {
			var res sql.Result
			switch kinds[strings.TrimPrefix(e.k, ns+":")] {
			case kindLongerHere:
				// Ours has rows theirs never saw; their keys would cover fewer rows than we hold.
				continue
			case kindEqual:
				res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.meta(key, value) VALUES (?, ?)`, e.k, e.v)
			default: // new, longer_there, or a session with keys but no transcript
				res, err = tx.ExecContext(ctx, `INSERT INTO main.meta(key, value) VALUES (?, ?)
					ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE meta.value <> excluded.value`, e.k, e.v)
			}
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			rep.MetaKeys += int(n)
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
	res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.meta(key, value)
		SELECT key, value FROM inc.meta WHERE key LIKE 'emerge\_seen:%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	rep.MetaKeys += int(n)
	return nil
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
// Every insert filters out rows already here rather than relying on INSERT OR IGNORE: an ignored
// row still CONSUMES an AUTOINCREMENT value, so re-merging an archive whose 3,000 rows were all
// present pushed the sequence 3,000 past MAX(seq) — and the next observation anyone mined advanced
// the decay clock by 3,000 at once, halving every facet in the profile.
//
// The work is skipped only when there is nothing to place: no new folded row, no new unfolded row,
// and none of OUR unfolded rows that the incoming side already folded. Skipping on "no new folded
// row" alone left those last rows above our cursor, folded a second time, and queued new unfolded
// rows "ours, then theirs".
func mergeObservations(ctx context.Context, tx *sql.Tx, inc, base *mergeSide, rep *MergeReport) error {
	for _, q := range []string{
		`CREATE TEMP TABLE merge_inc_cur(lens TEXT PRIMARY KEY, cur INTEGER NOT NULL)`,
		`CREATE TEMP TABLE merge_base_cur(lens TEXT PRIMARY KEY, cur INTEGER NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for table, cur := range map[string]map[string]int64{"merge_inc_cur": inc.cursors, "merge_base_cur": base.cursors} {
		for l, c := range cur {
			if _, err := tx.ExecContext(ctx, `INSERT INTO temp.`+table+`(lens, cur) VALUES (?, ?)`, l, c); err != nil {
				return err
			}
		}
	}
	const incFolded = `o.seq <= COALESCE((SELECT cur FROM temp.merge_inc_cur c WHERE c.lens = o.lens), 0)`
	const notHere = `NOT EXISTS (SELECT 1 FROM main.observations here WHERE here.obs_id = o.obs_id)`
	const ourUnfolded = `m.seq > COALESCE((SELECT cur FROM temp.merge_base_cur c WHERE c.lens = m.lens), 0)`

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inc.observations o WHERE NOT `+notHere).
		Scan(&rep.Observations.AlreadyHere); err != nil {
		return err
	}

	// O1
	res, err := tx.ExecContext(ctx, `INSERT INTO main.observations(`+obsCols+`)
		SELECT `+prefixCols("o")+` FROM inc.observations o WHERE `+incFolded+` AND `+notHere+` ORDER BY o.seq`)
	if err != nil {
		return err
	}
	n1, _ := res.RowsAffected()
	rep.Observations.Reviewed = int(n1)

	var newUnfolded, overlap int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inc.observations o WHERE NOT (`+incFolded+`) AND `+notHere).
		Scan(&newUnfolded); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.observations m WHERE `+ourUnfolded+`
		AND m.obs_id IN (SELECT o.obs_id FROM inc.observations o WHERE `+incFolded+`)`).Scan(&overlap); err != nil {
		return err
	}
	if n1 == 0 && newUnfolded == 0 && overlap == 0 {
		return nil // nothing arrives and nothing moves — which is also what keeps a re-run a no-op
	}

	var g1 int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM main.observations`).Scan(&g1); err != nil {
		return err
	}
	// O2: our unfolded rows (below the incoming block, above our own cursor), minus any the
	// incoming side already folded.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE merge_moved AS
		SELECT `+prefixCols("m")+`, m.seq AS oldseq FROM main.observations m
		WHERE m.seq <= ? AND `+ourUnfolded+`
		  AND m.obs_id NOT IN (SELECT o.obs_id FROM inc.observations o WHERE `+incFolded+`)`, base.head); err != nil {
		return err
	}
	var moved int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM temp.merge_moved`).Scan(&moved); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM main.observations WHERE seq IN (SELECT oldseq FROM temp.merge_moved)`); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO main.observations(`+obsCols+`)
		SELECT `+obsCols+` FROM (
		  SELECT `+obsCols+`, 0 AS src, oldseq AS ord FROM temp.merge_moved
		  UNION ALL
		  SELECT `+prefixCols("o")+`, 1, o.seq FROM inc.observations o
		  WHERE NOT (`+incFolded+`) AND `+notHere+` AND o.obs_id NOT IN (SELECT obs_id FROM temp.merge_moved)
		) ORDER BY ts, src, ord`)
	if err != nil {
		return err
	}
	n2, _ := res.RowsAffected()
	rep.Observations.Unreviewed = int(n2 - moved)

	// O3: every lens's cursor to g1. A lens with no cursor on either side had every row unfolded,
	// so all of them are above g1 now and g1 is still exact for it.
	lenses, err := queryStrings(ctx, tx, `SELECT DISTINCT lens FROM main.observations`)
	if err != nil {
		return err
	}
	for _, l := range lenses {
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, reviewRowidKey(l), strconv.FormatInt(g1, 10)); err != nil {
			return err
		}
	}
	return nil
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

func currentEvidence(obsTS map[string]string, f *Facet) string {
	if cur := f.Current(); cur != nil {
		return evidenceTime(obsTS, cur)
	}
	if n := len(f.Versions); n > 0 {
		return evidenceTime(obsTS, &f.Versions[n-1])
	}
	return ""
}

// rebaseClocks moves one side's decay clocks into the merged archive's seq space. Decay is
// "observations since last confirmed", measured as head - clock. Each version keeps its distance
// from its own side's head, PLUS the observations the other side contributes that are newer than
// THIS VERSION's own evidence: those were real chances to confirm it, and none did.
//
// Per version, not per side. Measured against a side's newest row, one late observation on an
// otherwise idle laptop cancelled thousands of real chances — a June facet sat 1 observation from
// the head of a merged archive holding 300 newer ones, confident forever, the problem #173 exists
// to fix. otherTS is the other side's contributed timestamps, sorted.
//
// 0 stays 0 ("no clock"). The clamp at 1 keeps a clock from reaching 0 by arithmetic, which would
// silently mean "never decays".
func rebaseClocks(facets []Facet, sideHead, mergedHead int64, ownTS map[string]string, otherTS []string) {
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
			e := evidenceTime(ownTS, v)
			newer := len(otherTS) - sort.Search(len(otherTS), func(k int) bool { return otherTS[k] > e })
			c := mergedHead - (d + int64(newer))
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

func keyAt(id string) string { return id[strings.LastIndex(id, "|")+1:] }

// mergeFacets merges the incoming L2 into ours. Rules, per incoming facet:
//
//  1. SHARED HISTORY — some facet of ours in the same key family has a chain that is equal to, a
//     prefix of, or extended by the incoming one (a restore, a re-run, a merge back). Nothing new is
//     stored. A longer chain is adopted only if its extension is evidenced AFTER the other side's
//     last shared version: chain length alone let a February change overrule our December
//     reaffirmation of the old value, leaving a closed version citing evidence from after it
//     closed. When that check fails the pair is treated as diverged (rule 3) instead. At each shared
//     version, confidence and its clock are taken TOGETHER from the side whose evidence is newer —
//     one side's confidence with the other's clock is how a downgrade got lost (#175) — and
//     citations are unioned. Archive state: see adoptShared.
//  2. NEW — no facet of ours with that key: stored as it was, lineage and all.
//  3. SAME KEY, DIVERGED — both archives minted the key independently. Active beats archived; then
//     the copy with newer evidence keeps the key; ties go to ours. The other copy moves to
//     <key>__mN with its whole history, and if both were active it is archived INTO the winner,
//     whose current version gains its citations — exactly what compaction does for a survivor.
//
// Lineage is a text reference ("dimension/key") resolved within a lens, so a rename rewrites a
// reference with the rename map of the side it CAME from: a loser's own absorbed facets keep
// pointing at it, and a reference adopted from the other side follows the other side's renames.
func mergeFacets(base, inc *mergeSide, mergedHead int64, arrivingForBase, arrivingForInc []string, now time.Time, rep *MergeFacets) []Facet {
	nowStr := now.UTC().Format(time.RFC3339)
	baseFacets := cloneFacets(base.facets)
	incFacets := cloneFacets(inc.facets)
	// Evidence time is read over BOTH archives' observations: an observation has the same timestamp
	// in every archive that holds it, and a version citing one this side lacks would otherwise fall
	// back to its RecordedAt — making two identical copies of a facet look as if one were newer.
	// Verified on a real archive split in two: 1,553 identical facets were reported as updated.
	allTS := make(map[string]string, len(base.obsTS)+len(inc.obsTS))
	for _, m := range []map[string]string{inc.obsTS, base.obsTS} {
		for id, t := range m {
			allTS[id] = t
		}
	}
	rebaseClocks(baseFacets, base.head, mergedHead, allTS, arrivingForBase)
	rebaseClocks(incFacets, inc.head, mergedHead, allTS, arrivingForInc)
	rep.Incoming = len(incFacets)

	result := map[string]*Facet{}
	fromBase := map[string]bool{}
	incomingRef := map[*Facet]bool{} // our facets whose MergedInto was adopted from the other side
	family := map[string][]string{}  // lens|dim|family -> ids of OUR facets
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
			rel := chainRelation(b.Versions, f.Versions)
			switch rel {
			case chainUnrelated:
				continue
			case chainBaseShorter: // theirs extends ours: only if the extension is newer evidence
				if currentEvidence(allTS, f) <= evidenceTime(allTS, &b.Versions[len(b.Versions)-1]) {
					continue
				}
			case chainIncShorter: // ours extends theirs: only if our extension is not older than their last word
				if currentEvidence(allTS, b) < evidenceTime(allTS, &f.Versions[len(f.Versions)-1]) {
					continue
				}
			}
			changed := adoptShared(b, f, rel, allTS, allTS)
			if changed && f.MergedInto != "" && b.MergedInto == f.MergedInto {
				incomingRef[b] = true
			}
			switch {
			case rel == chainBaseShorter:
				rep.FastForwarded++
			case changed:
				rep.Updated++
			default:
				rep.Shared++
			}
			continue nextIncoming
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
			incWins = currentEvidence(allTS, f) > currentEvidence(allTS, b)
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

	// Lineage rewrite, each reference by the rename map of the side it came from.
	for _, fc := range result {
		dim, key, ok := ParseFacetRef(fc.MergedInto)
		if !ok {
			continue
		}
		renames := incRename
		if fromBase[facetID(fc.Lens, fc.Dimension, fc.Key)] && !incomingRef[fc] {
			renames = baseRename
		}
		if nk, ok := renames[facetID(fc.Lens, dim, key)]; ok {
			fc.MergedInto = FacetRef(dim, nk)
		}
	}
	// Same-key losers that were both active, archived into their winner — after the rewrite, so the
	// new link is not itself rewritten, and only into a winner that is still active.
	for _, a := range archives {
		loser, winner := result[a.loser], result[a.winner]
		if !winner.Active() {
			continue
		}
		loser.ArchivedAt = nowStr
		loser.MergedInto = FacetRef(winner.Dimension, winner.Key)
		loser.ArchiveReason = mergeArchiveReasonPrefix + " (witness merge, " + nowStr[:10] + ")"
		if wc, lc := winner.Current(), loser.Current(); wc != nil && lc != nil {
			wc.BecauseOf = unionIDs(wc.BecauseOf, lc.BecauseOf)
		}
	}
	// Backstop: a facet archived into itself has no active copy anywhere, and drops out of the
	// profile until a review happens to re-assert its key. Whatever produced it, undo it.
	for _, fc := range result {
		if dim, key, ok := ParseFacetRef(fc.MergedInto); ok && dim == fc.Dimension && key == fc.Key {
			fc.ArchivedAt, fc.MergedInto, fc.ArchiveReason = "", "", ""
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

// adoptShared folds an incoming facet whose history ours shares into ours (rule 1), and reports
// whether anything about ours changed — a facet-only delta is a real change, and the CLI's
// "nothing to merge" must not hide it.
//
// Archive state follows three rules, each from a reproduced failure:
//   - An archive the MERGE made (a same-key loser) is bookkeeping and is never adopted. Adopting it
//     on a merge back archived the winning facet into itself.
//   - A facet of ours archived INTO a survivor stays archived, whatever the other side's LastSeen
//     says. The other machine re-confirming its own copy after a merge un-archived the merge's loser
//     and left two active copies of one trait. (The reviewer's own rule is the same: evidence for a
//     merged-into facet is redirected, never used to resurrect it.)
//   - Otherwise the side that acted on the facet LAST wins: a facet the other side compacted away
//     stays archived, and one we merely retired comes back if they reasserted it afterwards.
func adoptShared(b, f *Facet, rel int, baseTS, incTS map[string]string) bool {
	before := cloneFacets([]Facet{*b})[0]
	n := min(len(b.Versions), len(f.Versions))
	for i := 0; i < n; i++ {
		bv, iv := &b.Versions[i], &f.Versions[i]
		if evidenceTime(incTS, iv) > evidenceTime(baseTS, bv) {
			bv.Confidence, bv.ConfidenceSeq = iv.Confidence, iv.ConfidenceSeq
		}
		// Only when the union adds an id. Assigning it unconditionally turned a facet with no
		// citations (nil) into an empty slice, which the change check below counted as an update —
		// on a real archive, an identical copy reported "4 updated" and was not a no-op.
		if u := unionIDs(bv.BecauseOf, iv.BecauseOf); len(u) != len(bv.BecauseOf) {
			bv.BecauseOf = u
		}
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
	mergeMade := strings.HasPrefix(f.ArchiveReason, mergeArchiveReasonPrefix)
	mergedIntoSurvivor := b.ArchivedAt != "" && b.MergedInto != ""
	if !mergeMade && !mergedIntoSurvivor && (b.ArchivedAt != f.ArchivedAt || b.MergedInto != f.MergedInto) {
		if max(f.ArchivedAt, f.LastSeen) > max(b.ArchivedAt, b.LastSeen) {
			b.ArchivedAt, b.MergedInto, b.ArchiveReason = f.ArchivedAt, f.MergedInto, f.ArchiveReason
		}
	}
	if f.LastSeen > b.LastSeen {
		b.LastSeen = f.LastSeen
	}
	return !reflect.DeepEqual(before, *b)
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
