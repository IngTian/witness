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
