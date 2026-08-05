package store

import (
	"slices"
	"testing"
	"time"
)

// A ranged drain excludes sessions whose raw.ts julianday() can't read, and that exclusion
// must be COUNTABLE so the worker can say so out loud.
//
// PendingSessionsUpdatedBetween filters on MAX(julianday(ts)); an unreadable ts yields NULL
// and NULL fails both bounds. Keeping the session out of a window it cannot be placed in is
// right, and the unranged drain still mines it — but the skip used to be invisible, so
// `distill start --since ...` reported "nothing pending" with no explanation.
func TestRangedDrainSkipsUnreadableTimestampsAndSaysHowMany(t *testing.T) {
	s := tempStore(t)
	if err := s.EnableLens("default"); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ session, ts string }{
		{"file:good", "2026-03-01T10:00:00Z"},
		{"file:spaced", "2026-03-01 10:00:00"}, // julianday understands this
		{"file:dateonly", "2026-03-01"},        // and this
		{"file:blank", ""},                     // unreadable
		{"file:garbage", "07/26/2026"},         // unreadable (US-style)
	}
	for i, r := range rows {
		if err := s.AppendRaw(RawRecord{TS: r.ts, Session: r.session, Seq: i, Role: "document", Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	// Unranged: EVERY session is pending, including the unreadable-ts ones. This is the
	// property that keeps them from being stranded, so it is asserted first.
	all, err := s.PendingSessions([]string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(rows) {
		t.Fatalf("unranged drain must offer every session; got %d of %d: %v", len(all), len(rows), all)
	}

	// Ranged: only the readable ones.
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ranged, err := s.PendingSessionsUpdatedBetween([]string{"default"}, since, until)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"file:good", "file:spaced", "file:dateonly"} {
		if !slices.Contains(ranged, want) {
			t.Errorf("a session with a readable ts was excluded from the range: %q (got %v)", want, ranged)
		}
	}

	// And the count the worker warns with must match exactly what was dropped.
	skipped := len(all) - len(ranged)
	if got := s.SessionsWithNoUsableTimestamp(); got != skipped {
		t.Errorf("SessionsWithNoUsableTimestamp() = %d, but the range dropped %d sessions — "+
			"the warning would understate what the user is missing", got, skipped)
	}
	if got := s.SessionsWithNoUsableTimestamp(); got != 2 {
		t.Errorf("want the 2 unreadable-ts sessions counted, got %d", got)
	}
}

// A session with even ONE readable ts is not counted: MAX ignores NULLs, so it does fall in
// a window and a warning about it would be a false alarm.
func TestSessionsWithNoUsableTimestampIgnoresPartiallyReadableSessions(t *testing.T) {
	s := tempStore(t)
	if err := s.AppendRaw(RawRecord{TS: "", Session: "file:mixed", Seq: 0, Role: "document", Text: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRaw(RawRecord{TS: "2026-03-01T10:00:00Z", Session: "file:mixed", Seq: 1, Role: "document", Text: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := s.SessionsWithNoUsableTimestamp(); got != 0 {
		t.Errorf("a session with one readable ts must not be counted, got %d", got)
	}
	if err := s.EnableLens("default"); err != nil {
		t.Fatal(err)
	}
	ranged, err := s.PendingSessionsUpdatedBetween([]string{"default"},
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ranged, "file:mixed") {
		t.Errorf("the partially-readable session should be in range: %v", ranged)
	}
}

// An archive with no unreadable timestamps counts zero, so the worker stays quiet.
func TestSessionsWithNoUsableTimestampIsZeroOnAHealthyArchive(t *testing.T) {
	s := tempStore(t)
	if err := s.AppendRaw(RawRecord{TS: "2026-03-01T10:00:00Z", Session: "file:ok", Role: "document", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := s.SessionsWithNoUsableTimestamp(); got != 0 {
		t.Errorf("healthy archive counted %d", got)
	}
}
