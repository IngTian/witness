package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

// The message-to-turn rule exists twice: readTurns builds L0 from OpenCode's database, and
// exportedTranscriptDigest rebuilds the same transcript from `opencode export` to prove the session
// is unchanged since capture. If the two drift, every native mine fails with "changed after L0
// capture" and backs off forever, stalling the whole OpenCode archive. So the same message set,
// covering every case either rule filters, must give the same digest through both.
func TestImportAndExportDigestAgreeOnTheTurnRule(t *testing.T) {
	type part struct{ Type, Text string }
	msgs := []struct {
		role      string
		completed int64
		parts     []part
	}{
		{"user", 0, []part{{"text", "  help me debug this  "}}},
		{"assistant", 40, []part{{"text", "first note"}, {"tool", ""}, {"text", "   "}, {"text", "final answer"}}},
		{"system", 0, []part{{"text", "a system prompt is never a turn"}}},
		{"assistant", 0, []part{{"text", "an incomplete stream"}}},
		{"user", 0, []part{{"text", " \n "}}},
		{"user", 0, []part{{"file", "file content must stay out"}, {"text", "thanks"}}},
	}

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	type exportedMessage struct {
		Info  map[string]any   `json:"info"`
		Parts []map[string]any `json:"parts"`
	}
	var export struct {
		Messages []exportedMessage `json:"messages"`
	}
	for i, m := range msgs {
		info := map[string]any{"role": m.role, "time": map[string]any{"completed": m.completed}}
		data, _ := json.Marshal(info)
		id, ts := fmt.Sprintf("msg_%d", i), int64(100*(i+1))
		if _, err := db.Exec(`INSERT INTO message VALUES (?, 'ses', ?, ?, ?)`, id, ts, ts, string(data)); err != nil {
			t.Fatal(err)
		}
		em := exportedMessage{Info: info}
		for j, p := range m.parts {
			pd := map[string]any{"type": p.Type, "text": p.Text}
			pdata, _ := json.Marshal(pd)
			if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, 'ses', ?, ?, ?)`,
				fmt.Sprintf("prt_%d_%d", i, j), id, ts+int64(j), ts+int64(j), string(pdata)); err != nil {
				t.Fatal(err)
			}
			em.Parts = append(em.Parts, pd)
		}
		export.Messages = append(export.Messages, em)
	}

	turns, err := readTurns(context.Background(), db, "ses")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("readTurns kept %d turns, want 3 (two users, one completed assistant): %+v", len(turns), turns)
	}
	entries := make([]platform.TranscriptEntry, len(turns))
	for i, tn := range turns {
		entries[i] = platform.TranscriptEntry{Role: tn.Role, Text: tn.Text}
	}
	exported, _ := json.Marshal(export)
	got, err := exportedTranscriptDigest(exported)
	if err != nil {
		t.Fatal(err)
	}
	if want := platform.TranscriptDigest(entries); got != want {
		t.Fatalf("the export digest disagrees with L0 built by readTurns, so every native mine of this "+
			"session would fail as changed after capture\nL0 turns: %+v", entries)
	}
}
