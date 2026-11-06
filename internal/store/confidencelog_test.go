package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestWriteFacetsLoggedWritesBothAtomically(t *testing.T) {
	s := tempStore(t)

	facets := []Facet{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "trait1",
			Versions: []FacetVersion{
				{Value: "test value", Confidence: 0.8, ConfidenceSeq: 100},
			},
		},
	}

	events := []ConfidenceEvent{
		{
			Lens:      "lensA",
			Dimension: "dimA",
			Key:       "keyA",
			At:        time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			ClockSeq:  100,
			From:      0.0,
			To:        0.8,
			Kind:      EventNew,
		},
		{
			Lens:      "lensB",
			Dimension: "dimB",
			Key:       "keyB",
			At:        time.Date(2026, 11, 5, 13, 30, 45, 0, time.UTC).Format(time.RFC3339),
			ClockSeq:  250,
			From:      0.7,
			To:        0.92,
			Kind:      EventReinforce,
		},
	}

	if err := s.WriteFacetsLogged(facets, events); err != nil {
		t.Fatalf("WriteFacetsLogged: %v", err)
	}

	// Verify facets were written
	readFacets, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets: %v", err)
	}
	if len(readFacets) != 1 {
		t.Fatalf("facets count = %d, want 1", len(readFacets))
	}

	// Verify events were written and round-trip correctly with all distinct values
	readEvents, err := s.ReadConfidenceLog()
	if err != nil {
		t.Fatalf("ReadConfidenceLog: %v", err)
	}
	if !reflect.DeepEqual(readEvents, events) {
		t.Errorf("events round-trip mismatch:\nwrote: %+v\nread:  %+v", events, readEvents)
	}
}

func TestWriteFacetsLoggedRollsBackTogether(t *testing.T) {
	s := tempStore(t)

	// First write a facet to establish a baseline
	baseline := []Facet{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "existing",
			Versions: []FacetVersion{
				{Value: "existing value", Confidence: 0.5, ConfidenceSeq: 10},
			},
		},
	}
	if err := s.WriteFacets(baseline); err != nil {
		t.Fatalf("baseline WriteFacets: %v", err)
	}

	// Try to write facets with a duplicate lens/dimension/key — violates UNIQUE constraint
	badFacets := []Facet{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "trait1",
			Versions: []FacetVersion{
				{Value: "first", Confidence: 0.8, ConfidenceSeq: 100},
			},
		},
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "trait1", // duplicate!
			Versions: []FacetVersion{
				{Value: "second", Confidence: 0.9, ConfidenceSeq: 101},
			},
		},
	}

	events := []ConfidenceEvent{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "trait1",
			At:        time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			ClockSeq:  100,
			From:      0.0,
			To:        0.8,
			Kind:      EventNew,
		},
	}

	// The write must fail
	if err := s.WriteFacetsLogged(badFacets, events); err == nil {
		t.Fatal("WriteFacetsLogged with duplicate key: expected error, got nil")
	}

	// Verify the baseline facet still exists and nothing else was added
	readFacets, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets after failed write: %v", err)
	}
	if len(readFacets) != 1 {
		t.Fatalf("after rollback: facets count = %d, want 1", len(readFacets))
	}
	if readFacets[0].Key != "existing" {
		t.Errorf("after rollback: facet key = %q, want %q", readFacets[0].Key, "existing")
	}

	// Verify no events were written
	readEvents, err := s.ReadConfidenceLog()
	if err != nil {
		t.Fatalf("ReadConfidenceLog after failed write: %v", err)
	}
	if len(readEvents) != 0 {
		t.Fatalf("after rollback: events count = %d, want 0", len(readEvents))
	}
}

