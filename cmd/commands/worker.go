package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/lens"
	opencodeimport "github.com/IngTian/witness/internal/runtimes/opencode"
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
	// Resolve the effective runner ONCE here and overwrite cfg.Runner, so every
	// downstream consumer (the opencode-serve branch below, Worker/Reviewer/
	// Summarizer's RunWith) inherits it with no per-site change. This is what lets
	// an npm OpenCode user — who never ran `install`, so config says the default
	// "claude" — distill via OpenCode: their plugin passes WITNESS_RUNNER=opencode.
	// An explicit `install` choice (runner_bound) still wins (see ResolveRunner).
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		slog.Error("load lenses", "err", err)
		return true, err
	}
	ctx := context.Background()

	// Embedder is heavy (~448MB); load it lazily and once per short-lived worker
	// process, only if a session actually needs mining. There is deliberately no
	// resident embedding service: when the queue drains, the process exits and the
	// model memory is released.
	var emb *embed.Embedder
	var embErr error
	getEmb := func() (*embed.Embedder, error) {
		if emb == nil && embErr == nil {
			emb, embErr = embed.New()
		}
		return emb, embErr
	}

	pending := func() []string {
		p, _ := st.PendingSessionsUpdatedBetween(timeRange.since, timeRange.until)
		return p
	}
	var runFn distill.MineFunc
	if (len(pending()) > 0 || st.ReviewDue(cfg)) && strings.EqualFold(strings.TrimSpace(cfg.Runner), "opencode") {
		cleanupOpenCodeDistillSessions(ctx, time.Now().Add(-1*time.Hour))
		opencodeServer, err := distill.StartOpenCodeServer(ctx, cfg.TriageModel, cfg.DistillModel)
		if err != nil {
			slog.Error("opencode serve", "err", err)
			return true, err
		}
		defer func() {
			_ = opencodeServer.Close()
			cleanupOpenCodeDistillSessions(context.Background(), time.Now().Add(time.Second))
		}()
		runFn = opencodeServer.Run
	}
	sessionBudget := workerSessionBudget(cfg, auto)
	processedSessions := drainQueueLimit(pending, func(session string) {
		if st.MetaString("worker_stop_requested") == "1" {
			return
		}
		_ = st.SetMetaString("worker_current", session)
		_ = st.SetMetaString("worker_heartbeat", time.Now().UTC().Format(time.RFC3339))
		e, err := getEmb()
		if err != nil {
			slog.Error("embedder", "err", err)
			return
		}
		w := &distill.Worker{Store: st, Embedder: e, Lenses: lenses, Config: cfg, Run: runFn}
		if err := w.Process(ctx, session); err != nil {
			slog.Error("process session", "session", session, "err", err)
		}
	}, sessionBudget)
	remainingPending := len(pending())
	if workerBudgetReached(auto, sessionBudget, processedSessions, remainingPending) {
		next, _ := autoDistillNextAt(st, cfg.AutoDistillIntervalMinutes, time.Now())
		if next.IsZero() {
			next = time.Now().Add(time.Second)
		}
		scheduleWorkerWakeup(st, next, "auto")
		slog.Info("distill: runner background budget reached; leaving remaining work queued", "runner", cfg.Runner, "processed", processedSessions)
		return true, nil
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

func workerSessionBudget(cfg store.Config, auto bool) int {
	if !auto {
		return 0
	}
	return cfg.AutoDistillSessionBudget
}

func workerBudgetReached(auto bool, sessionBudget, processedSessions, remainingPending int) bool {
	return auto && sessionBudget > 0 && processedSessions >= sessionBudget && remainingPending > 0
}

func cleanupOpenCodeDistillSessions(ctx context.Context, before time.Time) {
	dbPath, err := opencodeimport.DefaultDBPath()
	if err != nil {
		slog.Warn("opencode cleanup: locate db", "err", err)
		return
	}
	deleted, err := opencodeimport.CleanupWitnessDistillSessions(ctx, dbPath, before)
	if err != nil {
		slog.Warn("opencode cleanup: witness-distill sessions", "err", err)
		return
	}
	if deleted > 0 {
		slog.Info("opencode cleanup: removed witness-distill sessions", "count", deleted)
	}
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
