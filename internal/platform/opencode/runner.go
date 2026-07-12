package opencode

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

var errRunBeforeOpen = errors.New("opencode runner: Run called before Open")

// NewRunner mints the OpenCode distillation runner, bound to cfg's models. It
// reuses ONE `opencode serve` process across every Run in a drain: Open sweeps
// stale distill sessions then starts the server; Close stops it and sweeps again.
// Baking that pairing into the runner is what keeps any call site (worker AND
// manual review) from leaking witness's own distill sessions — the sweep lives in
// Close, so it can't be forgotten.
func (Platform) NewRunner(cfg store.Config) platform.Runner {
	return &runner{cfg: cfg}
}

type runner struct {
	cfg    store.Config
	server *OpenCodeServer
}

func (r *runner) Open(ctx context.Context) error {
	cleanupDistillSessions(ctx, time.Now().Add(-1*time.Hour))
	srv, err := StartOpenCodeServer(ctx, r.cfg.TriageModel, r.cfg.DistillModel)
	if err != nil {
		return err
	}
	r.server = srv
	return nil
}

func (r *runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if r.server == nil {
		return "", errRunBeforeOpen
	}
	return r.server.Run(ctx, model, systemPrompt, input)
}

func (r *runner) Close() error {
	if r.server == nil {
		return nil // never opened (no work this drain) — nothing to stop or sweep
	}
	err := r.server.Close()
	// Post-cleanup uses a detached context so it still runs when the drain's ctx is
	// already done; the small +time.Second window covers a session created moments
	// before Close.
	cleanupDistillSessions(context.Background(), time.Now().Add(time.Second))
	return err
}

func (r *runner) ValidateModels(ctx context.Context, models ...string) error {
	return ValidateOpenCodeModels(ctx, models...)
}

func (*runner) InvocationHint() string { return "opencode serve" }

// cleanupDistillSessions removes witness's own distill sessions from the OpenCode
// DB so they aren't re-ingested as user sessions. Lives beside the runner so both
// the worker and the manual review path get it via Runner.Close().
func cleanupDistillSessions(ctx context.Context, before time.Time) {
	dbPath, err := DefaultDBPath()
	if err != nil {
		slog.Warn("opencode cleanup: locate db", "err", err)
		return
	}
	deleted, err := CleanupWitnessDistillSessions(ctx, dbPath, before)
	if err != nil {
		slog.Warn("opencode cleanup: witness-distill sessions", "err", err)
		return
	}
	if deleted > 0 {
		slog.Info("opencode cleanup: removed witness-distill sessions", "count", deleted)
	}
}
