package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	c := &cobra.Command{Use: "worker-run", Hidden: true, RunE: func(_ *cobra.Command, args []string) error {
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
	// Silent: this is the detached worker every trigger spawns.
	_, err = runWorkerInRange(auto, timeRange, "")
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
			return false, sessionTimeRange{}, fmt.Errorf("usage: witness worker-run [--auto] [--since <time>] [--until <time>]")
		}
	}
	timeRange, err := parseSessionTimeRange(since, until, time.Now())
	return auto, timeRange, err
}

// The worker's liveness meta keys and worker_mode values. Named, because a misspelt key reads
// as "unset": a typo'd stop key is "no stop requested", and the worker keeps making paid calls.
const (
	metaWorkerStopRequested = "worker_stop_requested"
	metaWorkerMode          = "worker_mode"
	metaWorkerPID           = "worker_pid"
	metaWorkerHeartbeat     = "worker_heartbeat"
	metaWorkerCurrent       = "worker_current"

	workerModeAuto        = "auto"
	workerModeManual      = "manual"
	workerModeAutoPending = "auto-pending" // spawned by the auto gate, lock not yet claimed
)

// runWorker is the SILENT worker run: background wakeups, auto runs, and any caller that has not
// asked for progress.
func runWorker(auto bool) (bool, error) {
	return runWorkerInRange(auto, sessionTimeRange{}, "")
}

// runWorkerInRange runs one worker pass. progressWhere opts a FOREGROUND caller into a progress
// line on stderr for the drain, labelled with it (a backfill passes its lens): sessions mined /
// total. "" is silent, and every background, detached or auto run must pass "" — nobody is
// watching their stderr. (Startup gets no line: runners + embedder measured 0.3-0.5s.)
func runWorkerInRange(auto bool, timeRange sessionTimeRange, progressWhere string) (bool, error) {
	return runWorkerTapped(auto, timeRange, progressWhere, nil)
}

