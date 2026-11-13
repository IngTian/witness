// Package opencode imports OpenCode's local session database into witness's L0.
package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

const (
	SessionPrefix = "opencode:"
	syncMetaKey   = "opencode_sync_time_updated_ms"
	// seenMetaKey is the newest turn end (`idle` time_created) any import read, targeted or full: the
	// full-sync watermark moves only on full syncs, and staleness needs the clock NewestActivity
	// reads. The name predates the switch from time_updated and stays, since builds of this branch
	// already wrote it.
	seenMetaKey          = "opencode_seen_time_updated_ms"
	importKeysMetaPrefix = store.OpenCodeImportKeysNS + ":"
	StaleAfter           = 15 * time.Minute
)

// Importer mirrors OpenCode text messages into witness raw records. It treats
// OpenCode as the source of truth and uses a message-id/content key list as the
// import watermark per session because OpenCode rows can be completed or edited
// after an earlier sync.
//
// Store is the narrow store.ImportStore slice (issue #73-C1) — its own source lock,
// meta watermark, raw-count probe, and the raw-import commit — not the whole
// *store.Store, so the importer can be exercised against a fake.
type Importer struct {
	Store  store.ImportStore
	DBPath string
}

type sessionRow struct {
	ID          string
	Directory   string
	TimeCreated int64
	TimeUpdated int64
}

// Turn represents one reconstructed turn (user or completed assistant) from v2 storage.
type Turn struct {
	MessageID string
	Role      string
	Text      string
	Seq       int64
	TS        int64 // message time_created ms
}

// ErrOpenCodeSchema marks a v2 DB with a missing or incomplete schema.
var ErrOpenCodeSchema = errors.New("opencode.db schema is incomplete or still migrating")

// DefaultDBPath returns OpenCode's SQLite database path. v2 has no Windows branch and resolves
// OPENCODE_DB this way (storage §1); opencode-<channel>.db only for non-release channels, not supported.
func DefaultDBPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("WITNESS_OPENCODE_DB")); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(os.Getenv("OPENCODE_DB")); p != "" {
		if p == ":memory:" {
			// skip :memory:, fall through to default
		} else if filepath.IsAbs(p) {
			return p, nil
		} else {
			// relative → resolve against <data>/opencode
			dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
			if dataHome == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return "", err
				}
				dataHome = filepath.Join(home, ".local", "share")
			}
			return filepath.Join(dataHome, "opencode", p), nil
		}
	}
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		return filepath.Join(dataHome, "opencode", "opencode.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db"), nil
}

// ConfigDir returns the directory OpenCode reads its config (opencode.json, plugins/) from:
// OPENCODE_CONFIG_DIR, else XDG_CONFIG_HOME/opencode, else ~/.config/opencode — on Windows
// too. The XDG step mirrors OpenCode's own lookup (packages/opencode/src/global/index.ts joins
// xdg-basedir's xdgConfig, which is $XDG_CONFIG_HOME else ~/.config, with "opencode"); the
// wiring once skipped it and wrote where OpenCode never reads while reporting success.
func ConfigDir() string {
	if d := os.Getenv("OPENCODE_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode")
}

// PluginFileName is the name of the witness plugin for OpenCode, installed by `witness wire opencode`.
const PluginFileName = "witness.js"

// PluginPath returns the absolute path to the OpenCode plugin file.
func PluginPath() string {
	return filepath.Join(ConfigDir(), "plugins", PluginFileName)
}

// PluginInstalled reports whether the witness plugin file exists in OpenCode's plugins directory.
func PluginInstalled() bool {
	_, err := os.Stat(PluginPath())
	return err == nil
}

