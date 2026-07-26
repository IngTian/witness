package commands

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
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
	}
	out := make([]ingestSession, 0, len(order))
	for _, sid := range order {
		out = append(out, *byID[sid])
	}
	return out
}
