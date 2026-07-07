package commands

import (
	"context"
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
	return &cobra.Command{Use: "worker", Hidden: true, RunE: func(_ *cobra.Command, args []string) error { return cmdWorker(args) }}
}

// cmdWorker is the single-flight background consumer. It holds one global lock for
// its whole run, drains EVERY pending session (delta-distilling each, once per
// run), then runs the reviewer if due. Triggers (session-start/end) just spawn
// this; if a consumer is already running, the new one no-ops immediately — no
// blocked-process pile-up, no daemon. The filesystem is the durable job queue;
// this lock elects the single consumer that drains it.
func cmdWorker(_ []string) error {
	_, err := runWorker()
	return err
}

func runWorker() (bool, error) {
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
	started := time.Now().UTC().Format(time.RFC3339)
	_ = st.SetMetaString("worker_status", "running")
	_ = st.SetMetaString("worker_pid", strconv.Itoa(os.Getpid()))
	_ = st.SetMetaString("worker_started_at", started)
	_ = st.SetMetaString("worker_heartbeat", started)
	_ = st.SetMetaString("worker_stop_requested", "")
	defer func() {
		_ = st.SetMetaString("worker_status", "idle")
		_ = st.SetMetaString("worker_pid", "")
		_ = st.SetMetaString("worker_current", "")
		_ = st.SetMetaString("worker_heartbeat", time.Now().UTC().Format(time.RFC3339))
	}()
	defer scheduleRetryWakeup(st)

	cfg := st.LoadConfig()
	lenses, err := activeLenses(st)
	if err != nil {
		slog.Error("load lenses", "err", err)
		return true, err
	}
	ctx := context.Background()

	// Embedder is heavy (~448MB); load it lazily and once, only if a session
	// actually needs mining. Review doesn't need it.
	var emb *embed.Embedder
	var embErr error
	getEmb := func() (*embed.Embedder, error) {
		if emb == nil && embErr == nil {
			emb, embErr = embed.New()
		}
		return emb, embErr
	}

	pending := func() []string { p, _ := st.PendingSessions(); return p }
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
	sessionBudget := workerSessionBudget(cfg)
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
	if sessionBudget > 0 && processedSessions >= sessionBudget {
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

func workerSessionBudget(cfg store.Config) int {
	_ = cfg
	return 0
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