// runWorkerTapped is runWorkerInRange that notes the first runner, embedder or model error in tap,
// so a foreground caller can put the cause of an unfinished drain on the screen. nil notes nothing.
func runWorkerTapped(auto bool, timeRange sessionTimeRange, progressWhere string, tap *causeTap) (bool, error) {
	st, err := store.Open()
	if err != nil {
		return false, err
	}
	defer st.Close()
	defer setupLogging(st)()
	// Refuse a newer archive before the lock, the meta stamps and the retry wakeup: the capture
	// hooks spawn this on every turn, and it should exit with the reason in witness.log, not try.
	if err := st.ArchiveNewer(); err != nil {
		slog.Error("worker refused", "err", err)
		return false, err
	}
	cfg := st.LoadConfig()
	if auto && !cfg.AutoDistill {
		return false, nil
	}

	unlock, ok := st.WorkerLock()
	if !ok {
		return false, nil // a consumer already holds the lock; our jobs are on disk for it
	}
	defer unlock()
	if auto && st.MetaString(metaWorkerStopRequested) == "1" {
		_ = st.SetMetaString(metaWorkerMode, "")
		return true, nil
	}
	if !auto {
		_ = st.SetMetaString(metaWorkerStopRequested, "")
	}
	started := time.Now().UTC().Format(time.RFC3339)
	// No worker_status/worker_started_at: liveness is the flock (issue #75) and the
	// running/stopping sub-state is DERIVED (see cmdDistillStatus) from the flock plus
	// worker_stop_requested — storing it too was the redundant third source. The keys
	// below are the ones still doing work: worker_pid is `distill stop`'s kill target,
	// worker_mode drives `distill stop --auto-only`, and worker_current/heartbeat are
	// diagnostic display (read only while the flock says a worker is live).
	_ = st.SetMetaString(metaWorkerPID, strconv.Itoa(os.Getpid()))
	if auto {
		_ = st.SetMetaString(metaWorkerMode, workerModeAuto)
	} else {
		_ = st.SetMetaString(metaWorkerMode, workerModeManual)
	}
	_ = st.SetMetaString(metaWorkerHeartbeat, started)
	// Clear the diagnostic detail on graceful exit so the store doesn't carry a dead
	// worker's pid/mode/current. Not required for correctness — cmdDistillStatus gates
	// every read on WorkerActive(), and the next worker overwrites these on start — but
	// it keeps an idle store clean.
	defer func() {
		_ = st.SetMetaString(metaWorkerPID, "")
		_ = st.SetMetaString(metaWorkerMode, "")
		_ = st.SetMetaString(metaWorkerCurrent, "")
		_ = st.SetMetaString(metaWorkerHeartbeat, time.Now().UTC().Format(time.RFC3339))
	}()
	if timeRange.empty() {
		defer scheduleRetryWakeup(st)
	}

	// Resolve the effective default runner ONCE and overwrite cfg.Runner: it is the runtime
	// a lens with no explicit `# runner` rides, and the target the per-lens router
	// (distill.RunnerFor) compares against. This is what lets an npm OpenCode user — who
	// never ran `install`, so config says the default "claude" — distill via OpenCode:
	// their plugin passes WITNESS_RUNNER=opencode. An explicit `install` choice
	// (runner_bound) still wins (see ResolveRunner). Per-lens runners layer on top via
	// newRunnerSet below.
	cfg.Runner = st.ResolveRunner(cfg)
	lenses := activeLenses(st)
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
	// to sweep any OpenCode distill sessions. proc.NotifyStop is the port's
	// signal-aware ctx (SIGINT+SIGTERM).
	ctx, stopSignals := procCtl.NotifyStop(context.Background())
	defer stopSignals()

	pending := func() []string {
		p, _ := st.PendingSessionsUpdatedBetween(lensNames, timeRange.since, timeRange.until)
		return p
	}
	// A ranged drain filters on MAX(julianday(ts)), which is NULL when a session has no
	// timestamp SQLite can read — and NULL fails both bounds. That exclusion is correct (a
	// session with no known time can't be placed in a window) and such sessions are still
	// drained by the ordinary no-range worker, but it used to be entirely SILENT: `distill
	// start --since ...` printed "nothing pending" with no hint that anything was skipped.
	if !timeRange.empty() {
		if n := st.SessionsWithNoUsableTimestamp(); n > 0 {
			slog.Warn("range drain skips sessions whose raw timestamps are unreadable; an unranged `witness distill start` still mines them",
				"skipped_sessions", n)
		}
	}
	// Resolve the SET of distillation runners the active lenses need and, if there's work,
	// Open each for the whole drain (issue #75 slice 2). Slice 1 opened one default runner;
	// now a lens may route to its own runtime, so the drain opens every distinct runtime
	// (OpenCode: one `opencode serve` + a pre-cleanup sweep; Claude: no-op). A non-default
	// runtime whose Open fails is circuit-broken (its lenses back off) rather than failing
	// the whole drain; only the default runner failing is fatal (see newRunnerSet). The
	// combined Close tears every opened runner down + runs each one's post-cleanup sweep.
	// The runner set is opened LAZILY on first actual need (ensureRunners), not merely
	// from a pre-loop hasPending snapshot. `capture` writes L0 without WorkerLock, so work
	// can land in the gap between that snapshot and the re-check loop's fresh pending() — if
	// rs were only opened on the snapshot, that late arrival would mine through a nil runner
	// set and spuriously back off (a slice-2 regression vs slice-1, which wrapped the runner
	// unconditionally). Opening on first need closes that gap while still paying the
	// OpenCode-serve/embedder cost only on a run that genuinely mines/reviews. The Close is
	// guarded unconditionally so a lazily-opened runner never leaks.
	var rs *runnerSet
	var rsErr error
	defer func() {
		if rs != nil {
			rs.Close()
		}
	}()
	ensureRunners := func() bool {
		if rs == nil && rsErr == nil {
			rs, rsErr = newRunnerSet(ctx, st, cfg, lenses)
			if rsErr != nil {
				slog.Error("open runners", "runner", cfg.Runner, "err", rsErr)
				tap.note(rsErr)
			}
		}
		return rs != nil
	}
	// runFn is the default runner's MineFunc — used by the reviewer's/summarizer's unified
	// pass and as the Worker's default Run; per-lens routing goes through rs.RunFor. It is
	// only reached after ensureRunners() opened the set (the drain/review gates below), so
	// rs is non-nil here; the guard is a belt-and-suspenders against a misroute.
	untappedRunFn := func(ctx context.Context, model, prompt, input string) (string, error) {
		if rs == nil {
			return "", &runnerDownError{name: cfg.Runner}
		}
		return rs.RunFor(nil)(ctx, model, prompt, input)
	}
	runFn := tap.wrap(untappedRunFn)

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
	stopRequested := func() bool { return st.MetaString(metaWorkerStopRequested) == "1" }

	// Lazily build the parallel-mining worker the first time there is mining to do;
	// the embedder loads once and is shared across drain passes (its parsed weights
	// and execution state can expand well beyond the ~448MB asset). nil if the
	// model isn't ready (review-only work can still proceed).
	var miner *distill.Worker
	// minerFailed latches a startup failure for the rest of this run.
	//
	// Without it, `miner` is cached only on SUCCESS, so every later getMiner() call retries the
	// whole heavy startup — measured: a run with no embedding model re-attempted the load FOUR
	// times and logged the same error four times, because getMiner has two callers (drainAll and
	// the loop's ensureMiner) and the loop re-enters. That is not just log noise: the retried step
	// is a ~448MB model parse, so on a machine where the load is SLOW rather than absent, the
	// worker pays that cost repeatedly while appearing frozen. One attempt per run is the right
	// policy — the next wakeup is the retry, and it gets a fresh process.
	minerFailed := false
	attempted := map[string]bool{}
	// runForLens is the per-lens runner resolver threaded into the engine (issue #75
	// slice 2): a lens declaring its own runtime mines/reviews against that runner. Only
	// reached after ensureRunners() (getMiner/runReview gate on it), so rs is non-nil; the
	// runFn fallback is a belt-and-suspenders that would back off rather than nil-deref.
	untappedRunForLens := func(ln *lens.Lens) distill.MineFunc {
		if rs == nil {
			return untappedRunFn
		}
		return rs.RunFor(ln)
	}
	runForLens := tap.wrapFor(untappedRunForLens)
	// getMiner performs the worker's one-time heavy startup: opening the runner set (which for
	// OpenCode spawns a private `opencode serve` and validates models) and loading the ~448MB
	// embedding model through GoMLX.
	//
	// Every step here is TRACED, at info, with a before/after pair. That is not noise — it is the
	// difference between a diagnosable report and a guess. Both steps are slow, unbounded and were
	// previously SILENT on success, so a wedged one was indistinguishable from a frozen worker: the
	// heartbeat is already stamped, the process is alive, and nothing further is ever written. A
	// real Windows run reported exactly that ("heartbeat stamped once, never updated, no log
	// entries after startup"), and the log could not say which of the two it was. With these lines
	// the LAST one written names the blocker, and the elapsed time separates "slow" from "hung".
	getMiner := func() *distill.Worker {
		if minerFailed {
			return nil // already tried and failed this run; the next wakeup retries in a fresh process
		}
		if miner == nil {
			// Open the runner set before mining — closes the "work arrived after the pre-loop
			// snapshot, rs still nil" gap. A failed open (fatal default) means we can't mine
			// this run; return nil so the loop breaks and the next wakeup retries.
			slog.Info("worker startup: opening runners", "runner", cfg.Runner, "lenses", len(lenses))
			t0 := time.Now()
			if !ensureRunners() {
				minerFailed = true
				return nil
			}
			slog.Info("worker startup: runners open", "runner", cfg.Runner, "took", time.Since(t0).String())

			// The embedder is the single heaviest startup step: it parses a ~448MB ONNX model and
			// initializes a GoMLX backend, with no context and no timeout. A partial download
			// passes embed.ModelReady()'s size floor and can still stall or fail here, so the
			// asset path and size are logged too — that is the first thing to check when this is
			// the last line in the log.
			slog.Info("worker startup: loading the embedding model", "assets", embed.AssetsDir())
			t1 := time.Now()
			emb, err := embed.New()
			if err != nil {
				slog.Error("embedder", "err", err, "assets", embed.AssetsDir()) // can't mine this run; review may still run
				tap.note(fmt.Errorf("embedding model: %w", err))
				minerFailed = true
				return nil
			}
			slog.Info("worker startup: embedding model ready", "took", time.Since(t1).String())
			miner = &distill.Worker{Store: st, Embedder: emb, Lenses: lenses, Config: cfg, Run: runFn, RunFor: runForLens, NativeFor: rs.NativeFor}
		}
		return miner
	}
	drainAll := func() {
		w := getMiner() // opens the runner set (ensureRunners) as a side effect; nil if it can't
		if w == nil {
			return
		}
		// Compute the session-window cap AFTER the runners are open: it must be safe for
		// EVERY opened runtime (different sessions run in parallel and may each touch any
		// runtime), so AND across the set. rs is non-nil here (getMiner ensured it).
		conc := distill.EffectiveConcurrency(cfg.MineConcurrency, rs.concurrentRunSafe())
		// Trace the drain boundary for the same reason getMiner traces startup: Drain can spend
		// minutes inside one model call, and without this line a log that stops here is
		// ambiguous between "never started draining" and "started and is waiting on a model".
		slog.Info("worker: draining", "pending", len(pending()), "concurrency", conc)
		opts := distill.DrainOpts{
			Conc:      conc,
			Pending:   pending,
			Stop:      stopRequested,
			Attempted: attempted,
			OnCommit: func(session string) {
				_ = st.SetMetaString(metaWorkerCurrent, session)
				_ = st.SetMetaString(metaWorkerHeartbeat, time.Now().UTC().Format(time.RFC3339))
			},
		}
		var progress distill.ProgressFunc
		if progressWhere != "" {
			pl := startProgress()
			defer pl.finish() // cleared before any review this run does: that is not mining
			progress = pl.update
		}
		drainPass(func(o distill.DrainOpts) int { return w.Drain(ctx, o) }, opts, stopRequested, progress, progressWhere)
	}

	reviewRan := false
	runDistillLoop(distillLoopDeps{
		stopRequested: stopRequested,
		pending:       pending,
		drainAll:      drainAll,
		reviewDue:     func() bool { return st.ReviewDue(cfg) },
		runReview: func() {
			// Review also needs the runner set (its own per-lens routing + the unified
			// summary's default runner). Open it here so a review-only run (no mining) still
			// has live runners; a failed open skips the review this pass.
			if !ensureRunners() {
				return
			}
			r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: runForLens}
			// A foreground drain (lens backfill, worker run) reviews after it mines: the same
			// fold line and profile spinner as `witness worker review`, or that stretch is minutes
			// of silence after the mining line clears. A detached worker stays silent.
			fold := &phaseProgress{start: startProgress}
			if progressWhere != "" {
				r.Progress = fold.update
			}
			err := r.Run(ctx, time.Now())
			fold.finish()
			if err != nil {
				slog.Error("review", "err", err)
			} else {
				slog.Info("review complete")
				var sp *spinner
				if progressWhere != "" {
					sp = startSpinner("distilling the profile from the latest facets…")
				}
				_ = regenerateProfile(ctx, st, cfg, lenses, runFn, runForLens) // best-effort: logged
				sp.Stop()
				if progressWhere != "" {
					sp = startSpinner("linking the profile to its sources…")
				}
				err := resolvePortraitSources(ctx, st, cfg, lenses, untappedRunFn, untappedRunForLens) // best-effort: logged
				if err == nil {
					// Success: clear retry_after (post-review path ignores the gate but clears on success).
					_ = st.SetMetaString("sources_retry_after", "")
				}
				sp.Stop()
				reviewRan = true
			}
		},
		ensureMiner:    func() bool { return getMiner() != nil },
		hasUnattempted: func(p []string) bool { return miner.HasUnattempted(p, attempted) },
	})

	// End-of-run self-heal: if no review ran this worker run but portraits have stale sources,
	// resolve them. This catches portraits that became stale after the last review (facet edits,
	// portrait regeneration from a prompt change) without forcing a review. Only opens runners
	// when StaleSources returns work; an idle worker with everything current never pays the cost.
	// Respects stop request and context cancellation: after a stop, do not start new model calls.
	// Bounded retries: if sources_retry_after is set and not yet expired, skip the self-heal.
	if selfHealAllowed(reviewRan, stopRequested, ctx) {
		// Check retry-after gate (only honored by self-heal path).
		retryAfter := st.MetaString("sources_retry_after")
		if retryAfter != "" {
			if t, err := time.Parse(time.RFC3339, retryAfter); err == nil && time.Now().Before(t) {
				slog.Info("profile: skipping source resolution self-heal until retry window expires", "retry_after", retryAfter)
				// Skip the entire self-heal block below.
			} else {
				// Window expired or parse failed: proceed with self-heal.
				retryAfter = "" // mark as expired
			}
		}

		if retryAfter == "" {
			lensPrompt, unifiedPrompt, err := lens.LoadResolvePrompts()
			if err != nil {
				slog.Warn("profile: source resolver prompts unavailable; self-heal skipped this run", "err", err)
			} else {
				sm := &distill.Summarizer{Store: st, Config: cfg, Lenses: lenses, ResolvePrompt: lensPrompt, ResolveUnifiedPrompt: unifiedPrompt, Run: runFn, RunFor: runForLens}
				stale, err := sm.StaleSources()
				if err != nil {
					slog.Warn("profile: failed to check stale sources; self-heal skipped this run", "err", err)
				} else if len(stale) > 0 {
					if ensureRunners() {
						var sp *spinner
						if progressWhere != "" {
							sp = startSpinner("linking the profile to its sources…")
						}
						err := resolvePortraitSources(ctx, st, cfg, lenses, untappedRunFn, untappedRunForLens)
						if err != nil {
							// Self-heal failed: set retry_after = now + 6h.
							retryAt := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
							_ = st.SetMetaString("sources_retry_after", retryAt)
						} else {
							// Success: clear retry_after.
							_ = st.SetMetaString("sources_retry_after", "")
						}
						sp.Stop()
					}
				}
			}
		}
	}

	return true, nil
}

