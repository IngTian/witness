package commands

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// seedOpenCodeV2DB creates a temporary OpenCode v2 DB with one session (ses_fixture):
// user "hello", completed assistant "hi", idle record. Both time_updated and the idle's
// time_created are set to updatedMs. Returns the DB path.
func seedOpenCodeV2DB(t *testing.T, updatedMs int64) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("../../internal/platform/opencode/testdata", "opencode-2.2.7-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		string(schema),
		// session
		"INSERT INTO session_v2 (id, project_id, slug, directory, version, time_created, time_updated) VALUES ('ses_fixture', 'prj', 'slug', '/repo', '2.2.7', 1000, " + strconv.FormatInt(updatedMs, 10) + ");",
		// user message
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_user_1', 'ses_fixture', 'user', 1, 2000, 2000, '{\"time\":{\"created\":2000},\"text\":\"hello\"}');",
		// completed assistant
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_asst_2', 'ses_fixture', 'assistant', 2, 3000, 3000, '{\"time\":{\"created\":3000,\"completed\":3100},\"agent\":\"build\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}');",
		// idle
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_idle_3', 'ses_fixture', 'idle', 3, " + strconv.FormatInt(updatedMs, 10) + ", " + strconv.FormatInt(updatedMs, 10) + ", '{\"outcome\":\"completed\"}');",
	}
	if _, err := db.Exec("PRAGMA foreign_keys=OFF;\n" + stmts[0] + "\n" + stmts[1] + "\n" + stmts[2] + "\n" + stmts[3] + "\n" + stmts[4]); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// addOpenCodeSession adds session id to a seeded DB: a user turn, a completed assistant turn and an
// idle, all at atMs, so a later import has something new to take.
func addOpenCodeSession(t *testing.T, path, id string, atMs int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := strconv.FormatInt(atMs, 10)
	if _, err := db.Exec("PRAGMA foreign_keys=OFF;\n" +
		"INSERT INTO session_v2 (id, project_id, slug, directory, version, time_created, time_updated) VALUES ('" + id + "', 'prj', 'slug', '/repo', '2.2.7', " + at + ", " + at + ");\n" +
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_" + id + "1', '" + id + "', 'user', 1, " + at + ", " + at + ", '{\"text\":\"again\"}');\n" +
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_" + id + "2', '" + id + "', 'assistant', 2, " + at + ", " + at + ", '{\"time\":{\"completed\":" + at + "},\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}');\n" +
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_" + id + "3', '" + id + "', 'idle', 3, " + at + ", " + at + ", '{\"outcome\":\"completed\"}');"); err != nil {
		t.Fatal(err)
	}
}
