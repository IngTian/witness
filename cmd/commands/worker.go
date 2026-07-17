package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newInternalWorkerCmd() *cobra.Command {
	var auto bool
	var since string
	var until string
	c := &cobra.Command{Use: "worker", Hidden: true, RunE: func(_ *cobra.Command, args []string) error {
		if auto {
			args = append(args, "--auto")
		}
		if since != "" {
			args = append(args, "--since", since)
		}
		if until != "" {
			args = append(args, "--until", until)
		}
		return cmdWorker(args)
	}}
	c.Flags().BoolVar(&auto, "auto", false, "run with automatic distillation limits")
	c.Flags().StringVar(&since, "since", "", "only sessions updated at or after this time")
	c.Flags().StringVar(&until, "until", "", "only sessions updated at or before this time")
	return c
}

// cmdWorker is the single-flight background consumer. It holds one global lock for
// its whole run, drains pending sessions (bounded only for automatic runs), then
// runs the reviewer if due. Triggers just spawn this; if a consumer is already
// running, the new one no-ops immediately. The filesystem is the durable job
// queue; this lock elects the single consumer that drains it.
func cmdWorker(args []string) error {
	auto, timeRange, err := workerFlags(args)
	if err != nil {
		return err
	}
	_, err = runWorkerInRange(auto, timeRange)
	return err
}

func workerFlags(args []string) (bool, sessionTimeRange, error) {
	auto := false
	since := ""
	until := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--auto":
			auto = true
		case "--since", "--until":
			if i+1 >= len(args) {
				return false, sessionTimeRange{}, fmt.Errorf("%s requires a value", args[i])
			}
			if args[i] == "--since" {
				since = args[i+1]
			} else {
				until = args[i+1]
			}
			i++
		default:
			return false, sessionTimeRange{}, fmt.Errorf("usage: witness worker [--auto] [--since <time>] [--until <time>]")
		}
	}
	timeRange, err := parseSessionTimeRange(since, until, time.Now())
	return auto, timeRange, err
}

func runWorker(auto bool) (bool, error) {
	return runWorkerInRange(auto, sessionTimeRange{})
}

