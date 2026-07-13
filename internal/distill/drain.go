package distill

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"sync"

	"github.com/IngTian/witness/internal/store"
)

// maxMineConcurrency is an absolute upper bound on parallel miners, independent of
// core count. Each concurrent miner holds a ~0.35GB `claude -p` process, so MEMORY
// (not CPUs) is the binding constraint — on a 64-core box an un-clamped
// mine_concurrency would spawn dozens of processes and OOM. 16 keeps the worst case
// (16×0.35 + 1.5GB embedder ≈ 7GB) safe on a typical laptop while leaving ample
// headroom above the default of 4.
const maxMineConcurrency = 16

// EffectiveConcurrency is the number of sessions the engine will mine in parallel:
// the configured cap, clamped to the CPU count, to an absolute memory-safe ceiling,
// and to 1 when the runner cannot run concurrently. This is the POLICY
// (engine-owned) applied to the platform FACT (runner.ConcurrentRunSafe) — the
// platform never picks a number (issue #22).
func EffectiveConcurrency(want int, concurrentRunSafe bool) int {
	if !concurrentRunSafe {
		return 1
	}
	if want < 1 {
		want = 1
	}
	if want > maxMineConcurrency {
		want = maxMineConcurrency
	}
	if n := runtime.GOMAXPROCS(0); want > n {
		want = n
	}
	return want
}

// DrainOpts configures a drain. pending reports the currently-distillable sessions
// (re-consulted so mid-drain arrivals are picked up); stop reports a graceful-stop
// request (checked before dispatching each new session); max caps the number of
// sessions committed (<=0 = unbounded); onCommit is called just before each
// session's results are committed (e.g. to stamp worker_current).
//
// Attempted, if non-nil, is the attempt-once set to use instead of a fresh internal
// one. The caller passes the SAME map across several Drain calls in one worker run
// (the re-check loop that keeps working when capture lands new L0 mid-drain). The
// key is (session, watermark) — NOT session alone — so:
//   - a STUCK session (commit/read error that never advances the watermark) keeps
//     the same key every pass → attempted once → can't spin the loop or re-mine
//     forever;
//   - a REGROWN session (distilled, then resumed and gained new turns → its
//     watermark moved) gets a NEW key → is re-attempted, so mid-run arrivals for an
//     already-drained session are not silently filtered out (issue #22 review #1).
//
// When nil, Drain makes its own — a single Drain is still attempt-once by itself.
type DrainOpts struct {
	Conc      int
	Pending   func() []string
	Stop      func() bool
	Max       int
	OnCommit  func(session string)
	Attempted map[string]bool
}

// attemptKey identifies a (session, raw-turn-count) attempt — the question it
// answers is "has NEW raw work arrived for this session since I last attempted it
// this run?". Keying on RawCount (not the distilled watermark) is what makes the
// re-check loop both safe and complete:
//   - a STUCK session (mine/commit fails → RawCount unchanged) keeps the same key
//     → attempted once → no spin/re-mine, the loop terminates;
//   - a REGROWN session (resumed, gained turns → RawCount increased) gets a NEW key
//     → re-attempted, so mid-run arrivals for an already-drained session are caught
//     (issue #22 review #1). A successful distill is NOT itself a reason to
//     re-attempt — the real pending() query drops a fully-distilled session — so
//     the advancing distilled-count must NOT be in the key.
func (w *Worker) attemptKey(session string) string {
	return session + "\x00" + strconv.Itoa(w.Store.RawCount(session))
}

// HasUnattempted reports whether any of the given sessions has NOT yet been
// attempted (by its current (session,RawCount) key) in the run tracked by
// attempted. The worker's outer drain→review→re-check loop uses this to decide
// whether NEW work arrived while it held the lock — a still-pending stuck/backed-off
// session (same key, already attempted) returns false so the loop can't spin, while
// a fresh or regrown session returns true. Keeps the attempt-key format private to
// the engine (issue #22 review #1).
func (w *Worker) HasUnattempted(sessions []string, attempted map[string]bool) bool {
	for _, s := range sessions {
		if !attempted[w.attemptKey(s)] {
			return true
		}
	}
	return false
}

