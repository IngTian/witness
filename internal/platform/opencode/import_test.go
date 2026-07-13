package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

func TestDefaultDBPathHonorsXDGDataHome(t *testing.T) {
	t.Setenv("WITNESS_OPENCODE_DB", "")
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	got, err := DefaultDBPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "opencode", "opencode.db")
	if got != want {
		t.Fatalf("DefaultDBPath() = %q, want %q", got, want)
	}
}

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

func TestImporterTargetsSessionsWithoutAdvancingFullSyncWatermark(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	mutateOpenCodeDB(t, dbPath, `
		INSERT INTO session VALUES ('ses_other', '/other', 'other work', 6000, 9000);
		INSERT INTO message VALUES ('msg_other', 'ses_other', 6100, 6100, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_other', 'msg_other', 'ses_other', 6100, 6100, '{"type":"text","text":"do not import me"}');
	`)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetMetaString(syncMetaKey, "1234"); err != nil {
		t.Fatal(err)
	}

	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), []string{"ses_test"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 || stats.Records != 2 {
		t.Fatalf("stats = %+v, want one targeted session", stats)
	}
	if got := st.RawCount(SessionPrefix + "ses_other"); got != 0 {
		t.Fatalf("non-target session imported %d records", got)
	}
	if got := st.MetaString(syncMetaKey); got != "1234" {
		t.Fatalf("targeted import advanced full sync watermark to %q", got)
	}
}

func TestImporterRebuildsWhenCompletedAssistantAppearsMidSession(t *testing.T) {
	dbPath := seedMutableOpenCodeDB(t)
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
	if stats.Records != 2 {
		t.Fatalf("initial import wrote %d records, want 2", stats.Records)
	}
	if err := st.MarkDistilled(SessionPrefix+"ses_mutable", 2); err != nil {
		t.Fatal(err)
	}

	mutateOpenCodeDB(t, dbPath, `
		UPDATE message SET data = '{"role":"assistant","time":{"completed":2500}}', time_updated = 2500 WHERE id = 'msg_a1';
		UPDATE session SET time_updated = 7000 WHERE id = 'ses_mutable';
	`)
	stats, err = (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 3 {
		t.Fatalf("rebuild wrote %d records, want 3", stats.Records)
	}
	raw, err := st.ReadRaw(SessionPrefix + "ses_mutable")
	if err != nil {
		t.Fatal(err)
	}
	rolesAndText := []string{}
	for _, r := range raw {
		rolesAndText = append(rolesAndText, r.Role+":"+r.Text)
	}
	want := []string{"user:USER-ONE", "assistant:ASSISTANT-ONE", "user:USER-TWO"}
	if len(rolesAndText) != len(want) {
		t.Fatalf("raw = %#v, want %#v", rolesAndText, want)
	}
	for i := range want {
		if rolesAndText[i] != want[i] {
			t.Fatalf("raw = %#v, want %#v", rolesAndText, want)
		}
	}
	if got := st.DistilledCount(SessionPrefix + "ses_mutable"); got != 0 {
		t.Fatalf("rebuild should reset distill progress, got %d", got)
	}
}

func TestImporterRebuildsWhenImportedTextChanges(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDistilled(SessionPrefix+"ses_test", 2); err != nil {
		t.Fatal(err)
	}
	mutateOpenCodeDB(t, dbPath, `
		UPDATE part SET data = '{"type":"text","text":"updated answer"}', time_updated = 7000 WHERE id = 'prt_a2';
		UPDATE session SET time_updated = 7000 WHERE id = 'ses_test';
	`)
	if _, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := st.ReadRaw(SessionPrefix + "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("got %d raw records, want rebuilt 2 records", len(raw))
	}
	if raw[1].Text != "first note\n\nupdated answer" {
		t.Fatalf("assistant text was not rebuilt: %+v", raw[1])
	}
	if got := st.DistilledCount(SessionPrefix + "ses_test"); got != 0 {
		t.Fatalf("changed text should reset distill progress, got %d", got)
	}
}

func TestImporterSkipsWitnessDistillSessions(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	mutateOpenCodeDB(t, dbPath, `UPDATE session SET title = 'witness-distill' WHERE id = 'ses_test';`)
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
	if stats.Records != 0 || stats.Sessions != 0 {
		t.Fatalf("witness-distill session should be skipped, stats = %+v", stats)
	}
	raw, err := st.ReadRaw(SessionPrefix + "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("witness-distill raw should be empty, got %d", len(raw))
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
		CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL, title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
		CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		INSERT INTO session VALUES ('ses_test', '/repo', 'normal work', 1000, 5000);
		INSERT INTO message VALUES ('msg_user', 'ses_test', 1100, 1100, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_user', 'msg_user', 'ses_test', 1100, 1100, '{"type":"text","text":"help me debug this"}');
		INSERT INTO message VALUES ('msg_assistant', 'ses_test', 2000, 4000, '{"role":"assistant","time":{"completed":4000}}');
		INSERT INTO part VALUES ('prt_a1', 'msg_assistant', 'ses_test', 2100, 2100, '{"type":"text","text":"first note"}');
		INSERT INTO part VALUES ('prt_tool', 'msg_assistant', 'ses_test', 2200, 2200, '{"type":"tool","tool":"bash"}');
		INSERT INTO part VALUES ('prt_patch', 'msg_assistant', 'ses_test', 2300, 2300, '{"type":"patch","files":[{"path":"main.go"}],"text":"patch body must stay out"}');
		INSERT INTO part VALUES ('prt_file', 'msg_assistant', 'ses_test', 2400, 2400, '{"type":"file","path":"main.go","content":"file content must stay out"}');
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

func seedMutableOpenCodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL, title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
		CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		INSERT INTO session VALUES ('ses_mutable', '/repo', 'mutable work', 1000, 6000);
		INSERT INTO message VALUES ('msg_u1', 'ses_mutable', 1100, 1100, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_u1', 'msg_u1', 'ses_mutable', 1100, 1100, '{"type":"text","text":"USER-ONE"}');
		INSERT INTO message VALUES ('msg_a1', 'ses_mutable', 2000, 2000, '{"role":"assistant","time":{"created":2000}}');
		INSERT INTO part VALUES ('prt_a1', 'msg_a1', 'ses_mutable', 2100, 2100, '{"type":"text","text":"ASSISTANT-ONE"}');
		INSERT INTO message VALUES ('msg_u2', 'ses_mutable', 3000, 3000, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_u2', 'msg_u2', 'ses_mutable', 3000, 3000, '{"type":"text","text":"USER-TWO"}');
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mutateOpenCodeDB(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}
