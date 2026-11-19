package commands

import (
	"context"
	"log/slog"
	"maps"
	"slices"
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
	lenses   []*lens.Lens             // the active lenses this drain will run
	byName   map[string]*openedRunner // runtime name → lazily-opened runner (or a broken marker)
	defaultR string                   // the resolved default runner name (the "" / default routing target)
	gate     *usageGate               // pauses a runtime's shared-allowance calls after a refusal (usagepause.go)
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
// Each runner is minted with a cfg carrying the models that runtime should VALIDATE at Open:
// the union of every model its lenses will ask it for, computed by runtimeModels. This was
// NOT IMPLEMENTED in #148 (the comment claimed it but nothing computed a union), and deliberately
// left out — but #232 (OpenCode v2, #218) reversed that decision because two things need it:
//  1. On the OpenCode runner, an empty stage model is an error at Open, so the
//     union must include "" when a cross-runtime lens has no per-lens model.
//  2. OPENCODE_DISABLE_FREE_MODELS=1 unless an opencode/* model is configured, which requires
//     knowing whether ANY lens will use a free model.
//
// The plumbing (store.Config.RunnerModels, a slice) already existed; runtimeModels fills it.
func newRunnerSet(ctx context.Context, st *store.Store, cfg store.Config, lenses []*lens.Lens) (*runnerSet, error) {
	// Every model call opens through here (worker, review, compact + claims, profile regeneration),
	// so this is the one place to refuse an archive a newer witness wrote — before a call is paid
	// for, not at the facet write after it (see store.ErrArchiveNewer).
	if err := st.ArchiveNewer(); err != nil {
		return nil, err
	}
	rs := &runnerSet{cfg: cfg, lenses: lenses, byName: map[string]*openedRunner{}, defaultR: strings.TrimSpace(cfg.Runner), gate: newUsageGate(st)}

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
		rs.openRuntime(ctx, name)
	}
	// Fail closed if the default runner is broken — it's the one runtime the drain can't
	// proceed without (every lens with no runner of its own + the unified summary ride it), and this
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
// The cfg it mints with carries the union of models this runtime should validate at Open —
// every model its lenses will ask it for, computed by runtimeModels.
func (rs *runnerSet) openRuntime(ctx context.Context, name string) {
	rcfg := rs.cfg
	rcfg.RunnerModels = runtimeModels(rs.cfg, rs.lenses, name)

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
	rs.byName[name] = &openedRunner{runner: runner, run: rs.gate.wrap(name, runner, distill.RunnerMine(runner))}
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

// clearCrossRuntimeModels blanks the configured default stage models when `lens try` previews a
// lens on a NON-default runtime, because a model name is only valid on its own runtime: the
// preview's cfg.Runner becomes the lens's runner, so distill.ModelFor would otherwise hand that
// runtime the default runtime's model. The preview then rides the lens's own models or the
// runtime's default. (The drain keeps cfg.Runner the default, where ModelFor already returns ""
// for a cross-runtime lens.)
func clearCrossRuntimeModels(rcfg *store.Config, name, defaultRunner string) {
	if strings.TrimSpace(name) == strings.TrimSpace(defaultRunner) {
		return // keep the configured default models; they belong to this runtime
	}
	rcfg.TriageModel = ""
	rcfg.DistillModel = ""
}

// runtimeModels returns the union of models a given runtime should validate: every model
// its lenses will ask it for, across both stages (extract/review). On the default runtime
// it also includes the cross-lens unified summary's review model. Empty models ("") are
// kept (they represent an unnamed stage on a cross-runtime lens with no per-lens model)
// and the list is deduped and sorted: a runner names the first bad model it meets, so map order
// would make doctor name a different one of two bad models on each run.
func runtimeModels(cfg store.Config, lenses []*lens.Lens, runtime string) []string {
	seen := map[string]bool{}
	defaultRunner := strings.TrimSpace(cfg.Runner)
	isDefault := strings.TrimSpace(runtime) == defaultRunner

	// Add both stages of every lens routed to this runtime.
	for _, ln := range lenses {
		if distill.RunnerFor(cfg, ln) != strings.TrimSpace(runtime) {
			continue
		}
		seen[distill.ModelFor(cfg, ln, distill.PhaseExtract)] = true
		seen[distill.ModelFor(cfg, ln, distill.PhaseReview)] = true
	}

	// On the default runtime, add the cross-lens unified summary's review model.
	if isDefault {
		seen[distill.ModelFor(cfg, nil, distill.PhaseReview)] = true
	}

	return slices.Sorted(maps.Keys(seen))
}