// openSourceDB opens the user's OpenCode DB read-only and probes its schema.
func openSourceDB(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open opencode db %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := ProbeSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// v2Reads is every table and column the importer reads; ProbeSchema refuses a DB lacking any.
var v2Reads = []struct {
	table string
	cols  []string
}{
	{"session_v2", []string{"id", "directory", "title", "time_created", "time_updated", "parent_id", "fork_session_id", "agent"}},
	{"session_message", []string{"id", "session_id", "type", "seq", "time_created", "data"}},
	{"session_part", []string{"message_id", "ordinal", "type", "data"}},
}

// ProbeSchema checks that the DB has the v2 tables and columns the importer reads, and names
// exactly the missing ones: after an OpenCode schema change the refusal is the only report of
// what moved. Refuses v1-only DBs and mid-migration states.
func ProbeSchema(ctx context.Context, db *sql.DB) error {
	tables, err := queryNames(ctx, db, `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session_v2','session_message','session_part','session')`)
	if err != nil {
		return err
	}
	hasV1 := slices.Contains(tables, "session")
	var missing []string
	for _, r := range v2Reads {
		if !slices.Contains(tables, r.table) {
			missing = append(missing, r.table)
		}
	}
	if len(missing) == len(v2Reads) && hasV1 {
		return fmt.Errorf("this opencode.db was written by OpenCode v1: %w", ErrOpenCodeV1)
	}
	if len(missing) > 0 {
		// Tables first: probing a missing table would report every one of its columns too
		return fmt.Errorf("%w: missing %s", ErrOpenCodeSchema, strings.Join(missing, ", "))
	}
	for _, r := range v2Reads {
		have, err := queryNames(ctx, db, `SELECT name FROM pragma_table_info(?)`, r.table)
		if err != nil {
			return err
		}
		for _, c := range r.cols {
			if !slices.Contains(have, c) {
				missing = append(missing, r.table+"."+c)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing %s", ErrOpenCodeSchema, strings.Join(missing, ", "))
	}

	// If v1 table exists, require migration completed
	if hasV1 {
		// A NULL phase and no row both read as "not completed". A kv table that cannot be read leaves
		// the state unknown, so that refuses too, worded rather than as a raw SQL error.
		var phase sql.NullString
		err := db.QueryRowContext(ctx, `SELECT json_extract(value,'$.phase') FROM kv WHERE key='migration.v1-v2'`).Scan(&phase)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %v — witness cannot tell whether OpenCode finished migrating this database from v1", ErrOpenCodeSchema, err)
		}
		if phase.String != "completed" {
			return fmt.Errorf("%w: OpenCode is still migrating this database from v1; import again when it finishes", ErrOpenCodeSchema)
		}
	}

	return nil
}

// Import imports the requested OpenCode sessions. With no session ids, it imports
// sessions updated since the last full sync; the first full sync imports all
// existing OpenCode sessions.
func (im *Importer) Import(ctx context.Context, sessionIDs []string) (platform.ImportStats, error) {
	stats := platform.ImportStats{Agent: "opencode"}
	if im.Store == nil {
		return stats, fmt.Errorf("store is required")
	}
	dbPath := strings.TrimSpace(im.DBPath)
	if dbPath == "" {
		var err error
		dbPath, err = DefaultDBPath()
		if err != nil {
			return stats, err
		}
	}
	db, err := openSourceDB(ctx, dbPath)
	if err != nil {
		return stats, err
	}
	defer db.Close()

	sessions, err := im.sessions(ctx, db, sessionIDs)
	if err != nil {
		return stats, err
	}
	// The whole session list is read before the first import, so the total is exact.
	progress := platform.ImportProgressFrom(ctx)
	progress(0, len(sessions))
	var maxUpdated int64 // the incremental-sync watermark
	var newestIdle int64 // the seen watermark, on NewestActivity's clock
	for i, s := range sessions {
		n, err := im.importSession(ctx, db, s)
		progress(i+1, len(sessions))
		if err != nil {
			return stats, err
		}
		if n > 0 {
			stats.Sessions++
			stats.Records += n
		}
		maxUpdated = max(maxUpdated, s.TimeUpdated)
		idle, err := sessionNewestIdle(ctx, db, s.ID)
		if err != nil {
			return stats, err
		}
		newestIdle = max(newestIdle, idle)
	}
	// Only upward: a targeted import of an older session must not make a newer one look unimported
	if oldSeen, _ := strconv.ParseInt(strings.TrimSpace(im.Store.MetaString(seenMetaKey)), 10, 64); newestIdle > oldSeen {
		if err := im.Store.SetMetaString(seenMetaKey, strconv.FormatInt(newestIdle, 10)); err != nil {
			return stats, err
		}
	}
	if len(sessionIDs) == 0 && maxUpdated > 0 {
		if err := im.Store.SetMetaString(syncMetaKey, strconv.FormatInt(maxUpdated, 10)); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// sqliteURI opens the user's own OpenCode database READ-ONLY with a shared cache.
//
// Routed through the shared platform.SQLiteFileURI because this was previously a second,
// independent copy of the same url.URL composition — and both copies were broken on Windows
// the same way, turning the drive letter into a URI authority (issue #10).
func sqliteURI(path string) string {
	return platform.SQLiteFileURI(path, "mode=ro&cache=shared")
}

// legacyMarkerName is the label the PRE-isolation design stamped on the scratch
// sessions witness created inside the user's own OpenCode database. Nothing writes it
// any more — distillation now runs against a private database and never touches the
// user's — but an archive that ran the old version and was killed mid-distill can still
// have such rows sitting in the user's DB today.
//
// They must never be imported: witness would ingest its own distillation chatter as if
// it were the user coding, quietly polluting the growth profile with self-talk. Since
// the user's DB is opened READ-ONLY (the invariant this package exists to hold), the fix
// is to SKIP them, not delete them — a leftover row stays visible in OpenCode's own
// session list, where the user can remove it with their own tool if they wish.
const legacyMarkerName = "witness-distill"

func (im *Importer) sessions(ctx context.Context, db *sql.DB, ids []string) ([]sessionRow, error) {
	const cols = `SELECT id, directory, time_created, time_updated FROM session_v2`
	// `agent IS NOT ?` is NULL-safe: a NULL agent is "not witness-distill" and is imported
	skip := `parent_id IS NULL AND agent IS NOT ?`
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]any, 0, len(ids)+1)
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, strings.TrimPrefix(id, SessionPrefix))
		}
		// The skip applies even to an explicitly named id: a caller asking for a legacy
		// scratch session by name still must not get witness's own output into L0.
		q := cols + ` WHERE id IN (` + strings.Join(placeholders, ",") + `) AND ` + skip + ` ORDER BY time_updated, id`
		return scanSessions(ctx, db, q, append(args, legacyMarkerName)...)
	}
	last, _ := strconv.ParseInt(strings.TrimSpace(im.Store.MetaString(syncMetaKey)), 10, 64)
	if last > 0 {
		return scanSessions(ctx, db, cols+` WHERE time_updated >= ? AND `+skip+` ORDER BY time_updated, id`, last, legacyMarkerName)
	}
	return scanSessions(ctx, db, cols+` WHERE `+skip+` ORDER BY time_updated, id`, legacyMarkerName)
}

