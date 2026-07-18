package platform

import (
	"context"
	"strings"

	"github.com/IngTian/witness/internal/store"
)

// Runner is the default distillation-engine lifecycle: one engine drains every
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
	// ConcurrentRunSafe reports whether it is safe for the engine to call Run
	// concurrently (several sessions mining at once) against this runner. This is a
	// platform FACT (mechanism), NOT a policy: the engine owns the pool size and the
	// ceiling; the platform only states whether overlap is safe at all. Both runtimes
	// are true today: Claude — each Run is an independent `claude -p` process sharing
	// nothing; OpenCode — Run holds its mutex only for a closed-check and drives an
	// isolated session per call over the shared serve process, which a benchmark showed
	// accepts many concurrent sessions (issue #22 narrowed the mutex to flip this true).
	ConcurrentRunSafe() bool
}

// RunnerProvider is the Platform capability that builds this platform's Runner.
// Kept separate from Runner so a Platform value (a stateless registry entry) mints
// a fresh, cfg-bound Runner per drain rather than being one itself.
type RunnerProvider interface {
	NewRunner(cfg store.Config) Runner
}

// SweepsOnCloser is the OPTIONAL capability of a Runner whose Open/Close runs a
// PROCESS-GLOBAL cleanup sweep — one that reaches beyond this runner's own work and
// can disturb a concurrently-running witness worker. The OpenCode runner implements
// it (true): its Close() calls cleanupDistillSessions, deleting witness-distill
// sessions from the shared OpenCode DB, which would delete a background worker's
// in-flight distill session. The Claude runner does NOT implement it (each Run is an
// isolated `claude -p` process; Close is a no-op).
//
// This is a SEPARATE axis from ConcurrentRunSafe: that says "can the engine call Run
// on THIS runner concurrently" (true for both today); SweepsOnClose says "does
// closing this runner touch OTHER processes' state". A read-only tool that opens its
// own runner alongside a possible background worker (e.g. `witness lens try`) must
// hold the single-flight WorkerLock while a sweeping runner is open, but needs no
// lock for a non-sweeping one.
type SweepsOnCloser interface {
	SweepsOnClose() bool
}

// RunnerSweepsOnClose reports whether r runs a process-global sweep on Open/Close —
// nil-safe: a runner that doesn't implement SweepsOnCloser is treated as false (no
// sweep). Centralizing the type assertion here means callers can't accidentally use
// the wrong predicate (ConcurrentRunSafe) or mis-handle the not-implemented case.
func RunnerSweepsOnClose(r Runner) bool {
	s, ok := r.(SweepsOnCloser)
	return ok && s.SweepsOnClose()
}

// RunnerFor resolves the default runner for a drain. It applies the store's runner
// precedence (bound-meta > config line > WITNESS_RUNNER env > default — unchanged)
// to get ONE name, then mints that platform's Runner. Fails closed on an unknown
// name so a typo surfaces instead of silently defaulting.
//
// This is deliberately independent of ForSession: a Claude-runner user distilling
// imported OpenCode sessions resolves RunnerFor=Claude (shells to claude -p) while
// each session's ForSession=OpenCode still shapes its input. One engine, per-source
// input shaping — the two axes never derive from each other.
func RunnerFor(st store.RunnerResolver, cfg store.Config) (Runner, error) {
	return RunnerForName(strings.TrimSpace(st.ResolveRunner(cfg)), cfg)
}

// RunnerForName mints the Runner for an explicit runner NAME, applying the same
// empty→default + fail-closed-on-typo rules as RunnerFor. It is the per-lens-runner seam
// (issue #75 slice 2): the worker resolves each active lens's runtime (distill.RunnerFor)
// and mints one runner per distinct name via this, so lenses on different runtimes each
// get their own Runner. cfg is passed through for the runner's model prewarm/validate; a
// caller with per-lens models on this runtime should pass a cfg carrying the union (see
// the worker's per-runtime open).
func RunnerForName(name string, cfg store.Config) (Runner, error) {
	name = strings.TrimSpace(name)
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
