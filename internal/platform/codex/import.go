package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

const statePrefix = "codex_import_state:"

type Importer struct {
	Store store.ImportStore
	Home  string
}

type importState struct {
	Version  int      `json:"version"`
	Source   string   `json:"source"`
	Revision string   `json:"revision"`
	Keys     []string `json:"keys"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func codexHome() (string, error) {
	for _, key := range []string{"WITNESS_CODEX_HOME", "CODEX_HOME"} {
		if p := strings.TrimSpace(os.Getenv(key)); p != "" {
			return filepath.Abs(p)
		}
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".codex"), nil
}

func readOnlyURI(path string) string {
	p, _ := filepath.Abs(path)
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String()
}

// Import reads only supported conversation text, never credentials, config,
// reasoning, tool arguments/results or compaction summaries. SQLite projection
// cursors and file revisions avoid rescanning unchanged conversations. Keys and
// raw rows commit together, so a crash can always retry the session safely.
func (im *Importer) Import(ctx context.Context, ids []string) (platform.ImportStats, error) {
	stats := platform.ImportStats{Agent: "codex"}
	if im.Store == nil {
		return stats, fmt.Errorf("store is required")
	}
	home := im.Home
	if home == "" {
		var err error
		home, err = codexHome()
		if err != nil {
			return stats, err
		}
	}
	if _, err := os.Stat(home); err != nil {
		return stats, fmt.Errorf("open Codex home: %w", err)
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[strings.TrimPrefix(id, "codex:")] = true
	}
	seen := map[string]bool{}
	dbPath := filepath.Join(home, "thread_history_1.sqlite")
	if _, err := os.Stat(dbPath); err == nil {
		if err := im.importDB(ctx, dbPath, wanted, seen, &stats); err != nil {
			return stats, err
		}
	} else if !os.IsNotExist(err) {
		return stats, err
	}
	for _, folder := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(home, folder)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			return im.importFile(ctx, path, wanted, seen, &stats)
		})
		if err != nil {
			return stats, err
		}
	}
	for id := range wanted {
		if !seen[id] {
			return stats, fmt.Errorf("Codex session %q not found in supported local history", id)
		}
	}
	return stats, nil
}

func (im *Importer) unchanged(id, source, revision string) bool {
	var old importState
	if json.Unmarshal([]byte(im.Store.MetaString(statePrefix+"codex:"+id)), &old) != nil {
		return false
	}
	return old.Version == 1 && old.Source == source && old.Revision == revision && im.Store.RawCount("codex:"+id) == len(old.Keys)
}

func (im *Importer) apply(meta store.SessionMeta, source, revision string, records []store.RawRecord, identities []string, stats *platform.ImportStats) error {
	if len(records) == 0 {
		return nil
	} // deletion from Codex never erases the archive
	keys := make([]string, len(records))
	for i := range records {
		records[i].Session = meta.Session
		records[i].Seq = i
		body, _ := json.Marshal([]string{identities[i], records[i].Role, records[i].TS, records[i].Text})
		keys[i] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	var old importState
	key := statePrefix + meta.Session
	_ = json.Unmarshal([]byte(im.Store.MetaString(key)), &old)
	prefix := old.Version == 1 && len(old.Keys) <= len(keys) && im.Store.RawCount(meta.Session) == len(old.Keys)
	if prefix {
		for i, k := range old.Keys {
			if keys[i] != k {
				prefix = false
				break
			}
		}
	}
	tail := records
	if prefix {
		tail = records[len(old.Keys):]
	}
	state, _ := json.Marshal(importState{1, source, revision, keys})
	if err := im.Store.ApplyRawImport(meta, tail, key, string(state), !prefix); err != nil {
		return err
	}
	im.Store.SetSessionPlatform(meta.Session, "codex")
	if len(tail) > 0 {
		stats.Sessions++
		stats.Records += len(tail)
	}
	return nil
}

func (im *Importer) importDB(ctx context.Context, path string, wanted, seen map[string]bool, stats *platform.ImportStats) error {
	db, err := sql.Open("sqlite", readOnlyURI(path))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Read cursor + messages in one snapshot; a concurrent append must not stamp
	// a newer projection cursor against an older set of messages.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT thread_id, next_rollout_ordinal FROM thread_history_projection_state ORDER BY thread_id`)
	if err != nil {
		return fmt.Errorf("unsupported Codex history projection: %w", err)
	}
	type cursor struct {
		id      string
		ordinal int64
	}
	var cursors []cursor
	for rows.Next() {
		var c cursor
		if err := rows.Scan(&c.id, &c.ordinal); err != nil {
			rows.Close()
			return err
		}
		if len(wanted) == 0 || wanted[c.id] {
			cursors = append(cursors, c)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range cursors {
		revision := strconv.FormatInt(c.ordinal, 10)
		if im.unchanged(c.id, path, revision) {
			seen[c.id] = true
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT item_id, created_at_ms, item_json FROM thread_items
			WHERE thread_id = ? AND item_type IN ('userMessage','agentMessage') ORDER BY rollout_ordinal, item_id`, c.id)
		if err != nil {
			return err
		}
		var recs []store.RawRecord
		var keys []string
		for rows.Next() {
			var id, body string
			var ms int64
			if err := rows.Scan(&id, &ms, &body); err != nil {
				rows.Close()
				return err
			}
			var item struct {
				Type    string     `json:"type"`
				Text    string     `json:"text"`
				Content []textPart `json:"content"`
			}
			if err := json.Unmarshal([]byte(body), &item); err != nil {
				rows.Close()
				return fmt.Errorf("decode Codex item: %w", err)
			}
			role, text := "assistant", item.Text
			if item.Type == "userMessage" {
				role = "user"
				text = joinParts(item.Content, "text")
			} else if item.Type != "agentMessage" {
				continue
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			recs = append(recs, store.RawRecord{TS: time.UnixMilli(ms).UTC().Format(time.RFC3339), Role: role, Text: text})
			keys = append(keys, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			continue
		} // an unprojected legacy session may still have a rollout
		meta := store.SessionMeta{Session: "codex:" + c.id, Started: recs[0].TS}
		if err := im.apply(meta, path, revision, recs, keys, stats); err != nil {
			return err
		}
		seen[c.id] = true
	}
	return nil
}

func joinParts(parts []textPart, allowed ...string) string {
	var texts []string
	for _, p := range parts {
		for _, kind := range allowed {
			if p.Type == kind {
				texts = append(texts, p.Text)
				break
			}
		}
	}
	return strings.Join(texts, "\n")
}

func (im *Importer) importFile(ctx context.Context, path string, wanted, seen map[string]bool, stats *platform.ImportStats) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// Freeze at the observed size. A partially written final JSON row is retried,
	// and an actively growing rollout cannot make the import chase EOF forever.
	r := bufio.NewReader(io.LimitReader(f, info.Size()))
	header, err := r.ReadBytes('\n')
	if err != nil {
		return nil
	} // metadata is still being written
	var first struct {
		Type    string `json:"type"`
		Payload struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(header, &first); err != nil {
		return fmt.Errorf("decode Codex metadata: %w", err)
	}
	if first.Type != "session_meta" {
		return fmt.Errorf("unsupported Codex rollout metadata")
	}
	id := first.Payload.ID
	if id == "" {
		id = first.Payload.SessionID
	}
	if id == "" {
		return fmt.Errorf("Codex rollout has no session id")
	}
	if seen[id] || (len(wanted) > 0 && !wanted[id]) {
		return nil
	}
	seen[id] = true
	revision := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	if im.unchanged(id, path, revision) {
		return nil
	}
	meta := store.SessionMeta{Session: "codex:" + id, Cwd: first.Payload.Cwd, Started: first.Payload.Timestamp}
	var recs []store.RawRecord
	var keys []string
	offset := int64(len(header))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		} // never checkpoint an incomplete live row
		if err != nil {
			return err
		}
		var row struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type    string          `json:"type"`
				ID      string          `json:"id"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("decode Codex rollout at byte %d: %w", offset, err)
		}
		if row.Type == "response_item" && row.Payload.Type == "message" && (row.Payload.Role == "user" || row.Payload.Role == "assistant") {
			var parts []textPart
			if err := json.Unmarshal(row.Payload.Content, &parts); err != nil {
				return fmt.Errorf("decode Codex message content at byte %d: %w", offset, err)
			}
			text := joinParts(parts, "input_text", "output_text")
			if strings.TrimSpace(text) != "" {
				ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
				if err != nil {
					return fmt.Errorf("invalid Codex message timestamp: %w", err)
				}
				recs = append(recs, store.RawRecord{TS: ts.UTC().Format(time.RFC3339), Role: row.Payload.Role, Text: text})
				key := row.Payload.ID
				if key == "" {
					key = fmt.Sprintf("offset-%d", offset)
				}
				keys = append(keys, key)
			}
		}
		offset += int64(len(line))
	}
	return im.apply(meta, path, revision, recs, keys, stats)
}
