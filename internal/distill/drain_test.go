package distill

import (
	"context"
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// safeMiner is a concurrency-safe fake miner for the parallel-drain tests: it
// records how many times each transcript was mined (guarded), so a race detector
// run catches any accidental shared-state write in the MAP phase.
type safeMiner struct {
	mu    sync.Mutex
	calls int
}

func (m *safeMiner) run(_ context.Context, _, _, input string) (string, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	// Marshal so newlines/quotes in the transcript are escaped (a raw string concat
	// would emit invalid JSON and parse to a "quiet session" — a test-only trap).
	arr := []minedObs{{Dimension: "thinking", Observation: "obs-for:" + input, Evidence: "e", Poignancy: 3}}
	b, _ := json.Marshal(arr)
	return string(b), nil
}

func drainWorker(s *store.Store, run MineFunc) *Worker {
	return &Worker{
		Store:    s,
		Embedder: fakeEmbedder{},
		Lenses:   []*lens.Lens{{Name: "default", Global: true, Extract: "mine", Dimensions: []string{"thinking"}}},
		Config:   store.Config{},
		Run:      run,
	}
}

func TestEffectiveConcurrency(t *testing.T) {
	// Not safe → always 1, whatever the ask.
	if got := EffectiveConcurrency(8, false); got != 1 {
		t.Fatalf("unsafe runner must clamp to 1, got %d", got)
	}
	// Safe, want<1 → floor at 1.
	if got := EffectiveConcurrency(0, true); got != 1 {
		t.Fatalf("want<1 must floor to 1, got %d", got)
	}
	// Safe, small want → honored (GOMAXPROCS on CI is >= 2).
	if got := EffectiveConcurrency(2, true); got != 2 {
		t.Fatalf("safe runner should honor want=2 (GOMAXPROCS>=2), got %d", got)
	}
	// A huge want must be clamped to min(maxMineConcurrency, GOMAXPROCS) — never
	// spawn hundreds of `claude -p` (memory, not cores, is the binding constraint).
	ceiling := maxMineConcurrency
	if n := runtime.GOMAXPROCS(0); n < ceiling {
		ceiling = n
	}
	if got := EffectiveConcurrency(1000, true); got != ceiling {
		t.Fatalf("want=1000 must clamp to min(maxMineConcurrency=%d, GOMAXPROCS)=%d, got %d", maxMineConcurrency, ceiling, got)
	}
}

// A panic inside MineSession (e.g. the embedder panicking on a pathological input)
// must NOT crash the worker: mineSessionSafe converts it into a mining error so the
// session is logged and left pending, and sibling goroutines finish cleanly.
func TestMineSessionSafeRecoversPanic(t *testing.T) {
	s := newStore(t)
	w := drainWorker(s, func(context.Context, string, string, string) (string, error) {
		panic("boom in the model exec")
	})
	capture(t, s, "boom", "user", "turn")
	m, err := w.mineSessionSafe(context.Background(), "boom")
	if err == nil {
		t.Fatal("a panic in mining must surface as an error, not crash")
	}
	if m == nil || m.Session != "boom" {
		t.Fatalf("recovered result must carry the session id for attribution, got %+v", m)
	}
	// And the whole Drain must survive a panicking session, committing nothing for it.
	pending := func() []string { return []string{"boom"} }
	n := w.Drain(context.Background(), DrainOpts{Conc: 2, Pending: pending})
	if n != 1 {
		t.Fatalf("panicking session should be attempted-and-reaped once, got n=%d", n)
	}
	if obs, _ := s.ReadObservations(""); len(obs) != 0 {
		t.Fatalf("panicking session must write no observations, got %d", len(obs))
	}
}

// The drain contract (ported from the old cmd-level drainQueue tests, now that the
// loop lives in the engine): every pending job attempted at most once per run,
// jobs arriving mid-drain picked up, and termination even if a job stays pending.
func TestDrainProcessesArrivalsOnceAndTerminates(t *testing.T) {
	s := newStore(t)
	w := drainWorker(s, (&safeMiner{}).run)
	// Real L0 so mining does work; "stuck" stays in the synthetic pending set forever.
	for _, sess := range []string{"A", "B", "stuck"} {
		capture(t, s, sess, "user", "turn-"+sess)
	}

	pendingSet := map[string]bool{"A": true, "B": true, "stuck": true}
	pending := func() []string {
		out := []string{}
		for k := range pendingSet {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var order []string
	onCommit := func(session string) {
		order = append(order, session)
		if session == "A" {
			pendingSet["C"] = true // a new job arrives mid-drain
		}
		if session != "stuck" {
			delete(pendingSet, session) // normal jobs clear; "stuck" never does
		}
	}

	w.Drain(context.Background(), DrainOpts{Conc: 1, Pending: pending, OnCommit: onCommit})

	counts := map[string]int{}
	for _, sess := range order {
		counts[sess]++
	}
	for _, sess := range []string{"A", "B", "C", "stuck"} {
		if counts[sess] != 1 {
			t.Errorf("%s processed %d times, want exactly 1", sess, counts[sess])
		}
	}
}

func TestDrainStopsAfterBudget(t *testing.T) {
	s := newStore(t)
	w := drainWorker(s, (&safeMiner{}).run)
	for _, sess := range []string{"A", "B", "C"} {
		capture(t, s, sess, "user", "turn-"+sess)
	}
	pendingSet := map[string]bool{"A": true, "B": true, "C": true}
	pending := func() []string {
		out := []string{}
		for k := range pendingSet {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var order []string
	processed := w.Drain(context.Background(), DrainOpts{
		Conc: 1, Max: 1, Pending: pending,
		OnCommit: func(session string) { order = append(order, session); delete(pendingSet, session) },
	})
	if processed != 1 || len(order) != 1 || order[0] != "A" {
		t.Fatalf("processed=%d order=%v, want exactly the first job", processed, order)
	}
	if !pendingSet["B"] || !pendingSet["C"] {
		t.Fatalf("budgeted drain should leave remaining jobs queued: %#v", pendingSet)
	}
}

// The parallel path (Conc>1) must uphold the SAME attempt-once contract and be
// race-free. Run with `go test -race` to exercise the concurrent MAP phase.
func TestDrainParallelAttemptsEachOnce(t *testing.T) {
	s := newStore(t)
	m := &safeMiner{}
	w := drainWorker(s, m.run)
	const n = 12
	sessions := map[string]bool{}
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		capture(t, s, id, "user", "turn-"+id)
		sessions[id] = true
	}
	pending := func() []string {
		out := []string{}
		for k := range sessions {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var mu sync.Mutex
	committed := map[string]int{}
	processed := w.Drain(context.Background(), DrainOpts{
		Conc:    4,
		Pending: pending,
		OnCommit: func(session string) {
			mu.Lock()
			committed[session]++
			delete(sessions, session)
			mu.Unlock()
		},
	})
	if processed != n {
		t.Fatalf("expected %d committed, got %d", n, processed)
	}
	for id, c := range committed {
		if c != 1 {
			t.Fatalf("%s committed %d times, want exactly 1", id, c)
		}
	}
	// All sessions distilled: watermark advanced, observations written for each.
	obs, _ := s.ReadObservations("")
	if len(obs) != n {
		t.Fatalf("expected %d observations (one per session), got %d", n, len(obs))
	}
}

// PR2 re-check loop: the worker keeps draining while `capture` lands new sessions
// mid-run, WITHOUT any external wakeup. Simulated by the worker calling Drain in a
// loop with a SHARED Attempted set (as runWorkerInRange does): a new session
// appears after the first Drain empties the queue, and the next Drain picks it up.
func TestDrainRecheckLoopPicksUpMidRunArrivals(t *testing.T) {
	s := newStore(t)
	w := drainWorker(s, (&safeMiner{}).run)
	capture(t, s, "first", "user", "turn-first")

	released := false // becomes true after the first Drain empties the queue
	pending := func() []string {
		p, _ := s.PendingSessions(nil)
		if released && s.RawCount("second") == 0 {
			// a new session lands the instant the first drain finished
			capture(t, s, "second", "user", "turn-second")
			p, _ = s.PendingSessions(nil)
		}
		return p
	}

	attempted := map[string]bool{}
	passes, total := 0, 0
	for {
		n := w.Drain(context.Background(), DrainOpts{Conc: 1, Pending: pending, Attempted: attempted})
		passes++
		total += n
		released = true
		if n == 0 {
			break
		}
	}
	// Both sessions must be distilled, across more than one pass (the arrival was
	// caught by the loop, not the first drain).
	if raw, _ := s.ReadObservations(""); len(raw) != 2 {
		t.Fatalf("expected 2 observations (first + mid-run arrival), got %d", len(raw))
	}
	if s.DistilledCount("second", "default") == 0 {
		t.Fatal("mid-run arrival 'second' was never distilled by the re-check loop")
	}
	if passes < 2 {
		t.Fatalf("expected the arrival to require a second pass, ran %d pass(es)", passes)
	}
}

// PR2 livelock guard: a session that stays pending WITHOUT backing off (a commit
// path that never advances) must be attempted exactly once across the shared-set
// re-check loop, so the loop terminates instead of re-mining forever. We simulate
// "stuck" with a miner that always errors (MineFailed → backoff), plus the shared
// Attempted set that the outer loop relies on.
func TestDrainRecheckLoopTerminatesOnStuckSession(t *testing.T) {
	s := newStore(t)
	// Miner always fails → the session backs off and drops out of PendingSessions,
	// but we also assert the shared Attempted set prevents re-attempt within the run.
	m := &safeMiner{}
	w := drainWorker(s, func(ctx context.Context, a, b, c string) (string, error) {
		m.mu.Lock()
		m.calls++
		m.mu.Unlock()
		return "", context.DeadlineExceeded // transport-style failure
	})
	capture(t, s, "stuck", "user", "turn")

	// A pending source that would ALWAYS return the stuck session (ignores backoff),
	// so only the shared Attempted set can stop the loop.
	pending := func() []string { return []string{"stuck"} }

	attempted := map[string]bool{}
	passes := 0
	for passes < 100 { // hard cap so a bug fails loudly instead of hanging
		n := w.Drain(context.Background(), DrainOpts{Conc: 1, Pending: pending, Attempted: attempted})
		passes++
		if n == 0 {
			break
		}
	}
	if passes >= 100 {
		t.Fatal("re-check loop did not terminate on a perpetually-pending session")
	}
	if m.calls != 1 {
		t.Fatalf("stuck session mined %d times, want exactly 1 (shared Attempted set)", m.calls)
	}
}

// Review #1 regression: a session that was ALREADY drained this run but then gains
// new turns (resumed session) must be RE-ATTEMPTED, not filtered out by the shared
// attempted set. The (session, RawCount) key is what makes this work — the old
// session-only key would silently skip the new delta and it would sit undistilled
// until an unrelated later trigger. Uses a lying pending() that always returns the
// session, so ONLY the attempt-key logic decides re-attempt vs skip.
func TestDrainReattemptsRegrownSession(t *testing.T) {
	s := newStore(t)
	w := drainWorker(s, (&safeMiner{}).run)
	capture(t, s, "S", "user", "turn-1")

	grown := false
	pending := func() []string { return []string{"S"} } // always claims S is pending

	attempted := map[string]bool{}
	passes := 0
	for passes < 20 {
		n := w.Drain(context.Background(), DrainOpts{Conc: 1, Pending: pending, Attempted: attempted})
		passes++
		if n == 0 {
			if !grown {
				// After the first drain settles, the session is resumed with a new turn.
				capture(t, s, "S", "user", "turn-2")
				grown = true
				continue // the regrown session must be picked up on the next pass
			}
			break
		}
	}
	if passes >= 20 {
		t.Fatal("loop did not terminate")
	}
	// Both deltas distilled: watermark advanced to all 2 raw turns.
	if got := s.DistilledCount("S", "default"); got != 2 {
		t.Fatalf("regrown session watermark = %d, want 2 (both turns distilled)", got)
	}
}

// Review #3: the parallel path must be a TRUE rolling window — a slow oldest job
// must NOT idle the other slots. We make session "slow0" block on a gate while the
// rest finish fast; if the window were FIFO-blocked on the oldest, the fast jobs
// couldn't complete until slow0 released. We assert the fast jobs finish while
// slow0 is still blocked.
func TestDrainRollingWindowDoesNotHeadOfLineBlock(t *testing.T) {
	s := newStore(t)
	gate := make(chan struct{})
	var fastDone int32
	miner := func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "slow0") {
			<-gate // block the oldest submission until the fast ones have run
		} else {
			atomic.AddInt32(&fastDone, 1)
		}
		arr := []minedObs{{Dimension: "thinking", Observation: "obs:" + input, Evidence: "e", Poignancy: 3}}
		b, _ := json.Marshal(arr)
		return string(b), nil
	}
	w := drainWorker(s, miner)

	const n = 4
	ids := []string{"slow0", "b1", "c2", "d3"} // slow0 submitted first (the oldest)
	for _, id := range ids {
		capture(t, s, id, "user", "turn-"+id)
	}
	pending := func() []string { return append([]string(nil), ids...) }

	go func() {
		// Wait until the 3 fast jobs have all run despite slow0 blocking, then release.
		for atomic.LoadInt32(&fastDone) < n-1 {
			runtime.Gosched()
		}
		close(gate)
	}()

	done := make(chan int, 1)
	go func() {
		done <- w.Drain(context.Background(), DrainOpts{Conc: n, Pending: pending})
	}()

	select {
	case got := <-done:
		if got != n {
			t.Fatalf("committed %d, want %d", got, n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Drain hung — rolling window head-of-line blocked on the slow oldest job")
	}
	if obs, _ := s.ReadObservations(""); len(obs) != n {
		t.Fatalf("expected %d observations, got %d", n, len(obs))
	}
}
