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
	if got := s.RetryCount("sess", LensDefault); got != 0 {
		t.Fatalf("absent: want 0, got %d", got)
	}
	if n := s.IncRetry("sess", LensDefault); n != 1 {
		t.Fatalf("first inc should return 1, got %d", n)
	}
	if n := s.IncRetry("sess", LensDefault); n != 2 {
		t.Fatalf("second inc should return 2, got %d", n)
	}
	if got := s.RetryCount("sess", LensDefault); got != 2 {
		t.Fatalf("after 2 incs: want 2, got %d", got)
	}
	s.ResetRetry("sess", LensDefault)
	if got := s.RetryCount("sess", LensDefault); got != 0 {
		t.Fatalf("after reset: want 0, got %d", got)
	}
}

func TestPendingSessionsUsesWatermark(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "a", 4)

	// No marker yet → pending.
	if p, _ := s.PendingSessions(nil); !slices.Contains(p, "a") {
		t.Fatalf("fresh session should be pending, got %v", p)
	}

	// Distilled up to all 4 → not pending.
	s.MarkDistilled("a", LensDefault, 4)
	if p, _ := s.PendingSessions(nil); slices.Contains(p, "a") {
		t.Fatalf("fully-distilled session should NOT be pending, got %v", p)
	}

	// Resume: 2 new turns appended past the watermark → pending again.
	appendN(t, s, "a", 2) // now 6 records, watermark still 4
	if p, _ := s.PendingSessions(nil); !slices.Contains(p, "a") {
		t.Fatalf("resumed session with new turns should be pending again, got %v", p)
	}
}

func TestPendingSessionsIncludesStagedObservations(t *testing.T) {
	s := tempStore(t)
	if err := s.StageObservation(Observation{ID: "obs_a", Session: "a", Observation: "noticed a pattern"}); err != nil {
		t.Fatalf("StageObservation: %v", err)
	}
	if p, _ := s.PendingSessions(nil); !slices.Contains(p, "a") {
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
	if err := s.MarkDistilled("done", LensDefault, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.StageObservation(Observation{ID: "obs_staged", Session: "in-range", Observation: "x"}); err != nil {
		t.Fatal(err)
	}

	since := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 11, 23, 59, 59, 0, time.UTC)
	got, err := s.PendingSessionsUpdatedBetween(nil, since, until)
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

	got, err := s.PendingSessionsUpdatedBetween(nil, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("dated range should exclude sessions without a raw timestamp, got %v", got)
	}
}

// The heart of issue #55: with default fully distilled, enabling a NEW lens must
// make the session pending AGAIN for that lens — without a whole-watermark reset,
// and without re-offering the session when only the caught-up lens set is active.
func TestPendingIsPerLens(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "a", 4)
	s.MarkDistilled("a", LensDefault, 4) // default caught up

	// Only default active → not pending (nothing behind).
	if p, _ := s.PendingSessions([]string{LensDefault}); slices.Contains(p, "a") {
		t.Fatalf("with only default (caught up), session must NOT be pending: %v", p)
	}
	// Enable a second lens → the session is pending again (that lens is behind at 0),
	// even though default is fully distilled. THIS is what avoids the full re-mine.
	if p, _ := s.PendingSessions([]string{LensDefault, "codereview"}); !slices.Contains(p, "a") {
		t.Fatalf("a newly-active lens must make the session pending: %v", p)
	}
	// Catch the new lens up too → no longer pending for either.
	s.MarkDistilled("a", "codereview", 4)
	if p, _ := s.PendingSessions([]string{LensDefault, "codereview"}); slices.Contains(p, "a") {
		t.Fatalf("both lenses caught up → not pending: %v", p)
	}
}

