package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// selfTrafficWhere is agent-authoritative when the column exists, title-fallback
// otherwise. Both cleanup (uses it) and import (negates it) build from this.
func TestSelfTrafficWhere(t *testing.T) {
	clause, args := selfTrafficWhere(true)
	if clause != `agent = ?` || len(args) != 1 || args[0] != MarkerName {
		t.Fatalf("with agent column: clause=%q args=%v", clause, args)
	}
	clause, args = selfTrafficWhere(false)
	if clause != `title = ?` || len(args) != 1 || args[0] != MarkerName {
		t.Fatalf("without agent column: clause=%q args=%v", clause, args)
	}
}

// THE regression test for the dedup asymmetry fix: a witness distill session whose
// TITLE has drifted (OpenCode's auto-titler renamed it) but whose AGENT is still
// the marker must STILL be excluded from import. The old title-only filter let such
// a session through — re-ingesting witness's own lens-prompt + analysis as a user
// session. Requires a schema WITH an agent column (the authoritative path).
func TestImportExcludesAgentMarkedSessionDespiteDriftedTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Schema WITH agent column (modern OpenCode).
	if _, err := db.Exec(`
		CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL, title text NOT NULL, agent text, time_created integer NOT NULL, time_updated integer NOT NULL);
		CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
		-- a witness distill session whose title DRIFTED away from the marker, but agent still marks it
		INSERT INTO session VALUES ('ses_distill', '/tmp', 'Summarize the codebase', 'witness-distill', 1000, 5000);
		INSERT INTO message VALUES ('m1', 'ses_distill', 1100, 1100, '{"role":"user"}');
		INSERT INTO part VALUES ('p1', 'm1', 'ses_distill', 1100, 1100, '{"type":"text","text":"witness lens prompt leaking in"}');
		-- a genuine user session that MUST import
		INSERT INTO session VALUES ('ses_user', '/repo', 'real work', 'build', 1000, 6000);
		INSERT INTO message VALUES ('m2', 'ses_user', 2000, 4000, '{"role":"assistant","time":{"completed":4000}}');
		INSERT INTO part VALUES ('p2', 'm2', 'ses_user', 2100, 2100, '{"type":"text","text":"genuine answer"}');
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := (&Importer{Store: st, DBPath: path}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// The agent-marked (title-drifted) distill session must NOT have been imported.
	if raw, _ := st.ReadRaw(SessionPrefix + "ses_distill"); len(raw) != 0 {
		t.Fatalf("agent-marked distill session leaked into L0 despite drifted title: %d rows", len(raw))
	}
	// The genuine user session must still import.
	if raw, _ := st.ReadRaw(SessionPrefix + "ses_user"); len(raw) == 0 {
		t.Fatal("genuine user session was wrongly excluded")
	}
}
