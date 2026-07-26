package commands

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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

// applyIngestSession commits one grouped session with the same skip/append/replace
// dedup protocol as the OpenCode importer, keyed on the caller-id-derived keys.
func applyIngestSession(st *store.Store, s ingestSession) (int, error) {
	stateKey := "file_import_keys:" + s.Session
	oldKeys := parseImportKeysJSON(st.MetaString(stateKey))
	rawCount := st.RawCount(s.Session)
	if keysEqual(oldKeys, s.Keys) && rawCount == len(s.Keys) {
		return 0, nil // idempotent: nothing changed
	}
	replace := true
	start := 0
	if len(oldKeys) == 0 && rawCount == 0 {
		replace = false
	} else if len(oldKeys) > 0 && keysPrefix(s.Keys, oldKeys) && rawCount == len(oldKeys) {
		replace = false
		start = len(oldKeys) // append only the new tail
	}
	stateValue, _ := json.Marshal(s.Keys)
	meta := store.SessionMeta{Session: s.Session}
	if err := st.ApplyRawImport(meta, s.Records[start:], stateKey, string(stateValue), replace); err != nil {
		return 0, err
	}
	st.SetSessionPlatform(s.Session, "file")
	return len(s.Records) - start, nil
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

// keysEqual checks if two key slices are identical. Mirrors opencode's sameKeys.
func keysEqual(a, b []string) bool {
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

// keysPrefix checks if prefix is a prefix of keys. Mirrors opencode's keysHavePrefix.
func keysPrefix(keys, prefix []string) bool {
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
