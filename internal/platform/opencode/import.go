// Package opencode imports OpenCode's local session database into witness's L0.
package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
)

const (
	SessionPrefix        = "opencode:"
	syncMetaKey          = "opencode_sync_time_updated_ms"
	importKeysMetaPrefix = "opencode_import_keys:"
)

// Importer mirrors OpenCode text messages into witness raw records. It treats
// OpenCode as the source of truth and uses a message-id/content key list as the
// import watermark per session because OpenCode rows can be completed or edited
// after an earlier sync.
type Importer struct {
	Store  *store.Store
	DBPath string
}

type ImportStats struct {
	Sessions   int
	Records    int
	MaxUpdated int64
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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db"), nil
}

// Import imports the requested OpenCode sessions. With no session ids, it imports
// sessions updated since the last full sync; the first full sync imports all
// existing OpenCode sessions.
func (im *Importer) Import(ctx context.Context, sessionIDs []string) (ImportStats, error) {
	var stats ImportStats
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

	sessions, err := im.sessions(ctx, db, sessionIDs)
	if err != nil {
		return stats, err
	}
	for _, s := range sessions {
		n, err := im.importSession(ctx, db, s)
		if err != nil {
			return stats, err
		}
		if n > 0 {
			stats.Sessions++
			stats.Records += n
		}
		if s.TimeUpdated > stats.MaxUpdated {
			stats.MaxUpdated = s.TimeUpdated
		}
	}
	if len(sessionIDs) == 0 && stats.MaxUpdated > 0 {
		if err := im.Store.SetMetaString(syncMetaKey, strconv.FormatInt(stats.MaxUpdated, 10)); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func sqliteURI(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("cache", "shared")
	u.RawQuery = q.Encode()
	return u.String()
}

func (im *Importer) sessions(ctx context.Context, db *sql.DB, ids []string) ([]sessionRow, error) {
	// Exclude witness's OWN distill sessions by NEGATING the shared self-traffic
	// predicate (the same one cleanup DELETEs by), so the read and delete sides can
	// never disagree. Agent-authoritative: this fixes the old title-only filter,
	// which OpenCode's auto-titler could defeat by renaming a witness-distill
	// session — letting witness's own lens-prompt + analysis get ingested as a user
	// session. Older schemas with no agent column fall back to the title match.
	self, selfArgs := selfTrafficWhere(sessionHasAgentColumn(ctx, db))
	notSelf := `NOT (` + self + `)`

	const cols = `SELECT id, directory, title, time_created, time_updated FROM session`
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]any, 0, len(ids)+len(selfArgs))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, strings.TrimPrefix(id, SessionPrefix))
		}
		args = append(args, selfArgs...)
		q := cols + ` WHERE id IN (` + strings.Join(placeholders, ",") + `) AND ` + notSelf + ` ORDER BY time_updated`
		return scanSessions(ctx, db, q, args...)
	}
	last, _ := strconv.ParseInt(strings.TrimSpace(im.Store.MetaString(syncMetaKey)), 10, 64)
	if last > 0 {
		args := append([]any{last}, selfArgs...)
		return scanSessions(ctx, db, cols+` WHERE time_updated >= ? AND `+notSelf+` ORDER BY time_updated`, args...)
	}
	return scanSessions(ctx, db, cols+` WHERE `+notSelf+` ORDER BY time_updated`, selfArgs...)
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
	if sameKeys(oldKeys, keys) && rawCount == len(keys) {
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

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keysHavePrefix(keys, prefix []string) bool {
	if len(prefix) > len(keys) {
		return false
	}
	for i := range prefix {
		if keys[i] != prefix[i] {
			return false
		}
	}
	return true
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
