package commands

import (
	"context"
	"log/slog"
	"strings"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// runnerSet is the per-lens-runner lifecycle for one drain (issue #75 slice 2). Slice 1
// opened ONE global runner; a lens may now declare its own runtime, so a drain opens the
// SET of runners the active lenses actually need — each ONCE, lazily, under the worker's
// single WorkerLock. It hands the engine a per-lens resolver (RunFor) so mine/review pick
// the right runner, and a combined Close that tears every opened runner down.
//
// Per-runtime CIRCUIT BREAKER: if a runtime's Open fails (e.g. `opencode serve` won't
// start, or its provider balance is exhausted), that runtime is marked broken ONCE and
// every lens routed to it falls back to a no-op MineFunc that returns the Open error — so
// its lenses back off per-session-lens exactly like any transport failure, in ONE decision
// instead of N identical Open attempts. Lenses on a healthy runtime commit normally. This
// is the "an OpenCode outage doesn't touch Claude lenses" isolation.
type runnerSet struct {
	cfg     store.Config
	byName  map[string]*openedRunner // runtime name → lazily-opened runner (or a broken marker)
	globalR string                   // the resolved global runner name (the "" / default routing target)
}

type openedRunner struct {
	runner platform.Runner // nil if Open failed (broken)
	run    distill.MineFunc
	err    error // non-nil if this runtime is broken (Open failed)
}

// newRunnerSet resolves the distinct runtimes the active lenses need and OPENS each one
// (once). ctx threads to each Open (so SIGTERM tears them down). It never returns a fatal
// error for a single runtime's Open failure — that runtime is circuit-broken and its
// lenses back off — so a drain with a healthy Claude lens still proceeds when OpenCode is
// down. It DOES return an error only if the global runner itself can't even be resolved
// (a config typo), matching slice-1's fail-closed behavior.
//
// Each runner is minted with a cfg carrying the UNION of that runtime's per-lens models
// (plus the globals for the global runtime), so an OpenCode runner prewarms/validates
// every model its lenses will actually use — not just the two global stage models.
func newRunnerSet(ctx context.Context, st *store.Store, cfg store.Config, lenses []*lens.Lens) (*runnerSet, error) {
	rs := &runnerSet{cfg: cfg, byName: map[string]*openedRunner{}, globalR: strings.TrimSpace(cfg.Runner)}

	// Which runtimes are actually needed, and the model union per runtime.
	needed := map[string]bool{}
	for _, ln := range lenses {
		needed[distill.RunnerFor(cfg, ln)] = true
	}
	// The global runner is always potentially needed (the unified summary + any lens with
	// no explicit runner route there). Include it so it's opened even if, say, every
	// enabled lens declared opencode but the built-in default still rides the global.
	needed[rs.globalR] = true

	for name := range needed {
		rs.openRuntime(ctx, st, name, lenses)
	}
	// Fail closed if the GLOBAL runner is broken — it's the one runtime the drain can't
	// proceed without (the always-on default lens + the unified summary ride it), and this
	// matches slice-1's behavior where a failed global-runner Open returned an error. A
	// NON-global per-lens runtime being broken is NOT fatal: it's circuit-broken and only
	// its own lenses back off, so a healthy Claude drain isn't wedged by a down OpenCode.
	if g := rs.byName[rs.globalR]; g == nil || g.runner == nil {
		if g != nil && g.err != nil {
			return nil, g.err
		}
		return nil, &runnerDownError{name: rs.globalR}
	}
	return rs, nil
}

// openRuntime mints + Opens one runtime, recording either its MineFunc or a broken marker.
// The cfg it mints with carries this runtime's model union so the runner validates every
// model its lenses use.
func (rs *runnerSet) openRuntime(ctx context.Context, st *store.Store, name string, lenses []*lens.Lens) {
	rcfg := rs.cfg
	rcfg.Runner = name
	applyModelUnion(&rcfg, name, rs.globalR, lenses)

	runner, err := platform.RunnerForName(name, rcfg)
	if err != nil {
		// Resolve failure (unknown runner name). Record broken; newRunnerSet decides whether
		// it's fatal (global) or per-lens-backoff (a lens's bad `# runner`).
		slog.Error("resolve runner", "runner", name, "err", err)
		rs.byName[name] = &openedRunner{err: err}
		return
	}
	if err := runner.Open(ctx); err != nil {
		// Circuit-break: this runtime is down. Its lenses will back off via the no-op run.
		slog.Error("open runner", "runner", name, "err", err)
		rs.byName[name] = &openedRunner{err: err}
		return
	}
	rs.byName[name] = &openedRunner{runner: runner, run: distill.RunnerMine(runner)}
}

// RunFor is the per-lens MineFunc resolver handed to the engine. A lens on a broken
// runtime gets a MineFunc that always returns that runtime's Open error, so mining it
// records a transport-style failure → per-(session,lens) backoff, without ever touching a
// healthy runtime's lenses.
func (rs *runnerSet) RunFor(ln *lens.Lens) distill.MineFunc {
	name := distill.RunnerFor(rs.cfg, ln)
	or := rs.byName[name]
	if or == nil { // a runtime we somehow didn't open — treat as broken
		return brokenRun(name, nil)
	}
	if or.run != nil {
		return or.run
	}
	return brokenRun(name, or.err)
}

// concurrentRunSafe is the AND across opened runners: the engine's single session-window
// cap must be safe for EVERY runtime a session might touch (a session's lenses run
// serially within one goroutine, but different sessions run in parallel and may each hit
// any runtime). Both runtimes are true today, so this is 16-way as before; the AND keeps
// it correct if a future runtime is added that isn't concurrency-safe.
func (rs *runnerSet) concurrentRunSafe() bool {
	safe := true
	for _, or := range rs.byName {
		if or.runner != nil && !or.runner.ConcurrentRunSafe() {
			safe = false
		}
	}
	return safe
}

// Close tears down every opened runner (each Close runs its own post-cleanup sweep). A
// broken runtime (never opened) has nothing to close.
func (rs *runnerSet) Close() {
	for _, or := range rs.byName {
		if or.runner != nil {
			_ = or.runner.Close()
		}
	}
}

// brokenRun is the no-op MineFunc for a circuit-broken runtime: it returns the Open error
// so the engine records a mine failure and backs the (session,lens) off, without any LLM
// call. A nil err (shouldn't happen) still yields a clear message.
func brokenRun(name string, err error) distill.MineFunc {
	return func(context.Context, string, string, string) (string, error) {
		if err != nil {
			return "", err
		}
		return "", &runnerDownError{name: name}
	}
}

type runnerDownError struct{ name string }

func (e *runnerDownError) Error() string {
	return "runner " + e.name + " is unavailable this drain (open failed)"
}

// applyModelUnion sets rcfg's TriageModel/DistillModel to models valid on runtime `name`.
// For the GLOBAL runtime it keeps the configured globals (they belong to it). For a
// NON-global runtime the configured globals are for the wrong runtime, so it clears them
// (the runner uses its own default) — per-lens models are validated separately by the
// runner's own Open against the union we pass via ValidateModels in doctor; here we only
// need Open's prewarm to not choke on a wrong-runtime global. The per-lens models
// themselves are passed per-call (ModelFor), and OpenCode's server accepts any configured
// model per-call, so prewarming the globals is sufficient for Open to succeed.
func applyModelUnion(rcfg *store.Config, name, globalRunner string, lenses []*lens.Lens) {
	if strings.TrimSpace(name) == strings.TrimSpace(globalRunner) {
		return // keep the configured globals; they belong to this runtime
	}
	// Non-global runtime: the configured global models are for the wrong runtime. Clear
	// them so Open's prewarm/validate doesn't reject a cross-runtime model name; the
	// runner falls back to its own default and per-lens models are supplied per call.
	rcfg.TriageModel = ""
	rcfg.DistillModel = ""
}
