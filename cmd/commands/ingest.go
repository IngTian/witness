package commands

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

type ingestRecord struct {
	Text    string `json:"text"`
	ID      string `json:"id"`
	Session string `json:"session"`
	TS      string `json:"ts"`
	Role    string `json:"role"`
}

type ingestSession struct {
	Session string
	Records []store.RawRecord
	Keys    []string
	IDs     []string // parallel to Records/Keys: the original caller id (or "" for hash-only)
}

// parseNDJSON reads one JSON record per line. Blank lines are ignored; a line that
// isn't valid JSON, or whose text is empty, is skipped and counted — ingest is
// best-effort and never fails the whole batch on one bad record.
func parseNDJSON(r io.Reader) (recs []ingestRecord, skipped int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // allow large document lines
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec ingestRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || strings.TrimSpace(rec.Text) == "" {
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	// Check scanner error after the loop: a >16MB line (or newline-less stream)
	// would silently truncate the rest of the input. Log it so the truncation is
	// observable — do NOT return an error (ingest stays best-effort).
	if err := sc.Err(); err != nil {
		slog.Warn("NDJSON parse incomplete (scanner error, likely truncated input)", "error", err)
	}
	return recs, skipped
}

// recordKey is the dedup identity for one record. A caller id is authoritative: the
// key is id + a short content hash, so a re-sent identical record is a stable skip and
// an edited one (same id, changed text) yields a new key → update. With no id we fall
// back to a pure content hash (distinct "h:" namespace).
func recordKey(id, text string) string {
	h := sha256.Sum256([]byte(text))
	if strings.TrimSpace(id) != "" {
		return id + ":" + fmt.Sprintf("%x", h[:8])
	}
	return "h:" + fmt.Sprintf("%x", h[:16])
}

// groupSessions turns parsed records into per-session L0 batches. A record's `session`
// groups it; an empty `session` makes the record its own session (id-derived, else
// hash-derived). Every RawRecord gets a non-empty ts (record ts, else now), a role
// (else "document"), and a per-session seq. Session ids are "file:"-prefixed.
func groupSessions(recs []ingestRecord, now time.Time) []ingestSession {
	order := []string{}
	byID := map[string]*ingestSession{}
	nowStr := now.UTC().Format(time.RFC3339)
	for _, rec := range recs {
		key := recordKey(rec.ID, rec.Text)
		sid := rec.Session
		if strings.TrimSpace(sid) == "" {
			// own session: id-derived, else the content-hash key
			base := rec.ID
			if strings.TrimSpace(base) == "" {
				base = key
			}
			sid = base
		}
		sid = "file:" + sid
		s := byID[sid]
		if s == nil {
			s = &ingestSession{Session: sid}
			byID[sid] = s
			order = append(order, sid)
		}
		role := rec.Role
		if strings.TrimSpace(role) == "" {
			role = "document"
		}
		ts := strings.TrimSpace(rec.TS)
		if ts == "" {
			ts = nowStr
		}
		s.Records = append(s.Records, store.RawRecord{
			Session: sid,
			Seq:     len(s.Records),
			TS:      ts,
			Role:    role,
			Text:    rec.Text,
		})
		s.Keys = append(s.Keys, key)
		s.IDs = append(s.IDs, rec.ID) // track the original caller id
	}
	out := make([]ingestSession, 0, len(order))
	for _, sid := range order {
		out = append(out, *byID[sid])
	}
	return out
}

func newIngestCmd() *cobra.Command {
	var file string
	var quiet bool
	c := &cobra.Command{
		Use:     "ingest",
		GroupID: groupRead, // records-in sits with the read/data surface
		Short:   "Ingest records (NDJSON) into the archive and distill them.",
		Long: strings.TrimSpace(`
Feed records into witness as a source it distills — one JSON object per line (NDJSON),
from stdin or --file. Each record: {"text": "...", "id": "stable-key", "session":
"optional-group", "ts": "2026-07-26", "role": "document"}. Only text is required.

witness owns the format, not your data: convert your source (notes, articles, docs)
into records; witness distills them like any other session. Re-ingesting the same id
is idempotent; a changed body under the same id updates it.`),
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var r io.Reader = os.Stdin
			if strings.TrimSpace(file) != "" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			ing, skip, err := cmdIngest(r, quiet)
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Printf("ingested %d record(s)", ing)
				if skip > 0 {
					fmt.Printf(", skipped %d malformed", skip)
				}
				fmt.Println("; distilling in the background — witness status to watch")
			}
			return nil
		},
	}
	c.Flags().StringVar(&file, "file", "", "read NDJSON from this path instead of stdin")
	c.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable output")
	return c
}

// cmdIngest is the testable driver: parse → group → per-session dedup+write → kick the
// worker. Returns records written + malformed lines skipped.
func cmdIngest(reader io.Reader, quiet bool) (ingested, skipped int, err error) {
	recs, skipped := parseNDJSON(reader)
	if len(recs) == 0 {
		return 0, skipped, nil
	}
	st, err := store.Open()
	if err != nil {
		return 0, skipped, err
	}
	defer st.Close()
	for _, s := range groupSessions(recs, time.Now()) {
		n, err := applyIngestSession(st, s)
		if err != nil {
			return ingested, skipped, err
		}
		ingested += n
	}
	if ingested > 0 {
		spawnDetached("worker-run")
	}
	return ingested, skipped, nil
}

