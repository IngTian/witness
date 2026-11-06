package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestReadViewInputReadsEverything seeds 2 facets (one archived), 3 notes, 4 raw rows across
// 2 sessions (one file:) and 2 log rows. It asserts counts, the archived facet present,
// HighWater equal to the max seq, and Log equal to the events written — not to
// ReadConfidenceLog, which shares ReadViewInput's query and so would agree with any bug in it.
func TestReadViewInputReadsEverything(t *testing.T) {
	s := tempStore(t)

	// Seed 2 facets: one active, one archived
	facets := []Facet{
		{
			Lens: "default", Dimension: "workstyle", Key: "active_trait",
			LastSeen: "2026-11-05T10:00:00Z",
			Versions: []FacetVersion{
				{Value: "active value", Claim: "active claim", ValidFrom: "2026-11-05T10:00:00Z", Confidence: 0.8},
			},
		},
		{
			Lens: "default", Dimension: "biases", Key: "archived_trait",
			LastSeen:   "2026-11-05T09:00:00Z",
			ArchivedAt: "2026-11-05T11:00:00Z", MergedInto: "other_trait", ArchiveReason: "merged",
			Versions: []FacetVersion{
				{Value: "archived value", Claim: "archived claim", ValidFrom: "2026-11-05T09:00:00Z", Confidence: 0.7},
			},
		},
	}
	if err := s.WriteFacets(facets); err != nil {
		t.Fatalf("WriteFacets: %v", err)
	}

	// Seed 3 observations with known seq values
	_, err := s.db.Exec(`INSERT INTO observations(seq, obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source)
	                      VALUES (1, 'obs1', '2026-11-05T10:00:00Z', 'session1', 'default', 'workstyle', 'note 1', 'evidence 1', 5, 'prompt'),
	                             (2, 'obs2', '2026-11-05T10:01:00Z', 'session1', 'default', 'biases', 'note 2', 'evidence 2', 3, 'prompt'),
	                             (3, 'obs3', '2026-11-05T10:02:00Z', 'file:doc1', 'corpus', 'topic1', 'note 3', 'evidence 3', 4, 'file')`)
	if err != nil {
		t.Fatalf("insert observations: %v", err)
	}

	// Seed 4 raw rows across 2 sessions
	_, err = s.db.Exec(`INSERT INTO raw(session, seq, ts, role, effort, text)
	                    VALUES ('session1', 1, '2026-11-05T10:00:00Z', 'user', 'medium', 'turn 1'),
	                           ('session1', 2, '2026-11-05T10:01:00Z', 'assistant', 'medium', 'turn 2'),
	                           ('file:doc1', 1, '2026-11-05T09:00:00Z', 'user', '', 'file content'),
	                           ('session2', 1, '2026-11-05T11:00:00Z', 'user', 'low', 'turn 3')`)
	if err != nil {
		t.Fatalf("insert raw: %v", err)
	}

	// Seed 2 confidence log entries
	events := []ConfidenceEvent{
		{Lens: "default", Dimension: "workstyle", Key: "active_trait", At: "2026-11-05T10:00:00Z", ClockSeq: 1, From: 0.0, To: 0.8, Kind: EventNew},
		{Lens: "default", Dimension: "biases", Key: "archived_trait", At: "2026-11-05T09:00:00Z", ClockSeq: 1, From: 0.0, To: 0.7, Kind: EventNew},
	}
	if err := s.WriteFacetsLogged(facets, events); err != nil {
		t.Fatalf("WriteFacetsLogged: %v", err)
	}

	// Read everything
	in, err := s.ReadViewInput(context.Background())
	if err != nil {
		t.Fatalf("ReadViewInput: %v", err)
	}

	// Assert Notes: every field, distinct values, ORDER BY seq
	wantNotes := []ViewNote{
		{ID: "obs1", TS: "2026-11-05T10:00:00Z", Session: "session1", Lens: "default", Dimension: "workstyle", Observation: "note 1", Evidence: "evidence 1", Poignancy: 5},
		{ID: "obs2", TS: "2026-11-05T10:01:00Z", Session: "session1", Lens: "default", Dimension: "biases", Observation: "note 2", Evidence: "evidence 2", Poignancy: 3},
		{ID: "obs3", TS: "2026-11-05T10:02:00Z", Session: "file:doc1", Lens: "corpus", Dimension: "topic1", Observation: "note 3", Evidence: "evidence 3", Poignancy: 4},
	}
	if !reflect.DeepEqual(in.Notes, wantNotes) {
		t.Errorf("Notes mismatch:\ngot  %+v\nwant %+v", in.Notes, wantNotes)
	}

	// Assert Turns: every field, ORDER BY id
	wantTurns := []ViewTurn{
		{Session: "session1", TS: "2026-11-05T10:00:00Z"},
		{Session: "session1", TS: "2026-11-05T10:01:00Z"},
		{Session: "file:doc1", TS: "2026-11-05T09:00:00Z"},
		{Session: "session2", TS: "2026-11-05T11:00:00Z"},
	}
	if !reflect.DeepEqual(in.Turns, wantTurns) {
		t.Errorf("Turns mismatch:\ngot  %+v\nwant %+v", in.Turns, wantTurns)
	}

	// Assert facet count and archived facet present
	if got, want := len(in.Facets), 2; got != want {
		t.Errorf("facet count = %d, want %d", got, want)
	}
	foundArchived := false
	for _, f := range in.Facets {
		if f.Key == "archived_trait" && f.ArchivedAt != "" {
			foundArchived = true
			break
		}
	}
	if !foundArchived {
		t.Error("archived facet not found")
	}

	// Assert HighWater equals max seq (3)
	if got, want := in.HighWater, int64(3); got != want {
		t.Errorf("HighWater = %d, want %d", got, want)
	}

	// Assert Log is exactly what was written, in insertion order
	if !reflect.DeepEqual(in.Log, events) {
		t.Errorf("Log mismatch:\ngot  %+v\nwant %+v", in.Log, events)
	}
}