func TestWriteFacetsLoggedRollsBackOnEventInsertFailure(t *testing.T) {
	s := tempStore(t)

	// Establish a baseline
	baseline := []Facet{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "existing",
			Versions: []FacetVersion{
				{Value: "existing value", Confidence: 0.5, ConfidenceSeq: 10},
			},
		},
	}
	if err := s.WriteFacets(baseline); err != nil {
		t.Fatalf("baseline WriteFacets: %v", err)
	}

	// Install a trigger that forces event insert to fail
	if _, err := s.db.Exec(`CREATE TRIGGER reject_log BEFORE INSERT ON confidence_log BEGIN SELECT RAISE(ABORT,'forced'); END`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	defer s.db.Exec(`DROP TRIGGER IF EXISTS reject_log`)

	// Try to write facets with events — the event insert must fail
	newFacets := []Facet{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "new",
			Versions: []FacetVersion{
				{Value: "new value", Confidence: 0.8, ConfidenceSeq: 100},
			},
		},
	}
	events := []ConfidenceEvent{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "new",
			At:        time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			ClockSeq:  100,
			From:      0.0,
			To:        0.8,
			Kind:      EventNew,
		},
	}

	// The write must fail
	err := s.WriteFacetsLogged(newFacets, events)
	if err == nil {
		t.Fatal("WriteFacetsLogged with trigger: expected error, got nil")
	}

	// Verify the baseline facets are unchanged (rollback succeeded)
	readFacets, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets after failed write: %v", err)
	}
	if len(readFacets) != 1 {
		t.Fatalf("after rollback: facets count = %d, want 1", len(readFacets))
	}
	if readFacets[0].Key != "existing" {
		t.Errorf("after rollback: facet key = %q, want %q", readFacets[0].Key, "existing")
	}

	// Verify the store is still usable (not blocked by an open tx)
	type result struct {
		facets []Facet
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		fs, err := s.ReadFacets()
		ch <- result{fs, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("ReadFacets after rollback: %v", res.err)
		}
		if len(res.facets) != 1 {
			t.Errorf("store still usable: facets count = %d, want 1", len(res.facets))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFacets blocked for >2s — tx was left open")
	}
}

func TestWriteFacetsLoggedLeavesFacetsIdentical(t *testing.T) {
	s1 := tempStore(t)
	s2 := tempStore(t)

	facets := []Facet{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "trait1",
			Versions: []FacetVersion{
				{Value: "test value", Confidence: 0.8, ConfidenceSeq: 100,
					ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z"},
			},
		},
		{
			Lens:      "test",
			Dimension: "meta",
			Key:       "trait2",
			// Facet with both a closed version and an open version
			Versions: []FacetVersion{
				{Value: "old version", Confidence: 0.7, ConfidenceSeq: 30,
					ValidFrom: "2026-01-01T00:00:00Z", ValidTo: "2026-06-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z"},
				{Value: "current version", Confidence: 0.9, ConfidenceSeq: 50,
					ValidFrom: "2026-06-01T00:00:00Z", RecordedAt: "2026-06-01T00:00:00Z"},
			},
		},
	}

	events := []ConfidenceEvent{
		{
			Lens:      "test",
			Dimension: "thinking",
			Key:       "trait1",
			At:        time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			ClockSeq:  100,
			From:      0.0,
			To:        0.8,
			Kind:      EventNew,
		},
	}

	// Write with WriteFacets to s1
	if err := s1.WriteFacets(facets); err != nil {
		t.Fatalf("s1.WriteFacets: %v", err)
	}

	// Write with WriteFacetsLogged to s2
	if err := s2.WriteFacetsLogged(facets, events); err != nil {
		t.Fatalf("s2.WriteFacetsLogged: %v", err)
	}

	// Read both and compare with reflect.DeepEqual
	f1, err := s1.ReadFacets()
	if err != nil {
		t.Fatalf("s1.ReadFacets: %v", err)
	}
	f2, err := s2.ReadFacets()
	if err != nil {
		t.Fatalf("s2.ReadFacets: %v", err)
	}

	if !reflect.DeepEqual(f1, f2) {
		t.Errorf("WriteFacets and WriteFacetsLogged produced different facets:\nf1: %+v\nf2: %+v", f1, f2)
	}
}

func TestMigrateV14CreatesConfidenceLog(t *testing.T) {
	s := tempStore(t) // fully migrated to v15
	// Simulate a v14 DB by dropping the table and rolling back the version
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS confidence_log`,
		`DROP INDEX IF EXISTS idx_confidence_log_facet`,
		`PRAGMA user_version=14`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v14 schema (%q): %v", stmt, err)
		}
	}

	// Open (via migrate) brings it to v15
	if err := migrate(s.db); err != nil {
		t.Fatalf("migrate v14->v15: %v", err)
	}

	// Verify user_version=schemaVersion
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// Verify the table exists
	var hasTable int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='confidence_log'`).Scan(&hasTable); err != nil {
		t.Fatal(err)
	}
	if hasTable != 1 {
		t.Errorf("confidence_log table exists = %d, want 1", hasTable)
	}

	// Verify the index exists
	var hasIndex int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_confidence_log_facet'`).Scan(&hasIndex); err != nil {
		t.Fatal(err)
	}
	if hasIndex != 1 {
		t.Errorf("idx_confidence_log_facet index exists = %d, want 1", hasIndex)
	}

	// A second open is a no-op
	if err := migrate(s.db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var v2 int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if v2 != schemaVersion {
		t.Errorf("after second migrate: user_version = %d, want %d", v2, schemaVersion)
	}
}