// drainPass calls drain until a call reaps nothing (or a stop is requested), carrying ONE progress
// count across the calls: progress (nil = silent) gets each snapshot named where and continued
// past the sessions earlier calls finished. A seam so the count's wiring is testable without the
// embedder a real Drain needs.
func drainPass(drain func(distill.DrainOpts) int, opts distill.DrainOpts, stop func() bool, progress distill.ProgressFunc, where string) {
	finished := 0 // sessions reaped by this pass's earlier Drain calls
	if progress != nil {
		opts.Progress = func(p distill.Progress) { progress(continuedDrain(p, where, finished)) }
	}
	for !stop() {
		n := drain(opts)
		finished += n // Drain returns exactly the sessions it reaped, i.e. its final Done
		if n == 0 {
			break
		}
	}
}

// continuedDrain names a Drain snapshot and continues its count past the sessions earlier Drain
// calls of the same pass already finished. Each Drain counts from zero, so without the offset a
// second call that found a late arrival would drop the line from 312/312 back to 0/1.
func continuedDrain(p distill.Progress, where string, finished int) distill.Progress {
	stages := make([]distill.Stage, len(p.Stages))
	for i, s := range p.Stages {
		s.Done += finished
		s.Total += finished
		stages[i] = s
	}
	return distill.Progress{Where: where, Stages: stages}
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
//
// It returns the failure so a FOREGROUND caller (`witness worker review`) does not print "profile
// regenerated" over one that was not; the worker ignores it.
func regenerateProfile(ctx context.Context, st *store.Store, cfg store.Config, lenses []*lens.Lens, runFn distill.MineFunc, runForLens func(*lens.Lens) distill.MineFunc) error {
	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts(st.Root)
	if err != nil {
		slog.Warn("profile: summarizer prompts unavailable; skipping", "err", err)
		return err
	}
	sm := &distill.Summarizer{Store: st, Config: cfg, Lenses: lenses, LensPrompt: lensPrompt, UnifiedPrompt: unifiedPrompt, Run: runFn, RunFor: runForLens}
	if err := sm.Summarize(ctx); err != nil {
		slog.Warn("profile: summary regeneration failed; keeping prior", "err", err)
		return err
	}
	slog.Info("profile regenerated")
	return nil
}

// resolvePortraitSources links portrait sentences to their source facets. Reads the resolver
// prompts, builds a Summarizer with ResolvePrompt/ResolveUnifiedPrompt, and calls
// ResolveSources(), which resolves every stale portrait and writes the links to disk.
// Failures are logged but never propagated: source resolution is auxiliary metadata that must
// never break the review or worker run. Returns error only for logging; callers must not
// propagate it into the review's or worker's exit status.
func resolvePortraitSources(ctx context.Context, st *store.Store, cfg store.Config, lenses []*lens.Lens, runFn distill.MineFunc, runForLens func(*lens.Lens) distill.MineFunc) error {
	lensPrompt, unifiedPrompt, err := lens.LoadResolvePrompts()
	if err != nil {
		slog.Warn("profile: resolver prompts unavailable; skipping source linking", "err", err)
		return err
	}
	sm := &distill.Summarizer{Store: st, Config: cfg, Lenses: lenses, ResolvePrompt: lensPrompt, ResolveUnifiedPrompt: unifiedPrompt, Run: runFn, RunFor: runForLens}
	stale, err := sm.StaleSources()
	if err != nil {
		slog.Warn("profile: failed to check stale sources", "err", err)
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	if err := sm.ResolveSources(ctx); err != nil {
		slog.Warn("profile: source resolution failed", "err", err)
		return err
	}
	slog.Info("profile: sources linked", "portraits", len(stale))
	return nil
}

// selfHealAllowed returns true when the end-of-run self-heal should run. It checks:
// - reviewRan: if a review already ran this worker run, skip (review already resolved)
// - stopRequested: if a stop was requested, skip (no fresh model calls after stop)
// - ctx.Err(): if context is cancelled, skip (worker is shutting down)
func selfHealAllowed(reviewRan bool, stopRequested func() bool, ctx context.Context) bool {
	return !reviewRan && !stopRequested() && ctx.Err() == nil
}