// TestReadViewInputSkipsNothingButKeepsRaw verifies that a raw row with ts=” is returned
// as TS:"" (kept as-is).
func TestReadViewInputSkipsNothingButKeepsRaw(t *testing.T) {
	s := tempStore(t)

	// Seed a raw row with empty ts
	_, err := s.db.Exec(`INSERT INTO raw(session, seq, ts, role, effort, text)
	                     VALUES ('session1', 1, '', 'user', 'medium', 'some text')`)
	if err != nil {
		t.Fatalf("insert raw: %v", err)
	}

	in, err := s.ReadViewInput(context.Background())
	if err != nil {
		t.Fatalf("ReadViewInput: %v", err)
	}

	if len(in.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(in.Turns))
	}

	if in.Turns[0].TS != "" {
		t.Errorf("TS = %q, want empty string", in.Turns[0].TS)
	}
	if in.Turns[0].Session != "session1" {
		t.Errorf("Session = %q, want session1", in.Turns[0].Session)
	}
}

// TestReadViewInputNeverReadsText seeds a raw row with SECRET-TURN-TEXT and a note with
// an embedding. It asserts that fmt.Sprintf("%+v", in) does not contain the secret, and
// that ViewNote has exactly 8 fields (compile-time property, via reflection).
func TestReadViewInputNeverReadsText(t *testing.T) {
	s := tempStore(t)

	// Seed raw row with secret text
	_, err := s.db.Exec(`INSERT INTO raw(session, seq, ts, role, effort, text)
	                     VALUES ('session1', 1, '2026-11-05T10:00:00Z', 'user', 'medium', 'SECRET-TURN-TEXT')`)
	if err != nil {
		t.Fatalf("insert raw: %v", err)
	}

	// Seed observation with embedding
	_, err = s.db.Exec(`INSERT INTO observations(seq, obs_id, ts, session, lens, dimension, observation, evidence, poignancy, source, embedding)
	                    VALUES (1, 'obs1', '2026-11-05T10:00:00Z', 'session1', 'default', 'workstyle', 'note text', 'evidence text', 5, 'prompt', X'deadbeef')`)
	if err != nil {
		t.Fatalf("insert observation: %v", err)
	}

	in, err := s.ReadViewInput(context.Background())
	if err != nil {
		t.Fatalf("ReadViewInput: %v", err)
	}

	// Assert secret not in output
	dump := fmt.Sprintf("%+v", in)
	if strings.Contains(dump, "SECRET-TURN-TEXT") {
		t.Errorf("dump contains SECRET-TURN-TEXT")
	}

	// Assert ViewNote has exactly 8 fields
	noteType := reflect.TypeOf(ViewNote{})
	if noteType.NumField() != 8 {
		t.Errorf("ViewNote has %d fields, want exactly 8", noteType.NumField())
	}

	// List expected field names
	expectedFields := []string{"ID", "TS", "Session", "Lens", "Dimension", "Observation", "Evidence", "Poignancy"}
	for i, expected := range expectedFields {
		if i >= noteType.NumField() {
			break
		}
		field := noteType.Field(i)
		if field.Name != expected {
			t.Errorf("field %d: name = %q, want %q", i, field.Name, expected)
		}
	}
}

// TestWriteFileAtomicMode verifies that the written file is mode 0600 and replaces an
// existing 0644 file's contents. On Windows, skip the mode check.
func TestWriteFileAtomicMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	// Create existing file with 0644
	if err := os.WriteFile(path, []byte("old content"), 0644); err != nil {
		t.Fatalf("create existing file: %v", err)
	}

	// Write new content atomically
	newContent := []byte("new content")
	if err := WriteFileAtomic(path, newContent); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	// Check content was replaced
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(newContent) {
		t.Errorf("content = %q, want %q", got, newContent)
	}

	// Check mode is 0600 (skip on Windows)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		mode := info.Mode().Perm()
		if mode != 0600 {
			t.Errorf("mode = %o, want 0600", mode)
		}
	}
}

// TestReadViewInputRollsBackOnError verifies that the transaction is rolled back when an
// error occurs during ReadViewInput, leaving the store usable. This guards against a leaked
// open tx that would hang every later call (with MaxOpenConns(1)). The probe runs in a
// goroutine with a 2s bound: run inline, a leak blocks it forever and the failure surfaces
// only when `go test` kills the binary 10 minutes later.
func TestReadViewInputRollsBackOnError(t *testing.T) {
	s := tempStore(t)

	// Drop the confidence_log table to force an error during the log read
	if _, err := s.db.Exec(`DROP TABLE confidence_log`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	// ReadViewInput should fail when it tries to read the log
	_, err := s.ReadViewInput(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// The store should still be usable: the one connection must be free again
	done := make(chan error, 1)
	go func() { done <- s.db.QueryRow(`SELECT 1`).Scan(new(int)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("store unusable after ReadViewInput error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("store still blocked 2s after ReadViewInput failed: its transaction leaked")
	}
}
