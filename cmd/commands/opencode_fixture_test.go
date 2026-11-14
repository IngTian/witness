package commands

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform/opencode"
)

// execOpenCodeDB runs stmts in one Exec on the DB at path (creating it) with foreign keys off, so a
// fixture needs no project row.
func execOpenCodeDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA foreign_keys=OFF;\n" + strings.Join(stmts, "\n")); err != nil {
		t.Fatal(err)
	}
}

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
	at := strconv.FormatInt(updatedMs, 10)
	execOpenCodeDB(t, path,
		string(schema),
		"INSERT INTO session_v2 (id, project_id, slug, directory, version, time_created, time_updated) VALUES ('ses_fixture', 'prj', 'slug', '/repo', '2.2.7', 1000, "+at+");",
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_user_1', 'ses_fixture', 'user', 1, 2000, 2000, '{\"time\":{\"created\":2000},\"text\":\"hello\"}');",
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_asst_2', 'ses_fixture', 'assistant', 2, 3000, 3000, '{\"time\":{\"created\":3000,\"completed\":3100},\"agent\":\"build\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}');",
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_idle_3', 'ses_fixture', 'idle', 3, "+at+", "+at+", '{\"outcome\":\"completed\"}');",
	)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// addOpenCodeSession adds session id to a seeded DB: a user turn, a completed assistant turn and an
// idle, all at atMs, so a later import has something new to take.
func addOpenCodeSession(t *testing.T, path, id string, atMs int64) {
	t.Helper()
	at := strconv.FormatInt(atMs, 10)
	execOpenCodeDB(t, path,
		"INSERT INTO session_v2 (id, project_id, slug, directory, version, time_created, time_updated) VALUES ('"+id+"', 'prj', 'slug', '/repo', '2.2.7', "+at+", "+at+");",
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_"+id+"1', '"+id+"', 'user', 1, "+at+", "+at+", '{\"text\":\"again\"}');",
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_"+id+"2', '"+id+"', 'assistant', 2, "+at+", "+at+", '{\"time\":{\"completed\":"+at+"},\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}');",
		"INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_"+id+"3', '"+id+"', 'idle', 3, "+at+", "+at+", '{\"outcome\":\"completed\"}');",
	)
}

// v1OnlyOpenCodeDB is a real DB as OpenCode v1 left it (its session table, no session_v2): a fake
// header would fail as "file is not a database" before ProbeSchema ever ran.
func v1OnlyOpenCodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	execOpenCodeDB(t, path, `CREATE TABLE session (id TEXT PRIMARY KEY)`)
	return path
}

// plantOpenCodePlugin writes body as the installed plugin, which is what makes OpenCode wired for
// ImportAtDrainStart and health.
func plantOpenCodePlugin(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode.PluginPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubOpenCodeVersion makes the version probe answer v and err for one test, so none spawns the
// user's real `opencode`.
func stubOpenCodeVersion(t *testing.T, v opencode.Version, err error) {
	t.Helper()
	prev := openCodeVersion
	openCodeVersion = func(context.Context) (opencode.Version, error) { return v, err }
	t.Cleanup(func() { openCodeVersion = prev })
}
