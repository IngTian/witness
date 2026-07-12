package distill

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	opencodeimport "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

// Runner is the distillation-engine lifecycle, resolved ONCE per worker drain by
// NewRunner(cfg) and reused across every mine/review/summarize call. It replaces
// two older shapes that this file retires:
//
//   - the RunWith(runner, ...) string switch, which re-dispatched on the runner
//     name at EVERY call site (a second live dispatch duplicating this factory), and
//   - runOpenCode, which started a fresh `opencode serve` PER call.
//
// Crucially, a runner that persists sessions (OpenCode) runs its self-traffic
// cleanup in Close(), so no call site can forget it. That is what structurally
// fixes the review.go leak: the manual review path deferred the server's Close()
// but never the cleanup sweep the worker did — now the sweep lives in Close().
//
// This is the GLOBAL half of the platform split (issue #21): one engine drains
// every session regardless of the session's own source runtime. The per-session
// owning platform (which shapes L0→input) is a separate axis, handled elsewhere.
type Runner interface {
	// Open acquires whatever the engine needs to serve Run. OpenCode: sweep stale
	// distill sessions, then start `opencode serve`. Claude: no-op. Callers may skip
	// Open when there is no work; Close must tolerate an unopened runner.
	Open(ctx context.Context) error
	// Run performs one extraction/review/summarize pass. systemPrompt is the trusted
	// witness instruction; input is the UNTRUSTED corpus (see buildRunCmd fencing).
	Run(ctx context.Context, model, systemPrompt, input string) (string, error)
	// Close releases engine resources and, for a session-persisting engine, sweeps
	// witness's own distill sessions so they are never re-ingested as user sessions.
	Close() error
}

// NewRunner resolves the global distillation runner from cfg. This is the single
// place the runner name is switched on — the former RunWith duplicate is gone.
// An unknown name fails closed (never silently defaults) so a typo surfaces.
func NewRunner(cfg store.Config) (Runner, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Runner)) {
	case "", "claude":
		return &claudeRunner{}, nil
	case "opencode":
		return &openCodeRunner{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("unknown distillation runner %q (want claude or opencode)", cfg.Runner)
	}
}

// claudeRunner shells out to `claude -p` per Run; there is no persistent server,
// so Open/Close are no-ops and nothing is persisted to clean up (the nested run
// uses --no-session-persistence and the WITNESS_WORKER=1 recursion guard).
type claudeRunner struct{}

func (*claudeRunner) Open(context.Context) error { return nil }

func (*claudeRunner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	return runClaude(ctx, model, systemPrompt, input)
}

func (*claudeRunner) Close() error { return nil }

// openCodeRunner reuses ONE `opencode serve` process across every Run in a drain
// (vs. the old runOpenCode, which started a server per call). Open sweeps stale
// distill sessions then starts the server; Close stops it and sweeps again — the
// pairing the worker used to hand-assemble and review.go got wrong.
type openCodeRunner struct {
	cfg    store.Config
	server *OpenCodeServer
}

func (r *openCodeRunner) Open(ctx context.Context) error {
	cleanupOpenCodeDistillSessions(ctx, time.Now().Add(-1*time.Hour))
	srv, err := StartOpenCodeServer(ctx, r.cfg.TriageModel, r.cfg.DistillModel)
	if err != nil {
		return err
	}
	r.server = srv
	return nil
}

func (r *openCodeRunner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if r.server == nil {
		return "", fmt.Errorf("opencode runner: Run called before Open")
	}
	return r.server.Run(ctx, model, systemPrompt, input)
}

func (r *openCodeRunner) Close() error {
	if r.server == nil {
		return nil // never opened (e.g. no work this drain) — nothing to stop or sweep
	}
	err := r.server.Close()
	// Post-cleanup uses a detached context so it still runs when the drain's ctx is
	// already done; the small +time.Second window covers a session created moments
	// before Close.
	cleanupOpenCodeDistillSessions(context.Background(), time.Now().Add(time.Second))
	return err
}

// cleanupOpenCodeDistillSessions removes witness's own distill sessions from the
// OpenCode DB so they aren't re-ingested as user sessions. Moved here from
// cmd/commands/worker.go so BOTH the worker and the manual review path get it via
// Runner.Close() — no call site can omit it (that omission was the review.go leak).
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
