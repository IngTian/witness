package opencode

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// Regression guard for the review.go leak (issue #21 PR1, now living with the
// runner): the manual review path used to defer only the server's Close() and
// never the self-traffic sweep, leaking witness-distill sessions back into the
// pending queue. The sweep now lives in runner.Close(), so EVERY caller gets it.
// We prove Close() runs the sweep by pointing WITNESS_OPENCODE_DB at a temp DB
// holding a witness-distill session and asserting Close() removes it. A zero-value
// server stands in for an opened one (cmd nil -> Close short-circuits the process
// teardown and proceeds to the sweep).
func TestRunnerCloseSweepsDistillSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE session (id text PRIMARY KEY, title text NOT NULL, agent text, time_created integer NOT NULL);
		INSERT INTO session VALUES ('distill1', 'witness-distill', 'witness-distill', 1000);
		INSERT INTO session VALUES ('userwork', 'real work', 'build', 1000);
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Setenv("WITNESS_OPENCODE_DB", path)

	r := &runner{cfg: store.Config{Runner: "opencode"}, server: &OpenCodeServer{}}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var distillLeft, userLeft int
	_ = db.QueryRow(`SELECT COUNT(*) FROM session WHERE id = 'distill1'`).Scan(&distillLeft)
	_ = db.QueryRow(`SELECT COUNT(*) FROM session WHERE id = 'userwork'`).Scan(&userLeft)
	if distillLeft != 0 {
		t.Fatalf("Close did not sweep the witness-distill session (leak regressed): %d left", distillLeft)
	}
	if userLeft != 1 {
		t.Fatalf("Close wrongly removed real user work: %d left, want 1", userLeft)
	}
}

// Close on a runner that was never Opened (no work this drain) must be a safe no-op.
func TestRunnerCloseUnopened(t *testing.T) {
	r := &runner{cfg: store.Config{Runner: "opencode"}}
	if err := r.Close(); err != nil {
		t.Fatalf("Close on unopened runner: %v", err)
	}
}