func runWorkerInRange(auto bool, timeRange sessionTimeRange) (bool, error) {
	st, err := store.Open()
	if err != nil {
		return false, err
	}
	defer st.Close()
	defer setupLogging(st)()

	unlock, ok := st.WorkerLock()
	if !ok {
		return false, nil // a consumer already holds the lock; our jobs are on disk for it
	}
	defer unlock()
	if auto && st.MetaString("worker_stop_requested") == "1" {
		_ = st.SetMetaString("worker_mode", "")
		return true, nil
	}
	if !auto {
		_ = st.SetMetaString("worker_stop_requested", "")
	}
	started := time.Now().UTC().Format(time.RFC3339)
	// No worker_status/worker_started_at: liveness is the flock (issue #75) and the
	// running/stopping sub-state is DERIVED (see cmdDistillStatus) from the flock plus
	// worker_stop_requested — storing it too was the redundant third source. The keys
	// below are the ones still doing work: worker_pid is `distill stop`'s kill target,
	// worker_mode drives `distill stop --auto-only`, and worker_current/heartbeat are
	// diagnostic display (read only while the flock says a worker is live).
	_ = st.SetMetaString("worker_pid", strconv.Itoa(os.Getpid()))
	if auto {
		_ = st.SetMetaString("worker_mode", "auto")
	} else {
		_ = st.SetMetaString("worker_mode", "manual")
	}
	_ = st.SetMetaString("worker_heartbeat", started)
	// Clear the diagnostic detail on graceful exit so the store doesn't carry a dead
	// worker's pid/mode/current. Not required for correctness — cmdDistillStatus gates
	// every read on WorkerActive(), and the next worker overwrites these on start — but
	// it keeps an idle store clean.
	defer func() {
		_ = st.SetMetaString("worker_pid", "")
		_ = st.SetMetaString("worker_mode", "")
		_ = st.SetMetaString("worker_current", "")
		_ = st.SetMetaString("worker_heartbeat", time.Now().UTC().Format(time.RFC3339))
	}()
	if timeRange.empty() {
		defer scheduleRetryWakeup(st)
	}

	cfg := st.LoadConfig()
	// Resolve the effective GLOBAL runner ONCE and overwrite cfg.Runner: it is the runtime
	// a lens with no explicit `# runner` rides, and the target the per-lens router
	// (distill.RunnerFor) compares against. This is what lets an npm OpenCode user — who
	// never ran `install`, so config says the default "claude" — distill via OpenCode:
	// their plugin passes WITNESS_RUNNER=opencode. An explicit `install` choice
	// (runner_bound) still wins (see ResolveRunner). Per-lens runners layer on top via
	// newRunnerSet below.
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		slog.Error("load lenses", "err", err)
		return true, err
	}
	// The pending query cross-joins sessions against the ACTIVE lens set (#55). Use
	// the same loaded lenses the drain will mine, so the queue only offers pairs the
	// worker can actually process (a config-enabled-but-unloadable lens is excluded by
	// activeLenses above, so it won't spin here).
	lensNames := make([]string, len(lenses))
	for i, l := range lenses {
		lensNames[i] = l.Name
	}
	// Cancel the drain context on SIGTERM/SIGINT so a `distill stop` (SIGTERM to the
	// detached worker) or a Ctrl-C on a foreground `--all` backfill (SIGINT) tears
	// down in-flight `claude -p` children too — the ctx threads worker→Drain→
	// MineSession→Run→exec.CommandContext, and cancelling it sends the child a kill.
	// Without this, stopping the parent left up to `conc` orphaned children to run to
	// their own 10-min timeout (issue #22 audit). runner.Close still runs (deferred)
	// to sweep any OpenCode distill sessions.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	pending := func() []string {
		p, _ := st.PendingSessionsUpdatedBetween(lensNames, timeRange.since, timeRange.until)
		return p
	}
	// Resolve the SET of distillation runners the active lenses need and, if there's work,
	// Open each for the whole drain (issue #75 slice 2). Slice 1 opened one global runner;
	// now a lens may route to its own runtime, so the drain opens every distinct runtime
	// (OpenCode: one `opencode serve` + a pre-cleanup sweep; Claude: no-op). A non-global
	// runtime whose Open fails is circuit-broken (its lenses back off) rather than failing
	// the whole drain; only the GLOBAL runner failing is fatal (see newRunnerSet). The
	// combined Close tears every opened runner down + runs each one's post-cleanup sweep.
	var rs *runnerSet
	hasPending := len(pending()) > 0
	if hasPending || st.ReviewDue(cfg) {
		rs, err = newRunnerSet(ctx, st, cfg, lenses)
		if err != nil {
			slog.Error("open runners", "runner", cfg.Runner, "err", err)
			return true, err
		}
		defer rs.Close()
	}
	// runFn is the GLOBAL runner's MineFunc — used by the reviewer's/summarizer's unified
	// pass and as the Worker's default Run; per-lens routing goes through rs.RunFor. When
	// there's no work (rs nil), these are never called.
	runFn := func(ctx context.Context, model, prompt, input string) (string, error) {
		if rs == nil {
			return "", &runnerDownError{name: cfg.Runner}
		}
		return rs.RunFor(nil)(ctx, model, prompt, input)
	}

	// Drain pending sessions. The embedder is heavy (~448MB) and loaded once per
	// short-lived worker process ONLY when there is mining to do; it is shared
	// across the parallel miners (its own mutex serializes concurrent Embed calls,
	// which is cheap next to the LLM latency). Mining runs up to `conc` sessions at
	// once (MAP); commits are serial (REDUCE) — see distill.Worker.Drain.
	//
	// RE-CHECK LOOP (no external wakeup cascade): `capture` writes L0 without taking
	// WorkerLock, so new work can land WHILE this worker drains — and any trigger it
	// fires no-ops because we hold the lock. So after each Drain we re-check the
	// queue ourselves and keep working while it grows, instead of relying on a
	// detached per-second wakeup to re-drive us (the old 1 Hz CPU-peg cascade). A
	// SHARED `attempted` set across iterations makes this safe: a session that stays
	// pending without backing off (a commit/read error, not a mining timeout) is
	// tried once total, so a persistent failure can't spin the loop or re-mine
	// forever. We only loop again while a pass makes progress (commits > 0).
	stopRequested := func() bool { return st.MetaString("worker_stop_requested") == "1" }

	// Lazily build the parallel-mining worker the first time there is mining to do;
	// the embedder (~448MB) loads once and is shared across drain passes. nil if the
	// model isn't ready (review-only work can still proceed).
	var miner *distill.Worker
	attempted := map[string]bool{}
	// runForLens is the per-lens runner resolver threaded into the engine (issue #75
	// slice 2): a lens declaring its own runtime mines/reviews against that runner. nil-safe
	// when there's no work (rs nil) — never called in that case (getMiner returns without
	// mining, review only runs when rs is open).
	runForLens := func(ln *lens.Lens) distill.MineFunc {
		if rs == nil {
			return runFn
		}
		return rs.RunFor(ln)
	}
	getMiner := func() *distill.Worker {
		if miner == nil {
			emb, err := embed.New()
			if err != nil {
				slog.Error("embedder", "err", err) // can't mine this run; review may still run
				return nil
			}
			miner = &distill.Worker{Store: st, Embedder: emb, Lenses: lenses, Config: cfg, Run: runFn, RunFor: runForLens}
		}
		return miner
	}
	// The session-window cap must be safe for EVERY opened runtime (different sessions run
	// in parallel and may each touch any runtime). AND across the set; falls back to the
	// global runner's fact when there's no set yet (review-only / no work).
	concurrentSafe := true
	if rs != nil {
		concurrentSafe = rs.concurrentRunSafe()
	}
	conc := distill.EffectiveConcurrency(cfg.MineConcurrency, concurrentSafe)
	drainAll := func() {
		w := getMiner()
		if w == nil {
			return
		}
		for !stopRequested() {
			n := w.Drain(ctx, distill.DrainOpts{
				Conc:      conc,
				Pending:   pending,
				Stop:      stopRequested,
				Attempted: attempted,
				OnCommit: func(session string) {
					_ = st.SetMetaString("worker_current", session)
					_ = st.SetMetaString("worker_heartbeat", time.Now().UTC().Format(time.RFC3339))
				},
			})
			if n == 0 {
				break
			}
		}
	}

	runDistillLoop(distillLoopDeps{
		stopRequested: stopRequested,
		pending:       pending,
		drainAll:      drainAll,
		reviewDue:     func() bool { return st.ReviewDue(cfg) },
		runReview: func() {
			r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: runForLens}
			if err := r.Run(ctx, time.Now()); err != nil {
				slog.Error("review", "err", err)
			} else {
				slog.Info("review complete")
				regenerateProfile(ctx, st, cfg, lenses, runFn, runForLens)
			}
		},
		ensureMiner:    func() bool { return getMiner() != nil },
		hasUnattempted: func(p []string) bool { return miner.HasUnattempted(p, attempted) },
	})
	return true, nil
}

