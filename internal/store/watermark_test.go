package store

import (
	"slices"
	"testing"
	"time"
)

func appendN(t *testing.T, s *Store, session string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.AppendRaw(RawRecord{Session: session, Seq: i, Role: "user", Text: "x"}); err != nil {
			t.Fatalf("AppendRaw: %v", err)
		}
	}
}

func TestRetryCounter(t *testing.T) {
	s := tempStore(t)
	if got := s.RetryCount("sess"); got != 0 {
		t.Fatalf("absent: want 0, got %d", got)
	}
	if n := s.IncRetry("sess"); n != 1 {
		t.Fatalf("first inc should return 1, got %d", n)
	}
	if n := s.IncRetry("sess"); n != 2 {
		t.Fatalf("second inc should return 2, got %d", n)
	}
	if got := s.RetryCount("sess"); got != 2 {
		t.Fatalf("after 2 incs: want 2, got %d", got)
	}
	s.ResetRetry("sess")
	if got := s.RetryCount("sess"); got != 0 {
		t.Fatalf("after reset: want 0, got %d", got)
	}
}

func TestPendingSessionsUsesWatermark(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "a", 4)

	// No marker yet → pending.
	if p, _ := s.PendingSessions(); !slices.Contains(p, "a") {
		t.Fatalf("fresh session should be pending, got %v", p)
	}

	// Distilled up to all 4 → not pending.
	s.MarkDistilled("a", 4)
	if p, _ := s.PendingSessions(); slices.Contains(p, "a") {
		t.Fatalf("fully-distilled session should NOT be pending, got %v", p)
	}

	// Resume: 2 new turns appended past the watermark → pending again.
	appendN(t, s, "a", 2) // now 6 records, watermark still 4
	if p, _ := s.PendingSessions(); !slices.Contains(p, "a") {
		t.Fatalf("resumed session with new turns should be pending again, got %v", p)
	}
}

func TestPendingSessionsIncludesStagedObservations(t *testing.T) {
	s := tempStore(t)
	if err := s.StageObservation(Observation{ID: "obs_a", Session: "a", Observation: "noticed a pattern"}); err != nil {
		t.Fatalf("StageObservation: %v", err)
	}
	if p, _ := s.PendingSessions(); !slices.Contains(p, "a") {
		t.Fatalf("session with staged observations should be pending, got %v", p)
	}
}

func TestPendingSessionsUpdatedBetweenUsesLatestRawTimestamp(t *testing.T) {
	s := tempStore(t)
	appendAt := func(session, ts string) {
		t.Helper()
		if err := s.AppendRaw(RawRecord{Session: session, TS: ts, Role: "user", Text: "x"}); err != nil {
			t.Fatalf("AppendRaw: %v", err)
		}
	}
	appendAt("old", "2026-07-01T12:00:00Z")
	appendAt("crosses-since", "2026-07-01T12:00:00Z")
	appendAt("crosses-since", "2026-07-08T12:00:00Z")
	appendAt("in-range", "2026-07-10T12:00:00Z")
	appendAt("new", "2026-07-12T12:00:00Z")
	appendAt("done", "2026-07-10T12:00:00Z")
	if err := s.MarkDistilled("done", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.StageObservation(Observation{ID: "obs_staged", Session: "in-range", Observation: "x"}); err != nil {
		t.Fatal(err)
	}

	since := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 11, 23, 59, 59, 0, time.UTC)
	got, err := s.PendingSessionsUpdatedBetween(since, until)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"crosses-since", "in-range"}
	if !slices.Equal(got, want) {
		t.Fatalf("PendingSessionsUpdatedBetween = %v, want %v", got, want)
	}
}

func TestPendingSessionsUpdatedBetweenExcludesUndatedSession(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "undated", 1)
	if err := s.StageObservation(Observation{ID: "obs_only", Session: "staged-only", Observation: "x"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.PendingSessionsUpdatedBetween(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("dated range should exclude sessions without a raw timestamp, got %v", got)
	}
}

func TestNextBackoffAttempt(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	_ = s.SetNextAttempt("later", now.Add(10*time.Minute))
	_ = s.SetNextAttempt("sooner", now.Add(2*time.Minute))
	_ = s.SetNextAttempt("past", now.Add(-time.Minute))
	at, ok := s.NextBackoffAttempt(now)
	if !ok || !at.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("NextBackoffAttempt = %s, %v; want sooner", at, ok)
	}
}

func tempStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDistilledCountRoundTrip(t *testing.T) {
	s := tempStore(t)
	if got := s.DistilledCount("sess"); got != 0 {
		t.Fatalf("absent marker: want 0, got %d", got)
	}
	if err := s.MarkDistilled("sess", 7); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	if got := s.DistilledCount("sess"); got != 7 {
		t.Fatalf("after mark 7: want 7, got %d", got)
	}
	// Advancing the watermark overwrites, not appends.
	if err := s.MarkDistilled("sess", 12); err != nil {
		t.Fatalf("MarkDistilled advance: %v", err)
	}
	if got := s.DistilledCount("sess"); got != 12 {
		t.Fatalf("after advance: want 12, got %d", got)
	}
}

// TestMarkDistilledIfCurrentGuardsStaleGeneration is the CAS primitive behind the
// #49 C2 fix: the watermark advances only if the raw generation the caller mined
// (its high raw.id) still exists.
func TestMarkDistilledIfCurrentGuardsStaleGeneration(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "sess", 3)
	high := s.MaxRawID("sess")
	if high == 0 {
		t.Fatal("MaxRawID should be non-zero after appending raw")
	}

	// Current generation still present → advances.
	ok, err := s.MarkDistilledIfCurrent("sess", 3, high)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent: %v", err)
	}
	if !ok || s.DistilledCount("sess") != 3 {
		t.Fatalf("current generation should advance: ok=%v count=%d", ok, s.DistilledCount("sess"))
	}

	// Simulate a replace: delete + re-insert raw (new ids), reset progress. The old
	// `high` id no longer exists.
	meta := SessionMeta{Session: "sess"}
	newRecs := []RawRecord{{Session: "sess", Seq: 0, Role: "user", Text: "edited"}}
	if err := s.ApplyRawImport(meta, newRecs, "", "", true); err != nil {
		t.Fatalf("ApplyRawImport(replace): %v", err)
	}
	if s.DistilledCount("sess") != 0 {
		t.Fatalf("replace should have reset progress to 0, got %d", s.DistilledCount("sess"))
	}

	// A stale MarkDistilledIfCurrent keyed on the OLD high id must be refused.
	ok, err = s.MarkDistilledIfCurrent("sess", 3, high)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(stale): %v", err)
	}
	if ok {
		t.Fatal("stale generation should NOT advance the watermark")
	}
	if got := s.DistilledCount("sess"); got != 0 {
		t.Fatalf("watermark must remain 0 after a refused stale write, got %d", got)
	}

	// Keyed on the NEW generation's id → advances again.
	newHigh := s.MaxRawID("sess")
	if newHigh == high {
		t.Fatal("new generation should have a strictly higher max raw.id (AUTOINCREMENT never reuses)")
	}
	ok, err = s.MarkDistilledIfCurrent("sess", 1, newHigh)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(new): %v", err)
	}
	if !ok || s.DistilledCount("sess") != 1 {
		t.Fatalf("new generation should advance: ok=%v count=%d", ok, s.DistilledCount("sess"))
	}
}

// TestMarkDistilledIfCurrentEmptySessionGuard: rawHighID==0 (mine saw an empty
// session) must not clobber a concurrent import that added rows.
func TestMarkDistilledIfCurrentEmptySessionGuard(t *testing.T) {
	s := tempStore(t)

	// Mine saw nothing (rawHighID 0) and no raw exists → the no-op advance is allowed
	// (records the quiet session at count 0).
	ok, err := s.MarkDistilledIfCurrent("sess", 0, 0)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(empty): %v", err)
	}
	if !ok {
		t.Fatal("empty session with no raw should be allowed to record count 0")
	}

	// Now an import added rows AFTER the empty mine read. A stale rawHighID==0 write
	// must be refused so it doesn't mark the newly-imported rows as distilled.
	appendN(t, s, "sess", 2)
	ok, err = s.MarkDistilledIfCurrent("sess", 0, 0)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(empty, raced): %v", err)
	}
	if ok {
		t.Fatal("stale empty-mine write must be refused once raw exists")
	}
}