func scanSessions(ctx context.Context, db *sql.DB, q string, args ...any) ([]sessionRow, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		if err := rows.Scan(&s.ID, &s.Directory, &s.TimeCreated, &s.TimeUpdated); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// queryNames returns the one text column q yields (table or column names for ProbeSchema).
func queryNames(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
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

// ReadTurns reconstructs the turns (user and completed assistant messages) from a v2 session.
// Merges session_part and inline content text pieces, skips synthetic/shell/system/idle/compaction rows,
// reasoning parts, fork copies (gated on fork_session_id), and unsettled assistants.
func ReadTurns(ctx context.Context, db *sql.DB, sessionID string) ([]Turn, error) {
	// The WITH clause derives the fork flag in SQL so the signature stays (ctx, db, sessionID)
	rows, err := db.QueryContext(ctx, `
		WITH msgs AS (
		  SELECT id, seq, type, time_created, data FROM session_message
		   WHERE session_id = ?1 AND type IN ('user','assistant')
		     AND NOT (COALESCE((SELECT fork_session_id IS NOT NULL FROM session_v2 WHERE id = ?1), 0)
		              AND instr(substr(id,5),'_') > 0)
		)
		SELECT id, seq, type, time_created, json_extract(data,'$.time.completed'), 0, 0, COALESCE(json_extract(data,'$.text'),'')
		  FROM msgs WHERE type = 'user'
		UNION ALL
		SELECT m.id, m.seq, m.type, m.time_created, json_extract(m.data,'$.time.completed'), 0, p.ordinal, COALESCE(json_extract(p.data,'$.text'),'')
		  FROM msgs m JOIN session_part p ON p.message_id = m.id AND p.type = 'text' WHERE m.type = 'assistant'
		UNION ALL
		SELECT m.id, m.seq, m.type, m.time_created, json_extract(m.data,'$.time.completed'), 1, CAST(c.key AS INTEGER), COALESCE(json_extract(c.value,'$.text'),'')
		  FROM msgs m, json_each(m.data,'$.content') c
		 WHERE m.type = 'assistant' AND json_extract(c.value,'$.type') = 'text'
		 ORDER BY 2, 6, 7
	`, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	var curID, curRole string
	var curSeq, curTS int64
	var cur strings.Builder
	flush := func() {
		text := strings.TrimSpace(cur.String())
		if curID != "" && text != "" {
			out = append(out, Turn{MessageID: curID, Role: curRole, Text: text, Seq: curSeq, TS: curTS})
		}
		curID, curRole, curSeq, curTS = "", "", 0, 0
		cur.Reset()
	}

	for rows.Next() {
		var msgID, role, text string
		var seq, ts, completed sql.NullInt64
		var src, ord int64
		if err := rows.Scan(&msgID, &seq, &role, &ts, &completed, &src, &ord, &text); err != nil {
			return nil, err
		}
		// Skip assistant rows whose completed IS NULL
		if role == "assistant" && !completed.Valid {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if curID != msgID {
			flush()
			curID, curRole, curSeq, curTS = msgID, role, seq.Int64, ts.Int64
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(text)
	}
	flush()
	return out, rows.Err()
}

// TranscriptDigest computes a digest of the turn sequence, matching the import's L0 shape.
func TranscriptDigest(turns []Turn) string {
	entries := make([]platform.TranscriptEntry, len(turns))
	for i, t := range turns {
		entries[i] = platform.TranscriptEntry{Role: t.Role, Text: t.Text}
	}
	return platform.TranscriptDigest(entries)
}

// NewestActivity returns the newest COMPLETED turn (an `idle` record) over importable sessions.
// `idle` marks a turn's end (storage §3) and does not touch `time_updated` (storage §4), so it is
// never newer than what the turn-end import saw by more than the idle write; `time_updated` moves
// at every step start, so it would flag a turn that is merely still running.
func NewestActivity(ctx context.Context, dbPath string) (time.Time, error) {
	db, err := openSourceDB(ctx, dbPath)
	if err != nil {
		return time.Time{}, err
	}
	defer db.Close()

	var ms int64
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(m.time_created),0)
		  FROM session_message m
		  JOIN session_v2 s ON s.id = m.session_id
		 WHERE m.type = 'idle'
		   AND s.parent_id IS NULL
		   AND s.agent IS NOT ?`, legacyMarkerName).Scan(&ms)
	if err != nil {
		return time.Time{}, err
	}
	if ms == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(ms), nil
}

// sessionNewestIdle is the session's newest turn end, 0 when none has ended. Seen is recorded on
// this clock, not time_updated: an idle written with no step after it (an interrupt before the
// first step, a refused route) is newer than any time_updated, so comparing the two warned about
// a turn the import had already read.
func sessionNewestIdle(ctx context.Context, db *sql.DB, sessionID string) (int64, error) {
	var ms int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(time_created),0) FROM session_message WHERE session_id = ? AND type = 'idle'`, sessionID).Scan(&ms)
	return ms, err
}

// LastImported returns the newest turn end any import (targeted OR full) read, zero when unset.
func LastImported(st store.MetaKV) time.Time {
	ms, _ := strconv.ParseInt(strings.TrimSpace(st.MetaString(seenMetaKey)), 10, 64)
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// ForgetCapture is unwire's half of the capture switch. The seen watermark is what keeps
// ImportAtDrainStart, and status's capture rows, on for a machine with no plugin file, so leaving
// it would keep importing an editor the user turned off. The full-sync watermark stays, so a
// re-wire resumes incrementally. A still-loaded npm plugin sets it again on its next import, which
// is right: that OpenCode is still wired.
func ForgetCapture(st store.MetaKV) error { return st.SetMetaString(seenMetaKey, "") }

func (im *Importer) importSession(ctx context.Context, db *sql.DB, s sessionRow) (int, error) {
	turns, err := ReadTurns(ctx, db, s.ID)
	if err != nil {
		return 0, err
	}
	if len(turns) == 0 {
		return 0, nil
	}
	session := SessionPrefix + s.ID
	keys := turnKeys(turns)
	stateKey := importKeysMetaPrefix + session
	oldKeys := parseImportKeys(im.Store.MetaString(stateKey))
	rawCount := im.Store.RawCount(session)
	if slices.Equal(oldKeys, keys) && rawCount == len(keys) {
		return 0, nil
	}

	replace := true
	start := 0
	if len(oldKeys) == 0 && rawCount == 0 {
		replace = false
	} else if len(oldKeys) > 0 && keysHavePrefix(keys, oldKeys) && rawCount == len(oldKeys) {
		replace = false
		start = len(oldKeys)
	}
	records := rawRecords(session, turns[start:], start)
	stateValue, err := json.Marshal(keys)
	if err != nil {
		return 0, err
	}
	meta := store.SessionMeta{Session: session, Cwd: s.Directory, Started: msRFC3339(s.TimeCreated)}
	if err := im.Store.ApplyRawImport(meta, records, stateKey, string(stateValue), replace); err != nil {
		return 0, err
	}
	// Stamp the owning platform so platform.ForSession is column-authoritative
	// (prefix remains the fallback for rows imported before this).
	im.Store.SetSessionPlatform(session, "opencode")
	return len(records), nil
}

func turnKeys(turns []Turn) []string {
	keys := make([]string, len(turns))
	for i, t := range turns {
		keys[i] = messageKey(t.MessageID, t.Role, t.Text)
	}
	return keys
}

func messageKey(id, role, text string) string {
	h := sha256.Sum256([]byte(role + "\x00" + text))
	return id + ":" + fmt.Sprintf("%x", h[:8])
}

func parseImportKeys(data string) []string {
	var keys []string
	if err := json.Unmarshal([]byte(data), &keys); err != nil {
		return nil
	}
	return keys
}

func keysHavePrefix(keys, prefix []string) bool {
	return len(prefix) <= len(keys) && slices.Equal(keys[:len(prefix)], prefix)
}

func rawRecords(session string, turns []Turn, seqOffset int) []store.RawRecord {
	records := make([]store.RawRecord, len(turns))
	for i, t := range turns {
		records[i] = store.RawRecord{
			TS:      msRFC3339(t.TS),
			Session: session,
			Seq:     seqOffset + i,
			Role:    t.Role,
			Text:    t.Text,
		}
	}
	return records
}

func msRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