// Drain is the engine's session-drain loop. It preserves drainQueueLimit's contract
// exactly — every pending job attempted at most once per drain, jobs arriving
// mid-drain picked up on the next scan, terminates even if a job stays pending,
// optional budget cap — but splits each session into a parallel MAP (MineSession,
// the expensive LLM calls, up to Conc at once) and a serial REDUCE (CommitMining,
// the sole L1 writer). Invariants that make the parallelism safe:
//
//   - The store is written by exactly ONE goroutine (this one, in commit), so
//     MaxOpenConns(1) / single-writer semantics are untouched.
//   - `existing` is a single running corpus snapshot threaded through every commit
//     and appended after each write, so a later session dedups against an earlier
//     one's just-written observations — no cross-session dedup gap.
//   - Commits happen in submission order (FIFO over in-flight jobs), so the result
//     is deterministic w.r.t. the pending order.
//
// Returns the number of sessions ATTEMPTED-and-reaped this call (including
// map-failures, backoffs, quiet sessions, and no-op advances — every reaped job
// increments it), NOT strictly the number committed to L1. The worker's re-check
// loop uses "0 attempted → queue is drained → stop"; do not treat this as a commit
// count (e.g. for a budget) without re-deriving it.
func (w *Worker) Drain(ctx context.Context, opts DrainOpts) int {
	conc := opts.Conc
	if conc < 1 {
		conc = 1
	}
	existing, _ := w.Store.ReadObservations("") // ONE snapshot per drain (hoisted out of the per-session loop)

	stop := func() bool { return opts.Stop != nil && opts.Stop() }
	reached := func(claimed int) bool { return opts.Max > 0 && claimed >= opts.Max }

	attempted := opts.Attempted
	if attempted == nil {
		attempted = map[string]bool{}
	}
	processed := 0

	for {
		// Build the next batch of not-yet-attempted pending sessions. Re-scanning
		// here (not per-session) is what picks up mid-drain arrivals; the
		// (session,watermark)-keyed `attempted` set guarantees a stuck session is
		// tried once (so the loop always terminates) while a regrown session re-enters.
		var batch []string
		for _, s := range opts.Pending() {
			if !attempted[w.attemptKey(s)] {
				batch = append(batch, s)
			}
		}
		if len(batch) == 0 {
			return processed
		}

		if conc == 1 {
			// Serial fast path: no goroutines/channels for the common laptop case
			// (single pending session, or a runner that isn't ConcurrentRunSafe).
			for _, s := range batch {
				if stop() || reached(processed) {
					return processed
				}
				attempted[w.attemptKey(s)] = true
				m, err := w.mineSessionSafe(ctx, s)
				w.commitResult(m, err, &existing, opts.OnCommit)
				processed++
			}
			continue
		}

		// Parallel path: a TRUE rolling window. Up to `conc` miners run at once; a
		// miner slot frees the moment ANY map finishes (not just the oldest), so one
		// slow session no longer idles the other slots or stalls new dispatch (issue
		// #22 review #3). Commits still happen in submission order: each result
		// carries its submission index, out-of-order arrivals are buffered, and the
		// reducer commits the contiguous prefix. Only the single Drain goroutine
		// commits, so the store stays single-writer and `existing` race-free.
		processed += w.drainWindow(ctx, batch, conc, &existing, attempted, stop, reached, processed, opts.OnCommit)

		if stop() || reached(processed) {
			return processed
		}
		// else loop and re-scan pending for arrivals.
	}
}

// drainWindow runs one batch through the rolling-window MAP + ordered REDUCE and
// returns how many sessions it committed. See Drain for the invariants.
func (w *Worker) drainWindow(ctx context.Context, batch []string, conc int, existing *[]store.Observation, attempted map[string]bool, stop func() bool, reached func(int) bool, processedBase int, onCommit func(string)) int {
	type minedResult struct {
		idx int // submission index, for ordered commit
		m   *SessionMining
		err error
	}
	done := make(chan minedResult, conc) // any miner signals completion here
	sem := make(chan struct{}, conc)     // caps concurrent miners
	var wg sync.WaitGroup

	pending := make(map[int]minedResult) // out-of-order results awaiting their turn
	nextToCommit := 0                    // submission index of the next commit
	committed := 0

	// commitReady flushes every buffered result that is now contiguous from
	// nextToCommit, in submission order — so commits are deterministic even though
	// maps finish out of order.
	commitReady := func() {
		for {
			r, ok := pending[nextToCommit]
			if !ok {
				return
			}
			delete(pending, nextToCommit)
			nextToCommit++
			w.commitResult(r.m, r.err, existing, onCommit)
			committed++
		}
	}

	dispatched := 0
	for _, s := range batch {
		// Budget/stop: don't launch more than allowed committed-or-in-flight.
		if stop() || reached(processedBase+dispatched) {
			break
		}
		attempted[w.attemptKey(s)] = true
		sem <- struct{}{} // blocks until a slot frees — THIS is the rolling window
		// Drain any completions that arrived while we waited for a slot, freeing
		// their buffer + advancing commits promptly.
		for {
			select {
			case r := <-done:
				pending[r.idx] = r
				commitReady()
				continue
			default:
			}
			break
		}
		idx := dispatched
		dispatched++
		wg.Add(1)
		go func(idx int, session string) {
			defer wg.Done()
			defer func() { <-sem }()
			m, err := w.mineSessionSafe(ctx, session)
			done <- minedResult{idx: idx, m: m, err: err}
		}(idx, s)
	}

	// Drain remaining completions until every dispatched job has committed —
	// finishing in-flight work even on stop/budget (its mining is already paid for).
	for committed < dispatched {
		r := <-done
		pending[r.idx] = r
		commitReady()
	}
	wg.Wait()
	return committed
}

// mineSessionSafe wraps MineSession with a recover barrier so a panic in one
// session's mining (e.g. the embedder's context.MustExecOnceN panics on a
// pathological input) does NOT crash the whole worker and orphan the other
// in-flight `claude -p` children. A recovered panic is converted into a normal
// mining error: commitResult logs it and leaves the session pending (its delta is
// never advanced, so it retries), exactly like a read-side failure. Returns a
// SessionMining carrying the session id so the error is attributable. This upholds
// the "distillation must never take down the process" invariant now that mining
// runs across goroutines.
func (w *Worker) mineSessionSafe(ctx context.Context, session string) (m *SessionMining, err error) {
	defer func() {
		if r := recover(); r != nil {
			m = &SessionMining{Session: session}
			err = fmt.Errorf("panic mining session %s: %v", session, r)
		}
	}()
	return w.MineSession(ctx, session)
}

// commitResult applies one mining result: a map-phase read error is logged and
// skipped (nothing written, watermark untouched, so it retries next drain),
// otherwise CommitMining runs (which itself handles a mine transport failure as a
// backoff without writing).
func (w *Worker) commitResult(m *SessionMining, mineErr error, existing *[]store.Observation, onCommit func(session string)) {
	if mineErr != nil {
		if m != nil {
			slog.Error("mine session", "session", m.Session, "err", mineErr)
		} else {
			slog.Error("mine session", "err", mineErr)
		}
		return
	}
	if onCommit != nil {
		onCommit(m.Session)
	}
	if err := w.CommitMining(m, existing); err != nil {
		slog.Error("commit session", "session", m.Session, "err", err)
	}
}
