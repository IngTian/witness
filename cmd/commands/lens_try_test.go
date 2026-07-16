package commands

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
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

	err := cmdLensTry(writeLensFile(t, validLensBody), lensTryOpts{nSessions: 1})
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
	err := cmdLensTry(writeLensFile(t, validLensBody), lensTryOpts{nSessions: 1, oneSession: "does-not-exist"})
	if err == nil || !strings.Contains(err.Error(), "no raw turns") {
		t.Fatalf("claude preview should be lock-free and reach --session validation, got: %v", err)
	}
}

// A missing EXTRACT section is a usage error surfaced before any runner work.
func TestLensTryRejectsNoExtractFile(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	err := cmdLensTry(writeLensFile(t, "# name: x\n## REVIEW\nonly review\n"), lensTryOpts{nSessions: 1})
	if err == nil || !strings.Contains(err.Error(), "EXTRACT") {
		t.Fatalf("expected an EXTRACT-required error, got: %v", err)
	}
}

// A missing file surfaces the read error (not a nil-lens panic).
func TestLensTryMissingFile(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	if err := cmdLensTry(filepath.Join(t.TempDir(), "nope.md"), lensTryOpts{nSessions: 1}); err == nil {
		t.Fatalf("expected an error for a missing file")
	}
}

// --session validation: an unknown session id is rejected before mining.
func TestLensTryRejectsUnknownSession(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	err := cmdLensTry(writeLensFile(t, validLensBody), lensTryOpts{nSessions: 1, oneSession: "does-not-exist"})
	if err == nil || !strings.Contains(err.Error(), "no raw turns") {
		t.Fatalf("expected an unknown-session error, got: %v", err)
	}
}

// The REVIEW block flags facets missing key/value (a REVIEW prompt that didn't specify
// the facet output schema) as a PROMPT problem, instead of rendering blank facet lines
// that read like a tool bug. Well-formed facets still render.
func TestLensTryReviewFlagsMalformedFacets(t *testing.T) {
	review := &reviewPreview{
		model:  "test-model",
		obsFed: 3,
		facets: []distill.PreviewFacet{
			{Dimension: "attention", Key: "verifies_claims", Value: "double-checks before trusting", Confidence: 0.7},
			{Dimension: "attention", Key: "", Value: ""},        // malformed: no key/value
			{Dimension: "momentum", Key: "decisive", Value: ""}, // malformed: no value
		},
	}
	out := captureStdout(t, func() { lensTryRenderReviewHuman(review) })
	if !strings.Contains(out, "verifies_claims") {
		t.Fatalf("well-formed facet should render, got:\n%s", out)
	}
	if !strings.Contains(out, "2 facet(s) had no dimension/key/value") {
		t.Fatalf("malformed facets should be flagged as a prompt-schema problem, got:\n%s", out)
	}
}

// The REVIEW block flags prose drift distinctly (with actionable guidance), not as a
// generic "review failed".
func TestLensTryReviewDriftMessage(t *testing.T) {
	out := captureStdout(t, func() {
		lensTryRenderReviewHuman(&reviewPreview{model: "m", obsFed: 2, drifted: true})
	})
	if !strings.Contains(out, "prose drift") || !strings.Contains(out, "--review-model") {
		t.Fatalf("drift should render distinctly with actionable guidance, got:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// --review on a lens file with no REVIEW section fails EARLY (before any runner work),
// rather than silently skipping the half the user asked to preview.
func TestLensTryReviewRequiresReviewSection(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	// A valid EXTRACT but empty REVIEW section.
	body := "# name: cand\n## EXTRACT\nmine growth\n## REVIEW\n"
	err := cmdLensTry(writeLensFile(t, body), lensTryOpts{nSessions: 1, review: true})
	if err == nil || !strings.Contains(err.Error(), "REVIEW section") {
		t.Fatalf("--review with an empty REVIEW section should error early, got: %v", err)
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
