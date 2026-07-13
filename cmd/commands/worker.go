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
	"github.com/IngTian/witness/internal/platform"
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
	_ = st.SetMetaString("worker_status", "running")
	_ = st.SetMetaString("worker_pid", strconv.Itoa(os.Getpid()))
	if auto {
		_ = st.SetMetaString("worker_mode", "auto")
	} else {
		_ = st.SetMetaString("worker_mode", "manual")
	}
	_ = st.SetMetaString("worker_started_at", started)
	_ = st.SetMetaString("worker_heartbeat", started)
	defer func() {
		_ = st.SetMetaString("worker_status", "idle")
		_ = st.SetMetaString("worker_pid", "")
		_ = st.SetMetaString("worker_mode", "")
		_ = st.SetMetaString("worker_current", "")
		_ = st.SetMetaString("worker_heartbeat", time.Now().UTC().Format(time.RFC3339))
	}()
	if timeRange.empty() {
		defer scheduleRetryWakeup(st)
	}

	cfg := st.LoadConfig()
	// Resolve the effective runner ONCE here and overwrite cfg.Runner, so the single
	// Runner built below (platform.RunnerFor) inherits it with no per-site dispatch.
	// This is what lets an npm OpenCode user — who never ran `install`, so config
	// says the default "claude" — distill via OpenCode: their plugin passes
	// WITNESS_RUNNER=opencode. An explicit `install` choice (runner_bound) still wins
	// (see ResolveRunner).
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		slog.Error("load lenses", "err", err)
		return true, err
	}
	ctx := context.Background()

	pending := func() []string {
		p, _ := st.PendingSessionsUpdatedBetween(timeRange.since, timeRange.until)
		return p
	}
	// Resolve the global distillation runner once and, if there's work, Open it for
	// the whole drain (OpenCode: one `opencode serve` + a pre-cleanup sweep; Claude:
	// no-op). Close runs the paired post-cleanup — so nothing leaks distill sessions.
	runner, err := platform.RunnerFor(st, cfg)
	if err != nil {
		slog.Error("resolve runner", "err", err)
		return true, err
	}
	hasPending := len(pending()) > 0
	if hasPending || st.ReviewDue(cfg) {
		if err := runner.Open(ctx); err != nil {
			slog.Error("open runner", "runner", cfg.Runner, "err", err)
			return true, err
		}
	}
	defer runner.Close()
	runFn := distill.RunnerMine(runner)

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
	if hasPending && !stopRequested() {
		emb, err := embed.New()
		if err != nil {
			slog.Error("embedder", "err", err) // can't mine this run; fall through to review
		} else {
			conc := distill.EffectiveConcurrency(cfg.MineConcurrency, runner.ConcurrentRunSafe())
			w := &distill.Worker{Store: st, Embedder: emb, Lenses: lenses, Config: cfg, Run: runFn}
			attempted := map[string]bool{}
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
				// No progress this pass → either the queue is empty or every remaining
				// session was already attempted this run (stuck/backed-off). Stop; a
				// backed-off session is re-driven later by scheduleRetryWakeup, and any
				// row that landed in the tail window is caught by the session-start
				// backlog sweep on the next launch.
				if n == 0 {
					break
				}
			}
		}
	}

	// Review folded into the same single-flight pass (serialized under the lock,
	// so concurrent reviews can't clobber the facets). Due on the session-count
	// cap OR accumulated poignancy — whichever first. A successful review updates
	// the facets, so we regenerate the L4 narrative profile right after ("on
	// profile change"). The profile is purely derived: summarizing is best-effort
	// (log and move on), never failing the worker or blocking distillation.
	if st.MetaString("worker_stop_requested") != "1" && st.ReviewDue(cfg) {
		r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn}
		if err := r.Run(ctx, time.Now()); err != nil {
			slog.Error("review", "err", err)
		} else {
			slog.Info("review complete")
			regenerateProfile(ctx, st, cfg, runFn)
		}
	}
	return true, nil
}

// regenerateProfile refreshes the L4 narrative summaries from the current facets.
// Best-effort: any failure (missing prompts, a claude -p hiccup) is logged and
// swallowed, leaving the prior summaries in place — the profile is derived and
// non-critical, and must never break the worker.
func regenerateProfile(ctx context.Context, st *store.Store, cfg store.Config, runFn distill.MineFunc) {
	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts()
	if err != nil {
		slog.Warn("profile: summarizer prompts unavailable; skipping", "err", err)
		return
	}
	sm := &distill.Summarizer{Store: st, Config: cfg, LensPrompt: lensPrompt, UnifiedPrompt: unifiedPrompt, Run: runFn}
	if err := sm.Summarize(ctx); err != nil {
		slog.Warn("profile: summary regeneration failed; keeping prior", "err", err)
		return
	}
	slog.Info("profile regenerated")
}