// distillLoopDeps is the set of seams runDistillLoop needs. Extracted so the
// drain→review→re-check loop is unit-testable without a real store, embedder, or
// `claude -p` (the inline version could only be exercised end-to-end, which is why
// the review-only livelock — issue #49 C1 — shipped uncaught).
type distillLoopDeps struct {
	stopRequested  func() bool         // a graceful-stop was requested → exit promptly
	pending        func() []string     // currently-distillable sessions (re-consulted each pass)
	drainAll       func()              // drain every pending session (no-op if none / no miner)
	reviewDue      func() bool         // is an L2 review due (session-count or poignancy)
	runReview      func()              // run the reviewer + regenerate the L4 profile
	ensureMiner    func() bool         // lazily build the miner; false if the embedder is unavailable
	hasUnattempted func([]string) bool // any of these sessions not yet attempted this run
}

// runDistillLoop is the worker's drain→review→RE-CHECK-before-unlock loop (issue
// #22 review #1). `capture` writes L0 without WorkerLock, so work can arrive while
// we hold it — during the (possibly multi-second) review, or in the drain→unlock
// tail. A trigger fired in that window no-ops because we hold the lock, and nothing
// else re-drives ordinary L0 (scheduleRetryWakeup only covers backed-off sessions).
// So we loop: drain everything, review if due, then look again — if new pending
// work appeared, go around. The (session,RawCount) attempted key (inside
// hasUnattempted) lets a resumed/regrown session re-enter while a stuck one stays
// filtered, so this loop terminates. Only a row landing in the sub-millisecond gap
// between the final pending() check and unlock falls to the next-launch sweep.
func runDistillLoop(d distillLoopDeps) {
	for !d.stopRequested() {
		// Gate the drain on a FRESH pending() check every pass — never a stale
		// pre-loop snapshot. Gating on a one-time `hasPending` livelocked a review-only
		// worker: it starts with nothing pending, so the drain ran only once; if
		// capture then landed L0 during the multi-second review, the re-check below
		// saw the new session and looped, but the stale gate kept the drain from ever
		// draining it — an unbounded spin holding WorkerLock, the new session never
		// distilled (issue #49 C1). A fresh pending() also skips the ~448MB embedder
		// load when there is genuinely nothing to mine (drainAll no-ops regardless).
		if len(d.pending()) > 0 {
			d.drainAll()
		}
		// Review folded into the same single-flight pass (serialized under the lock,
		// so concurrent reviews can't clobber the facets).
		if !d.stopRequested() && d.reviewDue() {
			d.runReview()
		}
		// Re-check under the lock: did capture land NEW distillable work while we
		// drained/reviewed? Consult pending() BEFORE ensureMiner() so a review-only run
		// that never had work releases the lock without paying the embedder load. But
		// when fresh work HAS arrived we build the miner and loop to drain it here — a
		// review-only worker won't have built it above, and deferring the session to
		// the next launch is exactly the gap this re-check closes. ensureMiner()==false
		// means the embedder is unavailable (can't mine at all this run) → stop rather
		// than spin on work we can never attempt.
		p := d.pending()
		if len(p) == 0 || !d.ensureMiner() || !d.hasUnattempted(p) {
			break
		}
	}
}

// regenerateProfile refreshes the L4 narrative summaries from the current facets.
// Best-effort: any failure (missing prompts, a claude -p hiccup) is logged and
// swallowed, leaving the prior summaries in place — the profile is derived and
// non-critical, and must never break the worker.
func regenerateProfile(ctx context.Context, st *store.Store, cfg store.Config, lenses []*lens.Lens, runFn distill.MineFunc, runForLens func(*lens.Lens) distill.MineFunc) {
	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts()
	if err != nil {
		slog.Warn("profile: summarizer prompts unavailable; skipping", "err", err)
		return
	}
	sm := &distill.Summarizer{Store: st, Config: cfg, Lenses: lenses, LensPrompt: lensPrompt, UnifiedPrompt: unifiedPrompt, Run: runFn, RunFor: runForLens}
	if err := sm.Summarize(ctx); err != nil {
		slog.Warn("profile: summary regeneration failed; keeping prior", "err", err)
		return
	}
	slog.Info("profile regenerated")
}
