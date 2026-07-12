package opencode

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupWitnessDistillSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE session (id text PRIMARY KEY, title text NOT NULL, agent text, time_created integer NOT NULL);
		CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL);
		CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL);
		INSERT INTO session VALUES ('old_distill', 'witness-distill', 'witness-distill', 1000);
		INSERT INTO session VALUES ('new_distill', 'witness-distill', 'witness-distill', 9000);
		INSERT INTO session VALUES ('normal', 'normal work', 'build', 1000);
		INSERT INTO message VALUES ('msg_old', 'old_distill');
		INSERT INTO part VALUES ('prt_old', 'msg_old', 'old_distill');
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	deleted, err := CleanupWitnessDistillSessions(context.Background(), path, time.UnixMilli(5000))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d, want 1", deleted)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sessions, oldMessages, oldParts int
	_ = db.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&sessions)
	_ = db.QueryRow(`SELECT COUNT(*) FROM message WHERE session_id = 'old_distill'`).Scan(&oldMessages)
	_ = db.QueryRow(`SELECT COUNT(*) FROM part WHERE session_id = 'old_distill'`).Scan(&oldParts)
	if sessions != 2 || oldMessages != 0 || oldParts != 0 {
		t.Fatalf("sessions=%d oldMessages=%d oldParts=%d, want normal+new only and no children", sessions, oldMessages, oldParts)
	}
}
