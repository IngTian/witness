package platform

import (
	"context"
	"strings"

	"github.com/IngTian/witness/internal/store"
)

// Runner is the GLOBAL distillation-engine lifecycle: one engine drains every
// pending session regardless of which platform produced it. It is the whole of
// what the engine (internal/distill) knows about a runtime — distill calls Run and
// never learns whether that shells to `claude -p` or talks to `opencode serve`.
// This is the axis resolved by RunnerFor (by cfg.Runner), the counterpart to
// ForSession (the per-session owning platform).
type Runner interface {
	// Open acquires whatever the engine needs to serve Run (OpenCode: a pre-cleanup
	// sweep + `opencode serve`; Claude: nothing). Callers may skip Open when there is
	// no work; Close must tolerate an unopened runner.
	Open(ctx context.Context) error
	// Run performs one mining/review/summarize pass. systemPrompt is witness's own
	// instruction; input is the corpus being analyzed — the platform fences it with
	// WrapCorpus so it cannot impersonate instructions.
	Run(ctx context.Context, model, systemPrompt, input string) (string, error)
	// Close releases engine resources and, for a session-persisting engine, sweeps
	// witness's own distill sessions so they are never re-ingested as user sessions.
	Close() error
	// ValidateModels reports whether the configured models are usable by this engine
	// (feeds `witness doctor`). Claude is a no-op; OpenCode checks its model list.
	ValidateModels(ctx context.Context, models ...string) error
	// InvocationHint is a short human string naming how this engine runs, for
	// doctor/diagnostics (e.g. "claude -p" / "opencode serve").
	InvocationHint() string
}

// RunnerProvider is the Platform capability that builds this platform's Runner.
// Kept separate from Runner so a Platform value (a stateless registry entry) mints
// a fresh, cfg-bound Runner per drain rather than being one itself.
type RunnerProvider interface {
	NewRunner(cfg store.Config) Runner
}

// RunnerFor resolves the GLOBAL runner for a drain. It applies the store's runner
// precedence (bound-meta > config line > WITNESS_RUNNER env > default — unchanged)
// to get ONE name, then mints that platform's Runner. Fails closed on an unknown
// name so a typo surfaces instead of silently defaulting.
//
// This is deliberately independent of ForSession: a Claude-runner user distilling
// imported OpenCode sessions resolves RunnerFor=Claude (shells to claude -p) while
// each session's ForSession=OpenCode still shapes its input. One engine, per-source
// input shaping — the two axes never derive from each other.
func RunnerFor(st *store.Store, cfg store.Config) (Runner, error) {
	name := strings.TrimSpace(st.ResolveRunner(cfg))
	// An empty runner means "unset" — fall back to the default platform (Claude),
	// matching the config default and the old NewRunner's `case "", "claude"`. A
	// NON-empty but unrecognized name still fails closed (a real typo).
	var p Platform
	var ok bool
	if name == "" {
		p = Default()
	} else if p, ok = ByName(name); !ok {
		return nil, unknownRunnerError(name)
	}
	rp, ok := p.(RunnerProvider)
	if !ok {
		return nil, unknownRunnerError(name)
	}
	return rp.NewRunner(cfg), nil
}