// The staged-obs pending branch must be lens-EQUAL, not hardcoded to default: a
// session with staged obs whose default lens is backed off is still offered if ANY
// other active lens is ready (draining staged obs is lens-independent). Guards the
// fix that replaced `p.lens = 'default'` with a CROSS JOIN over the active set.
func TestStagedPendingIsLensEqual(t *testing.T) {
	s := tempStore(t)
	if err := s.StageObservation(Observation{ID: "obs_a", Session: "a", Observation: "x"}); err != nil {
		t.Fatalf("StageObservation: %v", err)
	}
	// default is backed off far out; codereview has no progress row (ready).
	_ = s.SetNextAttempt("a", LensDefault, time.Now().Add(time.Hour))

	// With default + codereview active, the staged session is still offered (via the
	// healthy codereview lens) even though default is parked.
	if p, _ := s.PendingSessions([]string{LensDefault, "codereview"}); !slices.Contains(p, "a") {
		t.Fatalf("staged session must be offered while a non-default lens is ready: %v", p)
	}
	// If default is the ONLY active lens and it's backed off, the staged session is
	// correctly parked (nothing can run this pass).
	if p, _ := s.PendingSessions([]string{LensDefault}); slices.Contains(p, "a") {
		t.Fatalf("staged session must be parked when the only active lens is backed off: %v", p)
	}
}

// A backoff on ONE lens must not park the session for a healthy sibling lens: the
// pair-scoped next_attempt only hides that (session,lens) pair.
func TestPerLensBackoffIsolation(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "a", 2)
	// codereview is backed off far into the future; default is untouched (behind at 0).
	_ = s.SetNextAttempt("a", "codereview", time.Now().Add(time.Hour))

	// default is still pending (not gated by codereview's backoff).
	if p, _ := s.PendingSessions([]string{LensDefault, "codereview"}); !slices.Contains(p, "a") {
		t.Fatalf("healthy default lens must keep the session pending despite codereview backoff: %v", p)
	}
	// If ONLY codereview were active, the session would be parked by its backoff.
	if p, _ := s.PendingSessions([]string{"codereview"}); slices.Contains(p, "a") {
		t.Fatalf("a lens under backoff must be parked when it's the only active lens: %v", p)
	}
	// default catches up; now only the backed-off codereview remains behind → parked.
	s.MarkDistilled("a", LensDefault, 2)
	if p, _ := s.PendingSessions([]string{LensDefault, "codereview"}); slices.Contains(p, "a") {
		t.Fatalf("default caught up + codereview backed off → session parked: %v", p)
	}
}

// A backoff stranded on a now-inactive lens must not count as outstanding work
// (Stats.BackedOff) or schedule a wakeup (NextBackoffAttempt) — else `distill start
// --all` falsely reports "incomplete" when every ACTIVE lens is caught up (#55).
func TestBackoffIgnoresInactiveLens(t *testing.T) {
	s := tempStore(t)
	appendN(t, s, "a", 2)
	s.MarkDistilled("a", LensDefault, 2)                               // default caught up
	_ = s.SetNextAttempt("a", "codereview", time.Now().Add(time.Hour)) // codereview backed off

	// codereview inactive → its backoff is inert.
	if st := s.Stats([]string{LensDefault}); st.BackedOff != 0 {
		t.Fatalf("a disabled lens's backoff must not count as BackedOff, got %d", st.BackedOff)
	}
	if _, ok := s.NextBackoffAttempt([]string{LensDefault}, time.Now()); ok {
		t.Fatalf("a disabled lens's backoff must not schedule a wakeup")
	}
	// codereview active → its backoff counts again.
	if st := s.Stats([]string{LensDefault, "codereview"}); st.BackedOff != 1 {
		t.Fatalf("an active backed-off lens should count, got %d", st.BackedOff)
	}
	if _, ok := s.NextBackoffAttempt([]string{LensDefault, "codereview"}, time.Now()); !ok {
		t.Fatalf("an active backed-off lens should schedule a wakeup")
	}
}

