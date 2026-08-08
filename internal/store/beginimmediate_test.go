package store

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

// beginImmediate must acquire the write lock WITHOUT carrying state into the transaction.
//
// It originally did `PRAGMA user_version = <value read beforehand>`, and the read happened
// OUTSIDE the transaction. When another process migrated the schema in that window, the
// stale value was written back INSIDE — silently REVERTING the recorded schema version
// while the new schema sat on disk (reproduced by hand: 8 -> 3), after which a later Open
// re-runs migrate() over an already-migrated database.
//
// The interleaving itself cannot be forced from a test: it needs another process to commit
// in the microseconds between beginImmediate's own read and its own write, and both
// connections in-test see the same file, so a behavioral test passes on the buggy code too
// (verified — which is why this asserts the PROPERTY instead of the race). The property is
// the real fix: the lock-acquiring write reads nothing, so there is no stale value to
// write back.
func TestBeginImmediateCarriesNoStateIntoTheTransaction(t *testing.T) {
	src, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatal(err)
	}
	// LF-normalize before scanning: a CRLF checkout (git's Windows default) makes the "\n}\n"
	// delimiter below miss, and this test would then t.Fatal on every Windows run instead of
	// asserting the property. .gitattributes pins *.go to LF as well.
	body := strings.ReplaceAll(strings.ReplaceAll(string(src), "\r\n", "\n"), "\r", "\n")
	start := strings.Index(body, "func beginImmediate(")
	if start < 0 {
		t.Fatal("beginImmediate not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit beginImmediate")
	}
	fn := body[start : start+end]

	// It must not read user_version (nor anything else) to feed its own write.
	if strings.Contains(fn, "PRAGMA user_version") {
		t.Error("beginImmediate must not touch user_version: reading it outside the tx and " +
			"writing it back inside silently reverts a concurrent migration")
	}
	if strings.Contains(fn, "QueryRow") || strings.Contains(fn, "Scan(") {
		t.Error("beginImmediate must not READ anything before its lock-acquiring write — " +
			"any value so captured is stale by the time it is written")
	}
}

// The behavioral half: beginImmediate leaves user_version untouched, leaks no scratch
// object into the schema, and works on a database that has no witness tables yet (migrate
// calls it BEFORE applying schemaV1, so the write cannot target a real table).
func TestBeginImmediateIsSchemaNeutral(t *testing.T) {
	s := tempStore(t)
	const bumped = 99
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	tx, err := beginImmediate(s.db)
	if err != nil {
		t.Fatalf("beginImmediate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != bumped {
		t.Fatalf("user_version changed: got %d, want %d", after, bumped)
	}
	var probes int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'witness_write_lock_probe%'`).Scan(&probes)
	if probes != 0 {
		t.Fatalf("the scratch probe table leaked into the schema (%d objects)", probes)
	}

	// A rolled-back transaction must also leave nothing behind.
	tx2, err := beginImmediate(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'witness_write_lock_probe%'`).Scan(&probes)
	if probes != 0 {
		t.Fatalf("rollback left the scratch table behind (%d objects)", probes)
	}

	// Empty database (no witness tables): migrate() calls beginImmediate before schemaV1.
	fresh, err := sql.Open("sqlite", t.TempDir()+"/fresh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	fresh.SetMaxOpenConns(1)
	ftx, err := beginImmediate(fresh)
	if err != nil {
		t.Fatalf("beginImmediate on an empty database: %v", err)
	}
	if err := ftx.Rollback(); err != nil {
		t.Fatal(err)
	}
}
