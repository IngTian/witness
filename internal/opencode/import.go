// Package opencode imports OpenCode's local session database into witness's L0.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/claude-witness/internal/store"
)

const (
	SessionPrefix = "opencode:"
	syncMetaKey   = "opencode_sync_time_updated_ms"
)

// Importer mirrors OpenCode text messages into witness raw records. It treats
// OpenCode as the source of truth and uses witness's raw count as the import
// watermark per session.
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
	TimeCreated int64
	TimeUpdated int64
}

type turn struct {
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
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = strings.TrimPrefix(id, SessionPrefix)
		}
		q := `SELECT id, directory, time_created, time_updated FROM session WHERE id IN (` + strings.Join(placeholders, ",") + `) ORDER BY time_updated`
		return scanSessions(ctx, db, q, args...)
	}
	last, _ := strconv.ParseInt(strings.TrimSpace(im.Store.MetaString(syncMetaKey)), 10, 64)
	if last > 0 {
		return scanSessions(ctx, db, `SELECT id, directory, time_created, time_updated FROM session WHERE time_updated >= ? ORDER BY time_updated`, last)
	}
	return scanSessions(ctx, db, `SELECT id, directory, time_created, time_updated FROM session ORDER BY time_updated`)
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

func (im *Importer) importSession(ctx context.Context, db *sql.DB, s sessionRow) (int, error) {
	turns, err := readTurns(ctx, db, s.ID)
	if err != nil {
		return 0, err
	}
	if len(turns) == 0 {
		return 0, nil
	}
	session := SessionPrefix + s.ID
	done := im.Store.RawCount(session)
	if done >= len(turns) {
		return 0, nil
	}
	if done == 0 {
		im.Store.RecordMeta(store.SessionMeta{Session: session, Cwd: s.Directory, Started: msRFC3339(s.TimeCreated)})
	}
	for i, t := range turns[done:] {
		if err := im.Store.AppendRaw(store.RawRecord{
			TS:      msRFC3339(t.TS),
			Session: session,
			Seq:     done + i,
			Role:    t.Role,
			Text:    t.Text,
		}); err != nil {
			return i, err
		}
	}
	return len(turns) - done, nil
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
			out = append(out, turn{TS: curTS, Role: curRole, Text: text})
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
