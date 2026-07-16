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

// ConcurrentRunSafe is true: OpenCodeServer.Run now holds its mutex only for the
// closed-check (server.go), not the whole request, and each Run drives its own
// isolated OpenCode session over the shared http.Client. A benchmark against a
// real `opencode serve` confirmed the server accepts many concurrent isolated
// sessions (see the local concurrency probe / issue #22), so the engine may mine
// several OpenCode sessions at once. If the configured PROVIDER rate-limits, the
// excess requests queue at the provider and witness's existing backoff absorbs it
// — that is a provider property, not a witness serialization constraint.
func (*runner) ConcurrentRunSafe() bool { return true }

// SweepsOnClose is true: Close() runs cleanupDistillSessions (see Close above), a
// PROCESS-GLOBAL sweep of the shared OpenCode DB that deletes witness-distill
// sessions created before now+1s — which would delete a concurrent background
// worker's in-flight distill session. A tool that opens its own OpenCode runner
// alongside a possible worker (e.g. `witness lens try`) must therefore hold the
// single-flight WorkerLock for the runner's whole open→Close lifetime. Distinct from
// ConcurrentRunSafe (which is about calling Run concurrently, not about Close's
// cross-process reach). Claude does not implement this — its Close is a no-op.
func (*runner) SweepsOnClose() bool { return true }

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
