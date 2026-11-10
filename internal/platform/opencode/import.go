// Package opencode imports OpenCode's local session database into witness's L0.
package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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
	SessionPrefix        = "opencode:"
	syncMetaKey          = "opencode_sync_time_updated_ms"
	importKeysMetaPrefix = store.OpenCodeImportKeysNS + ":"
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
	Title       string
	TimeCreated int64
	TimeUpdated int64
}

type turn struct {
	Key  string
	TS   int64
	Role string
	Text string
}

// DefaultDBPath returns OpenCode's default SQLite database path. Override with
// WITNESS_OPENCODE_DB for tests or non-standard installs.
func DefaultDBPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("WITNESS_OPENCODE_DB")); p != "" {
		return p, nil
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
	if _, err := os.Stat(dbPath); err != nil {
		return stats, fmt.Errorf("open opencode db %s: %w", dbPath, err)
	}
	db, err := sql.Open("sqlite", sqliteURI(dbPath))
	if err != nil {
		return stats, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := refuseV2Database(ctx, db); err != nil {
		return stats, err
	}
	sessions, err := im.sessions(ctx, db, sessionIDs)
	if err != nil {
		return stats, err
	}
	// The whole session list is read before the first import, so the total is exact.
	progress := platform.ImportProgressFrom(ctx)
	progress(0, len(sessions))
	var maxUpdated int64 // the incremental-sync watermark
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
	}
	if len(sessionIDs) == 0 && maxUpdated > 0 {
		if err := im.Store.SetMetaString(syncMetaKey, strconv.FormatInt(maxUpdated, 10)); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// refuseV2Database stops the import when OpenCode v2 owns the database. v2 migrates opencode.db
// IN PLACE into new tables (session_v2, …) and stops writing the v1 ones this importer reads, so
// a v1 read of a v2-active database is a frozen snapshot: capture would stop and every import
// would report "up to date". The signal is which table holds the NEWEST activity, not whether
// session_v2 exists: a user who tried v2 and went back to v1 keeps writing the v1 tables (#218).
func refuseV2Database(ctx context.Context, db *sql.DB) error {
	var hasV2, hasV1 int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_v2'`).Scan(&hasV2); err != nil || hasV2 == 0 {
		return nil // no v2 tables; an unreadable schema is reported by the session query itself
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session'`).Scan(&hasV1); err != nil {
		return err
	}
	if hasV1 == 0 {
		return fmt.Errorf("this opencode database was created by OpenCode v2: %w", ErrOpenCodeV2)
	}
	var newestV1, newestV2 int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(time_updated), 0) FROM session`).Scan(&newestV1); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(time_updated), 0) FROM session_v2`).Scan(&newestV2); err != nil {
		return err
	}
	if newestV2 > newestV1 {
		return fmt.Errorf("OpenCode v2 has migrated this opencode database and is writing it (its v1 tables stopped updating): %w", ErrOpenCodeV2)
	}
	return nil
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

// excludeLegacyScratch returns the predicate that skips leftover pre-isolation scratch
// sessions, keyed on the `agent` column when the running OpenCode has one and falling
// back to `title` for older schemas.
//
// It uses `IS NOT`, never `NOT (col = ?)`: under SQL's three-valued logic a NULL agent
// makes `agent = ?` NULL and `NOT NULL` falsy, which would SILENTLY DROP every genuine
// user session whose agent is NULL — sessions predating OpenCode's ADD COLUMN carry
// exactly that, so the naive negation would lose real capture data on the first full
// backfill. SQLite's `IS NOT` compares NULL-safely, so a NULL agent correctly reads as
// "not witness's own" and is imported.
func excludeLegacyScratch(hasAgent bool) (clause string, arg any) {
	if hasAgent {
		return `agent IS NOT ?`, legacyMarkerName
	}
	return `title IS NOT ?`, legacyMarkerName
}

