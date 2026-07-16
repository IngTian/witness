package commands

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// writeLensFile writes a candidate lens file and returns its path.
func writeLensFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cand.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// seedTryStore opens a store in a fresh WITNESS_HOME, sets the runner, and appends raw
// for one session so the command reaches (or is stopped before) the mining step. It
// returns the store handle so a test can hold the WorkerLock to simulate a busy worker.
func seedTryStore(t *testing.T, runner string) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetConfigString("runner", runner); err != nil {
		t.Fatalf("SetConfigString runner: %v", err)
	}
	if err := s.AppendRaw(store.RawRecord{Session: "s1", Seq: 0, Role: "user", Text: "hello"}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const validLensBody = "# name: cand\n## EXTRACT\nmine growth\n## REVIEW\nsynth\n"

// On a sweeping (OpenCode) runner, a preview must take the WorkerLock and bail if a
// worker holds it — BEFORE opening the runner (so no `opencode serve` starts and no
// cleanup sweep can fire). We simulate a busy worker by holding the lock ourselves.
func TestLensTryBailsWhenWorkerBusyOnSweepingRunner(t *testing.T) {
	s := seedTryStore(t, store.RunnerOpenCode)
	unlock, ok := s.WorkerLock() // stand in for a running worker
	if !ok {
		t.Fatal("precondition: could not take the worker lock")
	}
	defer unlock()

	err := cmdLensTry(writeLensFile(t, validLensBody), 1, "", "", false, false)
	if err == nil {
		t.Fatalf("expected `lens try` to bail when a worker holds the lock on a sweeping runner")
	}
	if !strings.Contains(err.Error(), "exclusive access") {
		t.Fatalf("error should explain the worker-lock contention, got: %v", err)
	}
}

// On Claude (no shutdown sweep) a preview must NOT take the worker lock — it can safely
// overlap a running worker. With the lock held, the command proceeds past the lock and
// stops at the unknown --session validation (proving it never blocked on the lock).
func TestLensTryOnClaudeIsLockFree(t *testing.T) {
	s := seedTryStore(t, store.RunnerClaude)
	unlock, ok := s.WorkerLock() // a worker is "running"
	if !ok {
		t.Fatal("precondition: could not take the worker lock")
	}
	defer unlock()

	// Use an unknown --session so we hit the pre-mining validation error (no real claude
	// call), which is only reachable if the lock did NOT block the claude path.
	err := cmdLensTry(writeLensFile(t, validLensBody), 1, "does-not-exist", "", false, false)
	if err == nil || !strings.Contains(err.Error(), "no raw turns") {
		t.Fatalf("claude preview should be lock-free and reach --session validation, got: %v", err)
	}
}

// A missing EXTRACT section is a usage error surfaced before any runner work.
func TestLensTryRejectsNoExtractFile(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	err := cmdLensTry(writeLensFile(t, "# name: x\n## REVIEW\nonly review\n"), 1, "", "", false, false)
	if err == nil || !strings.Contains(err.Error(), "EXTRACT") {
		t.Fatalf("expected an EXTRACT-required error, got: %v", err)
	}
}

// A missing file surfaces the read error (not a nil-lens panic).
func TestLensTryMissingFile(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	if err := cmdLensTry(filepath.Join(t.TempDir(), "nope.md"), 1, "", "", false, false); err == nil {
		t.Fatalf("expected an error for a missing file")
	}
}

// --session validation: an unknown session id is rejected before mining.
func TestLensTryRejectsUnknownSession(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	err := cmdLensTry(writeLensFile(t, validLensBody), 1, "does-not-exist", "", false, false)
	if err == nil || !strings.Contains(err.Error(), "no raw turns") {
		t.Fatalf("expected an unknown-session error, got: %v", err)
	}
}

// runPreviews must return results in SAMPLE ORDER regardless of completion order, and
// bound concurrency to `conc`. We use reversed per-index delays (earlier indices sleep
// LONGER) so completion order is the reverse of sample order; results[i] must still map
// to sessions[i]. An atomic peak-counter proves the bound. Run under -race.
func TestRunPreviewsOrderedAndBounded(t *testing.T) {
	sessions := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	const conc = 3

	var inFlight, peak int64
	var mu sync.Mutex
	preview := func(sess string) tryResult {
		n := atomic.AddInt64(&inFlight, 1)
		mu.Lock()
		if n > peak {
			peak = n
		}
		mu.Unlock()
		// Reverse-order delay: "a" (first) sleeps longest, so it finishes LAST.
		idx := strings.IndexByte("abcdefgh", sess[0])
		time.Sleep(time.Duration(len(sessions)-idx) * 2 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		// Echo the session id into the observation so we can verify positional mapping.
		return tryResult{obs: []store.Observation{{Observation: "obs:" + sess}}}
	}

	results := runPreviews(conc, sessions, preview)
	if len(results) != len(sessions) {
		t.Fatalf("want %d results, got %d", len(sessions), len(results))
	}
	for i, sess := range sessions {
		if len(results[i].obs) != 1 || results[i].obs[0].Observation != "obs:"+sess {
			t.Fatalf("results[%d] not paired with sessions[%d]=%q: %+v", i, i, sess, results[i])
		}
	}
	if peak > conc {
		t.Fatalf("concurrency bound violated: peak in-flight %d > conc %d", peak, conc)
	}
}

// A panic in one preview must become that index's error, not crash the process; the
// other sessions still complete.
func TestRunPreviewsPanicIsolated(t *testing.T) {
	sessions := []string{"ok1", "boom", "ok2"}
	preview := func(sess string) tryResult {
		if sess == "boom" {
			panic("simulated preview panic")
		}
		return tryResult{obs: []store.Observation{{Observation: "obs:" + sess}}}
	}
	results := runPreviews(2, sessions, preview)
	if results[1].err == nil || !strings.Contains(results[1].err.Error(), "panic") {
		t.Fatalf("panicking preview should carry an error, got: %+v", results[1])
	}
	for _, i := range []int{0, 2} {
		if results[i].err != nil || len(results[i].obs) != 1 {
			t.Fatalf("non-panicking session %d should succeed, got: %+v", i, results[i])
		}
	}
}
