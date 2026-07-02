package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/claude-witness/internal/store"
)

func TestImporterMirrorsOpenCodeTextParts(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 || stats.Records != 2 {
		t.Fatalf("stats = %+v, want 1 session / 2 records", stats)
	}

	raw, err := st.ReadRaw(SessionPrefix + "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("got %d raw records, want 2", len(raw))
	}
	if raw[0].Role != "user" || raw[0].Text != "help me debug this" {
		t.Fatalf("bad user record: %+v", raw[0])
	}
	if raw[1].Role != "assistant" || raw[1].Text != "first note\n\nfinal answer" {
		t.Fatalf("assistant text parts not merged: %+v", raw[1])
	}

	stats, err = (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 0 {
		t.Fatalf("second import should be idempotent, wrote %d records", stats.Records)
	}
}

func seedOpenCodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
		CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		INSERT INTO session VALUES ('ses_test', '/repo', 1000, 5000);
		INSERT INTO message VALUES ('msg_user', 'ses_test', 1100, 1100, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_user', 'msg_user', 'ses_test', 1100, 1100, '{"type":"text","text":"help me debug this"}');
		INSERT INTO message VALUES ('msg_assistant', 'ses_test', 2000, 4000, '{"role":"assistant","time":{"completed":4000}}');
		INSERT INTO part VALUES ('prt_a1', 'msg_assistant', 'ses_test', 2100, 2100, '{"type":"text","text":"first note"}');
		INSERT INTO part VALUES ('prt_tool', 'msg_assistant', 'ses_test', 2200, 2200, '{"type":"tool","tool":"bash"}');
		INSERT INTO part VALUES ('prt_a2', 'msg_assistant', 'ses_test', 3000, 3000, '{"type":"text","text":"final answer"}');
		INSERT INTO message VALUES ('msg_partial', 'ses_test', 6000, 6000, '{"role":"assistant","time":{"created":6000}}');
		INSERT INTO part VALUES ('prt_partial', 'msg_partial', 'ses_test', 6000, 6000, '{"type":"text","text":"partial stream"}');
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