// sessionHasAgentColumn reports whether the running OpenCode's session table carries an
// `agent` column, so the skip predicate keys on the authoritative one. Agent is
// preferred because OpenCode's auto-titler can rewrite a session's title after the fact,
// whereas the agent is set at creation and never rewritten.
func sessionHasAgentColumn(ctx context.Context, db *sql.DB) bool {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('session') WHERE name = 'agent'`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (im *Importer) sessions(ctx context.Context, db *sql.DB, ids []string) ([]sessionRow, error) {
	const cols = `SELECT id, directory, title, time_created, time_updated FROM session`
	skip, marker := excludeLegacyScratch(sessionHasAgentColumn(ctx, db))
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]any, 0, len(ids)+1)
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, strings.TrimPrefix(id, SessionPrefix))
		}
		// The skip applies even to an explicitly named id: a caller asking for a legacy
		// scratch session by name still must not get witness's own output into L0.
		q := cols + ` WHERE id IN (` + strings.Join(placeholders, ",") + `) AND ` + skip + ` ORDER BY time_updated`
		return scanSessions(ctx, db, q, append(args, marker)...)
	}
	last, _ := strconv.ParseInt(strings.TrimSpace(im.Store.MetaString(syncMetaKey)), 10, 64)
	if last > 0 {
		return scanSessions(ctx, db, cols+` WHERE time_updated >= ? AND `+skip+` ORDER BY time_updated`, last, marker)
	}
	return scanSessions(ctx, db, cols+` WHERE `+skip+` ORDER BY time_updated`, marker)
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
		if err := rows.Scan(&s.ID, &s.Directory, &s.Title, &s.TimeCreated, &s.TimeUpdated); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (im *Importer) importSession(ctx context.Context, db *sql.DB, s sessionRow) (int, error) {
	turns, err := readTurns(ctx, db, s.ID)
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

func turnKeys(turns []turn) []string {
	keys := make([]string, len(turns))
	for i, t := range turns {
		keys[i] = t.Key
	}
	return keys
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

func rawRecords(session string, turns []turn, seqOffset int) []store.RawRecord {
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

func readTurns(ctx context.Context, db *sql.DB, sessionID string) ([]turn, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.time_created, m.data, p.time_created, p.id, p.data
		  FROM message m
		  JOIN part p ON p.message_id = m.id
		 WHERE m.session_id = ?
		 ORDER BY m.time_created, m.id, p.time_created, p.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []turn
	var curID, curRole string
	var curTS int64
	var cur strings.Builder
	flush := func() {
		text := strings.TrimSpace(cur.String())
		if curID != "" && text != "" {
			out = append(out, turn{Key: messageKey(curID, curRole, text), TS: curTS, Role: curRole, Text: text})
		}
		curID, curRole, curTS = "", "", 0
		cur.Reset()
	}

	for rows.Next() {
		var msgID, msgData, partID, partData string
		var msgTS, partTS int64
		if err := rows.Scan(&msgID, &msgTS, &msgData, &partTS, &partID, &partData); err != nil {
			return nil, err
		}
		info := parseMessageInfo(msgData)
		role := info.Role
		if role != "user" && role != "assistant" {
			continue
		}
		if role == "assistant" && info.Time.Completed == 0 {
			continue
		}
		text, ok := textPart(partData)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		if curID != msgID {
			flush()
			curID, curRole, curTS = msgID, role, partTS
			if curTS == 0 {
				curTS = msgTS
			}
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(text)
	}
	flush()
	return out, rows.Err()
}

func messageKey(id, role, text string) string {
	h := sha256.Sum256([]byte(role + "\x00" + text))
	return id + ":" + fmt.Sprintf("%x", h[:8])
}

type messageInfo struct {
	Role string `json:"role"`
	Time struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
}

func parseMessageInfo(data string) messageInfo {
	var m messageInfo
	_ = json.Unmarshal([]byte(data), &m)
	return m
}

func textPart(data string) (string, bool) {
	var p struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return "", false
	}
	return p.Text, p.Type == "text"
}

func msRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
