package store

import "testing"

// SampleSessions orders by total raw text size, largest first, and honors the limit.
// Size-desc is what makes `witness lens try` deterministic across prompt edits and
// surfaces the meatiest (most chunk-prone) sessions.
func TestSampleSessionsSizeDescending(t *testing.T) {
	s := tempStore(t)
	// small: ~4 chars; medium: ~40; large: ~400 — deliberately unambiguous ordering.
	mustRaw(t, s, "small", "tiny")
	mustRaw(t, s, "medium", repeat("m", 40))
	mustRaw(t, s, "large", repeat("L", 400))

	got, err := s.SampleSessions(3)
	if err != nil {
		t.Fatalf("SampleSessions: %v", err)
	}
	want := []string{"large", "medium", "small"}
	if len(got) != len(want) {
		t.Fatalf("want %d sessions, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order wrong at %d: want %q, got %q (full: %v)", i, want[i], got[i], got)
		}
	}

	// Limit is honored, and the limited set is the LARGEST ones.
	top, err := s.SampleSessions(1)
	if err != nil {
		t.Fatalf("SampleSessions(1): %v", err)
	}
	if len(top) != 1 || top[0] != "large" {
		t.Fatalf("SampleSessions(1) should return the single largest session, got %v", top)
	}
}

// Equal-size sessions are ordered deterministically by the `, session` tie-break —
// the property `witness lens try` relies on so v1-vs-v2 prompt runs compare the SAME
// sessions. Without the tie-break, SQLite gives no order guarantee among ties.
func TestSampleSessionsTieBreakDeterministic(t *testing.T) {
	s := tempStore(t)
	// Three sessions with IDENTICAL total size, inserted out of id order.
	mustRaw(t, s, "ccc", "same-size")
	mustRaw(t, s, "aaa", "same-size")
	mustRaw(t, s, "bbb", "same-size")

	got, err := s.SampleSessions(3)
	if err != nil {
		t.Fatalf("SampleSessions: %v", err)
	}
	want := []string{"aaa", "bbb", "ccc"} // size ties → ascending session id
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("tie-break order wrong: want %v, got %v", want, got)
		}
	}
	// Stable across repeated calls (the whole point of determinism).
	again, _ := s.SampleSessions(3)
	for i := range got {
		if got[i] != again[i] {
			t.Fatalf("SampleSessions not stable across calls: %v vs %v", got, again)
		}
	}
}

// SampleRecentSessions orders by most-recent raw turn, newest first (session
// tie-break), and honors the limit — the `--recent` counterpart to size ordering.
func TestSampleRecentSessions(t *testing.T) {
	s := tempStore(t)
	// Distinct MAX(ts) per session, inserted out of recency order.
	mustRawTS(t, s, "old", "2020-01-01T00:00:00Z", "a")
	mustRawTS(t, s, "new", "2030-06-01T00:00:00Z", "b")
	mustRawTS(t, s, "mid", "2025-03-01T00:00:00Z", "c")

	got, err := s.SampleRecentSessions(3)
	if err != nil {
		t.Fatalf("SampleRecentSessions: %v", err)
	}
	want := []string{"new", "mid", "old"} // ts DESC
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("recency order wrong: want %v, got %v", want, got)
		}
	}
	// Limit honored, and it's the MOST recent.
	top, err := s.SampleRecentSessions(1)
	if err != nil {
		t.Fatalf("SampleRecentSessions(1): %v", err)
	}
	if len(top) != 1 || top[0] != "new" {
		t.Fatalf("SampleRecentSessions(1) should return the newest session, got %v", top)
	}
}

// SampleSessions on an empty archive returns an empty slice, not an error.
func TestSampleSessionsEmpty(t *testing.T) {
	s := tempStore(t)
	got, err := s.SampleSessions(5)
	if err != nil {
		t.Fatalf("SampleSessions on empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty archive should return no sessions, got %v", got)
	}
}

// RawChars sums a session's text length; unknown session → 0.
func TestRawChars(t *testing.T) {
	s := tempStore(t)
	mustRaw(t, s, "s1", "hello")  // 5
	mustRaw(t, s, "s1", "world!") // 6 → total 11
	if got := s.RawChars("s1"); got != 11 {
		t.Fatalf("RawChars want 11, got %d", got)
	}
	if got := s.RawChars("nope"); got != 0 {
		t.Fatalf("RawChars(unknown) want 0, got %d", got)
	}
}

func mustRaw(t *testing.T, s *Store, session, text string) {
	t.Helper()
	if err := s.AppendRaw(RawRecord{Session: session, Seq: s.NextSeq(session), Role: "user", Text: text}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
}

func mustRawTS(t *testing.T, s *Store, session, ts, text string) {
	t.Helper()
	if err := s.AppendRaw(RawRecord{Session: session, Seq: s.NextSeq(session), TS: ts, Role: "user", Text: text}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
}

func repeat(ch string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ch[0])
	}
	return string(out)
}