func TestDeleteLensDataDeletesItsConfidenceLog(t *testing.T) {
	s := tempStore(t)

	// Create facets for two lenses
	facetsA := []Facet{
		{Lens: "lensA", Dimension: "thinking", Key: "trait1",
			Versions: []FacetVersion{{Value: "A value", Confidence: 0.8, ConfidenceSeq: 10}}},
	}
	facetsB := []Facet{
		{Lens: "lensB", Dimension: "thinking", Key: "trait2",
			Versions: []FacetVersion{{Value: "B value", Confidence: 0.7, ConfidenceSeq: 20}}},
	}

	eventsA := []ConfidenceEvent{
		{Lens: "lensA", Dimension: "thinking", Key: "trait1",
			At: time.Now().Format(time.RFC3339), ClockSeq: 10, From: 0.0, To: 0.8, Kind: EventNew},
	}
	eventsB := []ConfidenceEvent{
		{Lens: "lensB", Dimension: "thinking", Key: "trait2",
			At: time.Now().Format(time.RFC3339), ClockSeq: 20, From: 0.0, To: 0.7, Kind: EventNew},
	}

	// Write both
	if err := s.WriteFacetsLogged(append(facetsA, facetsB...), append(eventsA, eventsB...)); err != nil {
		t.Fatalf("WriteFacetsLogged: %v", err)
	}

	// Delete lensA
	obs, facets, err := s.DeleteLensData("lensA")
	if err != nil {
		t.Fatalf("DeleteLensData: %v", err)
	}
	if obs != 0 || facets != 1 {
		t.Errorf("DeleteLensData returned obs=%d facets=%d, want obs=0 facets=1", obs, facets)
	}

	// Verify lensA events are gone, lensB events remain
	events, err := s.ReadConfidenceLog()
	if err != nil {
		t.Fatalf("ReadConfidenceLog: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("after delete: events count = %d, want 1", len(events))
	}
	if events[0].Lens != "lensB" {
		t.Errorf("remaining event lens = %q, want %q", events[0].Lens, "lensB")
	}
}

func TestMergeDoesNotCopyConfidenceLog(t *testing.T) {
	base := newArch(t)
	inc := newArch(t)

	// Base has one confidence_log row
	baseFacets := []Facet{
		{Lens: "base", Dimension: "thinking", Key: "trait1",
			Versions: []FacetVersion{{Value: "base value", Confidence: 0.8, ConfidenceSeq: 10}}},
	}
	baseEvents := []ConfidenceEvent{
		{Lens: "base", Dimension: "thinking", Key: "trait1",
			At: time.Now().Format(time.RFC3339), ClockSeq: 10, From: 0.0, To: 0.8, Kind: EventNew},
	}
	if err := base.st.WriteFacetsLogged(baseFacets, baseEvents); err != nil {
		t.Fatal(err)
	}

	// Incoming has one confidence_log row
	incFacets := []Facet{
		{Lens: "incoming", Dimension: "thinking", Key: "trait2",
			Versions: []FacetVersion{{Value: "inc value", Confidence: 0.9, ConfidenceSeq: 20}}},
	}
	incEvents := []ConfidenceEvent{
		{Lens: "incoming", Dimension: "thinking", Key: "trait2",
			At: time.Now().Format(time.RFC3339), ClockSeq: 20, From: 0.0, To: 0.9, Kind: EventNew},
	}
	if err := inc.st.WriteFacetsLogged(incFacets, incEvents); err != nil {
		t.Fatal(err)
	}

	// Merge the incoming archive into base
	path := inc.export()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if _, err := base.st.Merge(context.Background(), path, true, now); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Base confidence_log should still have ONLY its own row, not the incoming one
	events, err := base.st.ReadConfidenceLog()
	if err != nil {
		t.Fatalf("ReadConfidenceLog after merge: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("after merge: events count = %d, want 1 (merge must not copy confidence_log)", len(events))
	}
	if events[0].Lens != "base" {
		t.Errorf("after merge: event lens = %q, want %q", events[0].Lens, "base")
	}
}