// applyIngestSession commits one grouped session with TRUE MERGE/APPEND semantics keyed
// on the caller-supplied id (what the SCHEMA promises). Unlike OpenCode's prefix-or-replace
// (which always re-reads the FULL session), `witness ingest` allows INCREMENTAL batches under
// an explicit shared session → the new batch is NOT necessarily a prefix-superset of stored
// keys. So: load existing keys, partition incoming (skip/update/append), and merge — NEVER
// delete records the caller didn't mention.
func applyIngestSession(st *store.Store, s ingestSession) (int, error) {
	stateKey := "file_import_keys:" + s.Session
	oldKeys := parseImportKeysJSON(st.MetaString(stateKey))
	rawCount := st.RawCount(s.Session)

	// Build a map: id → (index in oldKeys, key) for existing records.
	oldKeyByID := make(map[string]struct {
		idx int
		key string
	})
	for i, key := range oldKeys {
		// Extract id from key (format: "id:hash" or "h:hash" for no-id fallback).
		// We skip "h:" keys (hash-only, no stable id) for the id-based merge.
		if colon := strings.IndexByte(key, ':'); colon > 0 && key[:colon] != "h" {
			id := key[:colon]
			oldKeyByID[id] = struct {
				idx int
				key string
			}{i, key}
		}
	}

	// Partition incoming records: skip (identical key), update (same id, changed key), append (new id).
	// We also track which old indices are touched by updates, so we know what to preserve.
	type updateOp struct {
		oldIdx     int
		newRecIdx  int
		newKey     string
		updatedKey string
	}
	var updates []updateOp
	var appends []int // indices in s.Records/Keys/IDs
	updatedIndices := make(map[int]bool)

	for i, id := range s.IDs {
		key := s.Keys[i]
		id = strings.TrimSpace(id)
		if id == "" {
			// No stable id → treat as append (hash-only keys never match by id).
			appends = append(appends, i)
			continue
		}
		if old, exists := oldKeyByID[id]; exists {
			if old.key == key {
				// Skip: identical key → idempotent no-op.
				continue
			}
			// Update: same id, but key changed (text edited).
			updates = append(updates, updateOp{old.idx, i, key, key})
			updatedIndices[old.idx] = true
		} else {
			// Append: new id.
			appends = append(appends, i)
		}
	}

	// Fast path: if nothing changed (all incoming keys already exist with same content),
	// and the stored count matches the key count (no orphans), then skip.
	if len(updates) == 0 && len(appends) == 0 && rawCount == len(oldKeys) {
		return 0, nil
	}

	// If we only have pure appends (no updates) AND the stored state is consistent,
	// use the cheap append path (replace=false, tail only).
	if len(updates) == 0 && len(appends) > 0 && rawCount == len(oldKeys) {
		// Pure append: just add the new tail.
		var appendRecs []store.RawRecord
		var appendKeys []string
		for _, idx := range appends {
			rec := s.Records[idx]
			rec.Seq = rawCount + len(appendRecs)
			appendRecs = append(appendRecs, rec)
			appendKeys = append(appendKeys, s.Keys[idx])
		}
		newKeys := append(append([]string(nil), oldKeys...), appendKeys...)
		stateValue, _ := json.Marshal(newKeys)
		meta := store.SessionMeta{Session: s.Session}
		if err := st.ApplyRawImport(meta, appendRecs, stateKey, string(stateValue), false); err != nil {
			return 0, err
		}
		st.SetSessionPlatform(s.Session, "file")
		return len(appendRecs), nil
	}

	// Complex case: we have updates, or the stored state is inconsistent (rawCount != len(oldKeys)).
	// We need to reconstruct the FULL merged record set (existing rows + updates + appends),
	// then rewrite with replace=true. Read the existing L0 to get the current records.
	existingRecs, err := st.ReadRaw(s.Session)
	if err != nil {
		return 0, err
	}

	// Build the merged set: start with existing records, apply updates in place, then append new.
	mergedRecs := make([]store.RawRecord, len(existingRecs))
	copy(mergedRecs, existingRecs)
	mergedKeys := make([]string, len(oldKeys))
	copy(mergedKeys, oldKeys)

	// Apply updates: replace the record at the old index with the new one.
	for _, upd := range updates {
		if upd.oldIdx < len(mergedRecs) && upd.oldIdx < len(mergedKeys) {
			// Preserve the original seq for the updated record (keep its position).
			newRec := s.Records[upd.newRecIdx]
			newRec.Seq = mergedRecs[upd.oldIdx].Seq
			mergedRecs[upd.oldIdx] = newRec
			mergedKeys[upd.oldIdx] = upd.updatedKey
		}
	}

	// Append new records.
	for _, idx := range appends {
		rec := s.Records[idx]
		rec.Seq = len(mergedRecs)
		mergedRecs = append(mergedRecs, rec)
		mergedKeys = append(mergedKeys, s.Keys[idx])
	}

	stateValue, _ := json.Marshal(mergedKeys)
	meta := store.SessionMeta{Session: s.Session}
	if err := st.ApplyRawImport(meta, mergedRecs, stateKey, string(stateValue), true); err != nil {
		return 0, err
	}
	st.SetSessionPlatform(s.Session, "file")
	return len(updates) + len(appends), nil
}

// parseImportKeysJSON decodes a JSON array of keys from meta storage. Mirrors
// opencode's unexported parseImportKeys. Returns nil on parse failure.
func parseImportKeysJSON(data string) []string {
	var keys []string
	if err := json.Unmarshal([]byte(data), &keys); err != nil {
		return nil
	}
	return keys
}
