package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// writeLensDir writes a candidate lens DIRECTORY (issue #75: lens.json + extract.md +
// review.md) and returns its path. name/extract/review empty are simply omitted, so a
// test can build a lens missing any piece.
func writeLensDir(t *testing.T, name, extract, review string) string {
	t.Helper()
	dir := t.TempDir()
	if name != "" {
		if err := os.WriteFile(filepath.Join(dir, "lens.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if extract != "" {
		if err := os.WriteFile(filepath.Join(dir, "extract.md"), []byte(extract), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if review != "" {
		if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte(review), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// validLensDir is a candidate lens directory with both prompts — the common fixture.
func validLensDir(t *testing.T) string {
	t.Helper()
	return writeLensDir(t, "cand", "mine growth", "synth")
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

	err := cmdLensTry(validLensDir(t), lensTryOpts{nSessions: 1})
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
	err := cmdLensTry(validLensDir(t), lensTryOpts{nSessions: 1, oneSession: "does-not-exist"})
	if err == nil || !strings.Contains(err.Error(), "no raw turns") {
		t.Fatalf("claude preview should be lock-free and reach --session validation, got: %v", err)
	}
}

// A directory missing extract.md is a usage error surfaced before any runner work.
func TestLensTryRejectsNoExtractFile(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	// review.md only — no extract.md.
	err := cmdLensTry(writeLensDir(t, "x", "", "only review"), lensTryOpts{nSessions: 1})
	if err == nil {
		t.Fatalf("expected an error for a lens dir with no extract.md, got nil")
	}
}

// A missing directory surfaces the read error (not a nil-lens panic).
func TestLensTryMissingFile(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	if err := cmdLensTry(filepath.Join(t.TempDir(), "nope"), lensTryOpts{nSessions: 1}); err == nil {
		t.Fatalf("expected an error for a missing directory")
	}
}

// --session validation: an unknown session id is rejected before mining.
func TestLensTryRejectsUnknownSession(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	err := cmdLensTry(validLensDir(t), lensTryOpts{nSessions: 1, oneSession: "does-not-exist"})
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

// The lens try model label must reflect the RESOLVED per-lens model, not the raw global
// (#75 audit): a registered lens with its own extract_model, previewed with NO --model
// flag, MINES on the per-lens model, so the reported model must be that per-lens one —
// else a prompt-diff run hides the exact variable under test. Drives lensTryEmitJSON.
func TestLensTryReportsResolvedPerLensModel(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	cfg := store.Config{TriageModel: "global-triage"}
	ln := &lens.Lens{Name: "codereview", Extract: "mine", ExtractModel: "per-lens-cheap"}
	out := captureStdout(t, func() {
		if err := lensTryEmitJSON(s, cfg, ln, false, nil, nil, nil); err != nil {
			t.Fatalf("lensTryEmitJSON: %v", err)
		}
	})
	var got lensTryJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, out)
	}
	if got.Model != "per-lens-cheap" {
		t.Fatalf("model label must be the resolved per-lens model, got %q (global was global-triage)", got.Model)
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

// registeredLensPath maps a registered lens NAME to its on-disk DIRECTORY, and leaves a
// path (or an unregistered name) alone — the `lens try codereview` convenience.
func TestRegisteredLensPathResolution(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	// Register a lens named "codereview" from a source directory.
	if err := s.RegisterLens("codereview", writeLensDir(t, "codereview", "mine", "synth")); err != nil {
		t.Fatalf("RegisterLens: %v", err)
	}

	// A bare registered name resolves to its registry directory.
	got, ok := registeredLensPath(s, "codereview")
	if !ok {
		t.Fatalf("a registered name should resolve to its directory")
	}
	if filepath.Base(got) != "codereview" || !strings.Contains(got, "lenses") {
		t.Fatalf("resolved path looks wrong: %q", got)
	}

	// An unregistered name does not resolve (falls through to path handling).
	if _, ok := registeredLensPath(s, "nope"); ok {
		t.Fatalf("an unregistered name must not resolve")
	}
	// A path-like or extensioned arg is always treated as a path, never a name — even
	// if a lens by that stem exists.
	for _, arg := range []string{"./codereview.md", "codereview.md", "dir/codereview", "/abs/codereview"} {
		if _, ok := registeredLensPath(s, arg); ok {
			t.Fatalf("path-like/extensioned arg %q must stay a path, not resolve as a name", arg)
		}
	}
}

// --review on a lens dir with no review.md fails EARLY (before any runner work),
// rather than silently skipping the half the user asked to preview.
func TestLensTryReviewRequiresReviewSection(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	// A valid extract.md but no review.md.
	err := cmdLensTry(writeLensDir(t, "cand", "mine growth", ""), lensTryOpts{nSessions: 1, review: true})
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

// tryRunnerCfg must, for a lens on a DIFFERENT runner than the global, both point cfg at
// the lens's runner AND clear the wrong-runtime global models — else `lens try` on a
// cross-runtime lens hands a global (e.g. claude) model name to the other runtime's Open
// and aborts the preview (the slice-2 audit bug). A same-runtime lens keeps the globals.
func TestTryRunnerCfgClearsCrossRuntimeGlobals(t *testing.T) {
	// global=claude with a claude triage/distill model set; lens routes to opencode.
	cfg := store.Config{Runner: "claude", TriageModel: "claude-sonnet", DistillModel: "claude-opus"}
	cross := &lens.Lens{Name: "cr", Runner: "opencode"}
	got := tryRunnerCfg(cfg, cross)
	if got.Runner != "opencode" {
		t.Fatalf("cross-runtime lens must preview on its own runner, got %q", got.Runner)
	}
	if got.TriageModel != "" || got.DistillModel != "" {
		t.Fatalf("wrong-runtime global models must be cleared, got triage=%q distill=%q", got.TriageModel, got.DistillModel)
	}
	// And ModelFor then falls back to the runtime default (""), not the claude global.
	if m := distill.ModelFor(got, cross, distill.PhaseExtract); m != "" {
		t.Fatalf("cross-runtime lens with no per-lens model must ride the runtime default, got %q", m)
	}

	// A lens on the SAME runner as the global keeps the globals (no clearing).
	same := &lens.Lens{Name: "s"} // no runner → global
	got2 := tryRunnerCfg(cfg, same)
	if got2.Runner != "claude" || got2.TriageModel != "claude-sonnet" {
		t.Fatalf("same-runtime lens must keep the global runner+models, got runner=%q triage=%q", got2.Runner, got2.TriageModel)
	}

	// A cross-runtime lens WITH its own per-lens model keeps that model (globals still cleared,
	// but ModelFor uses the per-lens one).
	tuned := &lens.Lens{Name: "t", Runner: "opencode", ExtractModel: "opencode/free"}
	got3 := tryRunnerCfg(cfg, tuned)
	if m := distill.ModelFor(got3, tuned, distill.PhaseExtract); m != "opencode/free" {
		t.Fatalf("cross-runtime lens with a per-lens model must use it, got %q", m)
	}
}
