package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func testImporter(t *testing.T) (*Importer, *store.Store) {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Importer{Store: st, Home: t.TempDir()}, st
}

func jsonLine(v any) string { b, _ := json.Marshal(v); return string(b) + "\n" }
func message(role, text string) string {
	return jsonLine(map[string]any{"type": "response_item", "timestamp": "2026-10-28T12:00:00.123Z",
		"payload": map[string]any{"type": "message", "role": role, "content": []textPart{{"input_text", text}}}})
}
func rollout(t *testing.T, home, id, body string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "10", "28", "rollout-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	header := jsonLine(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "timestamp": "2026-10-28T12:00:00Z", "cwd": "/work"}})
	if err := os.WriteFile(path, []byte(header+body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func runImport(t *testing.T, im *Importer, ids ...string) platform.ImportStats {
	t.Helper()
	stats, err := im.Import(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestLegacyLiveTailAndNamespace(t *testing.T) {
	im, st := testImporter(t)
	large := strings.Repeat("汉字", 40000)
	ignored := jsonLine(map[string]any{"type": "event_msg", "payload": map[string]string{"type": "user_message", "message": "duplicate", "content": "arbitrary event payload"}})
	partial := message("assistant", "answer")
	path := rollout(t, im.Home, "s1", message("system", "SECRET")+message("developer", "SECRET")+message("user", large)+ignored+partial[:len(partial)-3])
	if err := st.AppendRaw(store.RawRecord{Session: "s1", Text: "Claude's own session"}); err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im, "s1"); s.Records != 1 {
		t.Fatalf("first: %+v", s)
	}
	if s := runImport(t, im, "codex:s1"); s.Records != 0 {
		t.Fatalf("repeat: %+v", s)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(partial[len(partial)-3:])
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im); s.Records != 1 {
		t.Fatalf("completed tail: %+v", s)
	}
	recs, err := st.ReadRaw("codex:s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Text != large || recs[1].Text != "answer" || recs[1].Seq != 1 {
		t.Fatalf("bad records count=%d", len(recs))
	}
	if st.RawCount("s1") != 1 || st.SessionPlatform("codex:s1") != "codex" {
		t.Fatal("session ownership changed")
	}
	if p := platform.ForSession(st, "codex:s1"); p.Name() != "codex" {
		t.Fatal(p.Name())
	}
	if err := st.MarkDistilled("codex:s1", "default", 2); err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im); s.Records != 0 || st.DistilledCount("codex:s1", "default") != 2 {
		t.Fatal("no-op reset watermark")
	}
}

func testHistoryDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "thread_history_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE thread_history_projection_state(thread_id TEXT PRIMARY KEY,next_rollout_ordinal INTEGER)`,
		`CREATE TABLE thread_items(thread_id TEXT,turn_id TEXT,item_id TEXT,rollout_ordinal INTEGER,created_at_ms INTEGER,item_json TEXT,item_type TEXT,PRIMARY KEY(thread_id,turn_id,item_id))`,
		`INSERT INTO thread_history_projection_state VALUES('s1',10)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func putItem(t *testing.T, db *sql.DB, id string, ordinal int, kind string, body any) {
	t.Helper()
	b, _ := json.Marshal(body)
	if _, err := db.Exec(`INSERT OR REPLACE INTO thread_items VALUES('s1','turn',?,?,?,?,?)`, id, ordinal, 1793188800123, string(b), kind); err != nil {
		t.Fatal(err)
	}
}
func sqlMessages(t *testing.T, db *sql.DB) {
	putItem(t, db, "u1", 1, "userMessage", map[string]any{"type": "userMessage", "content": []textPart{{"text", "你好"}, {"image", "ignored"}, {"text", "second paragraph"}}})
	putItem(t, db, "a1", 2, "agentMessage", map[string]any{"type": "agentMessage", "text": "answer"})
	putItem(t, db, "tool", 3, "commandExecution", map[string]any{"type": "commandExecution", "command": "PRIVATE"})
	putItem(t, db, "reason", 4, "reasoning", map[string]any{"type": "reasoning", "content": "PRIVATE"})
}

func TestSQLiteAppendEditMigrationAndCleanup(t *testing.T) {
	im, st := testImporter(t)
	db := testHistoryDB(t, im.Home)
	sqlMessages(t, db)
	rollout(t, im.Home, "s1", message("user", "stale rollout must not be combined with projection"))
	if s := runImport(t, im); s.Records != 2 || s.Sessions != 1 {
		t.Fatalf("initial: %+v", s)
	}
	recs, _ := st.ReadRaw("codex:s1")
	if recs[0].Text != "你好\nsecond paragraph" {
		t.Fatal("multimodal selection wrong")
	}
	if s := runImport(t, im); s.Records != 0 {
		t.Fatalf("repeat: %+v", s)
	}
	if err := st.MarkDistilled("codex:s1", "default", 2); err != nil {
		t.Fatal(err)
	}
	putItem(t, db, "a2", 12, "agentMessage", map[string]string{"type": "agentMessage", "text": "more"})
	if _, err := db.Exec(`UPDATE thread_history_projection_state SET next_rollout_ordinal=13`); err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im, "s1"); s.Records != 1 || st.DistilledCount("codex:s1", "default") != 2 {
		t.Fatalf("append: %+v", s)
	}
	putItem(t, db, "a1", 2, "agentMessage", map[string]string{"type": "agentMessage", "text": "edited answer"})
	if _, err := db.Exec(`UPDATE thread_history_projection_state SET next_rollout_ordinal=14`); err != nil {
		t.Fatal(err)
	}
	if s := runImport(t, im); s.Records != 3 || st.DistilledCount("codex:s1", "default") != 0 {
		t.Fatalf("edit must reset: %+v", s)
	}
	recs, _ = st.ReadRaw("codex:s1")
	if len(recs) != 3 || recs[1].Text != "edited answer" {
		t.Fatal("edit missing")
	}
	if _, _, err := st.PruneSessionsBefore("2027-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if st.MetaString(statePrefix+"codex:s1") != "" {
		t.Fatal("cleanup leaked manifest")
	}
	if s := runImport(t, im); s.Records != 3 {
		t.Fatalf("rehydrate: %+v", s)
	}
}

type failCommit struct{ store.ImportStore }

func (f failCommit) ApplyRawImport(store.SessionMeta, []store.RawRecord, string, string, bool) error {
	return fmt.Errorf("simulated commit failure")
}

func TestCommitRetryAndErrors(t *testing.T) {
	im, st := testImporter(t)
	rollout(t, im.Home, "s1", message("user", "hello"))
	im.Store = failCommit{st}
	if _, err := im.Import(context.Background(), nil); err == nil {
		t.Fatal("commit failure hidden")
	}
	if st.RawCount("codex:s1") != 0 || st.MetaString(statePrefix+"codex:s1") != "" {
		t.Fatal("watermark advanced past failed commit")
	}
	im.Store = st
	if s := runImport(t, im); s.Records != 1 {
		t.Fatal(s)
	}
	if _, err := im.Import(context.Background(), []string{"missing"}); err == nil {
		t.Fatal("missing session hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := im.Import(ctx, nil); err == nil {
		t.Fatal("cancel ignored")
	}
	t.Setenv("WITNESS_CODEX_HOME", im.Home)
	unlock, ok := st.ImportLock("codex")
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	if s, err := (Platform{}).Import(context.Background(), st, nil); err != nil || s.Records != 0 {
		t.Fatalf("held lock: %+v, %v", s, err)
	}
}

func TestHomeAndReadOnlyURI(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", a)
	t.Setenv("WITNESS_CODEX_HOME", b)
	if p, err := codexHome(); err != nil || p != b {
		t.Fatalf("home %q %v", p, err)
	}
	t.Setenv("WITNESS_CODEX_HOME", "")
	if p, err := codexHome(); err != nil || p != a {
		t.Fatalf("home %q %v", p, err)
	}
	if uri := readOnlyURI(filepath.Join(a, "sp ace#中文.sqlite")); !strings.Contains(uri, "mode=ro") || strings.Contains(uri, "#中文") {
		t.Fatal(uri)
	}
	if _, ok := any(Platform{}).(platform.RunnerProvider); ok {
		t.Fatal("capture source unexpectedly provides a runner")
	}
}
