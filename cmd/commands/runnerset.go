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
// opened ONE default runner; a lens may now declare its own runtime, so a drain opens the
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
	cfg      store.Config
	byName   map[string]*openedRunner // runtime name → lazily-opened runner (or a broken marker)
	defaultR string                   // the resolved default runner name (the "" / default routing target)
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
// down. It DOES return an error only if the default runner itself can't even be resolved
// (a config typo), matching slice-1's fail-closed behavior.
//
// Each runner is minted with a cfg carrying the models that runtime should VALIDATE at Open: the
// configured default stage models for the default runtime, and none for a non-default one (see
// clearCrossRuntimeModels).
//
// It does NOT validate the union of that runtime's per-lens models, and the comment here used to
// claim it did (#148). Nothing computed such a union — the parameter that was supposed to carry the
// lenses was declared and never read, so the claim described behavior that never existed, which is
// worse than an absent feature because it stops anyone from looking.
//
// NOT IMPLEMENTED DELIBERATELY, rather than left as a TODO. store.Config carries exactly two model
// slots (TriageModel, DistillModel), so a real union needs a new Config field threaded through
// every runner — and the only thing it buys is EARLIER failure for a mistyped per-lens model.
// That model is already validated when it is first used: the run fails, the lens backs off, and the
// error names the model. Trading a new config field plus its migration for moving one error message
// earlier is not worth it. If per-lens models ever become common enough that a fail-at-Open check
// pays for itself, StartOpenCodeServerIn already accepts variadic models — the plumbing is there.
func newRunnerSet(ctx context.Context, st *store.Store, cfg store.Config, lenses []*lens.Lens) (*runnerSet, error) {
	rs := &runnerSet{cfg: cfg, byName: map[string]*openedRunner{}, defaultR: strings.TrimSpace(cfg.Runner)}

	// Which runtimes are actually needed, and the model union per runtime.
	needed := map[string]bool{}
	for _, ln := range lenses {
		needed[distill.RunnerFor(cfg, ln)] = true
	}
	// The default runner is always potentially needed (the unified summary + any lens with
	// no explicit runner route there). Include it so it's opened even if, say, every
	// enabled lens declared opencode but the built-in default still rides the default.
	needed[rs.defaultR] = true

	for name := range needed {
		rs.openRuntime(ctx, st, name)
	}
	// Fail closed if the default runner is broken — it's the one runtime the drain can't
	// proceed without (the always-on default lens + the unified summary ride it), and this
	// matches slice-1's behavior where a failed default-runner Open returned an error. A
	// non-default per-lens runtime being broken is NOT fatal: it's circuit-broken and only
	// its own lenses back off, so a healthy Claude drain isn't wedged by a down OpenCode.
	if g := rs.byName[rs.defaultR]; g == nil || g.runner == nil {
		// We're returning nil (so the caller's `defer rs.Close()` never runs) — close every
		// runtime we DID open here, or a successfully-opened opencode serve would leak when
		// the default runner is what failed. Map iteration is unordered, so OpenCode may have
		// opened before the default was even checked.
		rs.Close()
		if g != nil && g.err != nil {
			return nil, g.err
		}
		return nil, &runnerDownError{name: rs.defaultR}
	}
	return rs, nil
}

// openRuntime mints + Opens one runtime, recording either its MineFunc or a broken marker.
// The cfg it mints with carries only the models this runtime should validate at Open — the
// configured defaults for the default runtime, none for a cross-runtime one.
// st is currently UNREAD here, and that is a deliberate keep rather than an oversight: openRuntime
// resolves via platform.RunnerForName (name + cfg), while the sibling entry point platform.RunnerFor
// takes a store.RunnerResolver to apply the runner-precedence ladder. Any per-runtime resolution that
// needs the store lands here. Dropping it would touch six call sites to save one parameter, and the
// judgement is that the symmetry with RunnerFor is worth more than the line — unlike the lenses
// parameter removed in #148, which advertised a model UNION that was never computed.
func (rs *runnerSet) openRuntime(ctx context.Context, st *store.Store, name string) {
	rcfg := rs.cfg
	rcfg.Runner = name
	clearCrossRuntimeModels(&rcfg, name, rs.defaultR)

	runner, err := platform.RunnerForName(name, rcfg)
	if err != nil {
		// Resolve failure (unknown runner name). Record broken; newRunnerSet decides whether
		// it's fatal (default) or per-lens-backoff (a lens's bad `# runner`).
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

func (rs *runnerSet) NativeFor(ln *lens.Lens) bool {
	or := rs.byName[distill.RunnerFor(rs.cfg, ln)]
	if or == nil || or.runner == nil {
		return false
	}
	s, ok := or.runner.(platform.NativeSessionSupport)
	return ok && s.SupportsNativeSession()
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
// For the default runtime it keeps the configured default models (they belong to it). For a
// NON-default runtime the configured default models are for the wrong runtime, so it clears them
// (the runner uses its own default) — per-lens models are validated separately by the
// runner's own Open against the union we pass via ValidateModels in doctor; here we only
// need Open's prewarm to not choke on a wrong-runtime default model. The per-lens models
// themselves are passed per-call (ModelFor), and OpenCode's server accepts any configured
// model per-call, so prewarming the default models is sufficient for Open to succeed.
// clearCrossRuntimeModels blanks the configured default stage models when minting a runner for a
// NON-default runtime, because a model name is only valid on its own runtime: passing the
// Claude-side default to an OpenCode runner would make its Open-time validation reject a name that
// was never meant for it. The runner then falls back to its own default, and per-lens models are
// supplied per call.
//
// Renamed from applyModelUnion, which took a `lenses []*lens.Lens` parameter it never referenced
// (#148). The name and signature both advertised a union this function has never computed; nothing
// is lost by dropping them, and a caller can no longer believe passing lenses here achieves
// anything.
func clearCrossRuntimeModels(rcfg *store.Config, name, defaultRunner string) {
	if strings.TrimSpace(name) == strings.TrimSpace(defaultRunner) {
		return // keep the configured default models; they belong to this runtime
	}
	rcfg.TriageModel = ""
	rcfg.DistillModel = ""
}