func TestNextBackoffAttempt(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	_ = s.SetNextAttempt("later", LensDefault, now.Add(10*time.Minute))
	_ = s.SetNextAttempt("sooner", LensDefault, now.Add(2*time.Minute))
	_ = s.SetNextAttempt("past", LensDefault, now.Add(-time.Minute))
	at, ok := s.NextBackoffAttempt(nil, now)
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
	if got := s.DistilledCount("sess", LensDefault); got != 0 {
		t.Fatalf("absent marker: want 0, got %d", got)
	}
	if err := s.MarkDistilled("sess", LensDefault, 7); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 7 {
		t.Fatalf("after mark 7: want 7, got %d", got)
	}
	// Advancing the watermark overwrites, not appends.
	if err := s.MarkDistilled("sess", LensDefault, 12); err != nil {
		t.Fatalf("MarkDistilled advance: %v", err)
	}
	if got := s.DistilledCount("sess", LensDefault); got != 12 {
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
	ok, err := s.MarkDistilledIfCurrent("sess", LensDefault, 3, high)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent: %v", err)
	}
	if !ok || s.DistilledCount("sess", LensDefault) != 3 {
		t.Fatalf("current generation should advance: ok=%v count=%d", ok, s.DistilledCount("sess", LensDefault))
	}

	// Simulate a replace: delete + re-insert raw (new ids), reset progress. The old
	// `high` id no longer exists.
	meta := SessionMeta{Session: "sess"}
	newRecs := []RawRecord{{Session: "sess", Seq: 0, Role: "user", Text: "edited"}}
	if err := s.ApplyRawImport(meta, newRecs, "", "", true); err != nil {
		t.Fatalf("ApplyRawImport(replace): %v", err)
	}
	if s.DistilledCount("sess", LensDefault) != 0 {
		t.Fatalf("replace should have reset progress to 0, got %d", s.DistilledCount("sess", LensDefault))
	}

	// A stale MarkDistilledIfCurrent keyed on the OLD high id must be refused.
	ok, err = s.MarkDistilledIfCurrent("sess", LensDefault, 3, high)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(stale): %v", err)
	}
	if ok {
		t.Fatal("stale generation should NOT advance the watermark")
	}
	if got := s.DistilledCount("sess", LensDefault); got != 0 {
		t.Fatalf("watermark must remain 0 after a refused stale write, got %d", got)
	}

	// Keyed on the NEW generation's id → advances again.
	newHigh := s.MaxRawID("sess")
	if newHigh == high {
		t.Fatal("new generation should have a strictly higher max raw.id (AUTOINCREMENT never reuses)")
	}
	ok, err = s.MarkDistilledIfCurrent("sess", LensDefault, 1, newHigh)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(new): %v", err)
	}
	if !ok || s.DistilledCount("sess", LensDefault) != 1 {
		t.Fatalf("new generation should advance: ok=%v count=%d", ok, s.DistilledCount("sess", LensDefault))
	}
}

// TestMarkDistilledIfCurrentEmptySessionGuard: rawHighID==0 (mine saw an empty
// session) must not clobber a concurrent import that added rows.
func TestMarkDistilledIfCurrentEmptySessionGuard(t *testing.T) {
	s := tempStore(t)

	// Mine saw nothing (rawHighID 0) and no raw exists → the no-op advance is allowed
	// (records the quiet session at count 0).
	ok, err := s.MarkDistilledIfCurrent("sess", LensDefault, 0, 0)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(empty): %v", err)
	}
	if !ok {
		t.Fatal("empty session with no raw should be allowed to record count 0")
	}

	// Now an import added rows AFTER the empty mine read. A stale rawHighID==0 write
	// must be refused so it doesn't mark the newly-imported rows as distilled.
	appendN(t, s, "sess", 2)
	ok, err = s.MarkDistilledIfCurrent("sess", LensDefault, 0, 0)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(empty, raced): %v", err)
	}
	if ok {
		t.Fatal("stale empty-mine write must be refused once raw exists")
	}
}
