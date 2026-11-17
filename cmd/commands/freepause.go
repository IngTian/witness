package commands

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The free-model pause (spec §7.7, anti-goal "Burning the free allowance").
//
// OpenCode Zen counts free usage per NETWORK, not per session, and one refusal holds until it
// resets (evidence freetier §4). So after the first provider.free-limit refusal every other call
// to a free model — mine, review, claims, compaction, profile — would be refused too, and
// retrying into it per session only spends the allowance the moment it comes back. The gate
// wraps each runtime's MineFunc (runnerSet, and `lens try`, which opens its own runner), the one
// place every model call opens through, so nothing reaches a free model until the reset.
//
// Reset times are unreliable (freetier §4 Reliability: a "7h25m" refusal answered by a success
// seconds later), so T is never trusted to release a burst: after T exactly one call probes, and
// calls arriving meanwhile wait for its verdict. A refusal re-arms the pause with its own T, a
// success clears it, and any other failure (a panic included) keeps it and lets the next call probe.
//
// T lives in meta so the pause spans the short-lived worker processes and `lens try`.
// ponytail: `probing` is per process, so two processes past T may each probe once; a meta CAS
// would make it one if concurrent probing processes ever matter.
const freeModelsPausedKey = "free_models_paused_until" // RFC3339 UTC

// freeModelsPausedUntil reads the stored reset time; false when unset (or unparseable).
func freeModelsPausedUntil(st store.MetaKV) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, st.MetaString(freeModelsPausedKey))
	return t, err == nil
}

type freeGate struct {
	st        store.MetaKV
	now       func() time.Time
	mu        sync.Mutex // serializes probing and each meta read-decide-write: the drain runs up to 16 miners
	probing   bool
	probeDone chan struct{} // closed when the in-flight probe settles; its waiters then re-admit
	key       string        // empty keeps the legacy OpenCode network allowance key
	planGates map[string]*freeGate
}

func runnerPauseKey(runtime string) string { return platform.UsagePauseKey(runtime) }

func (g *freeGate) pauseKey() string {
	if g.key != "" {
		return g.key
	}
	return freeModelsPausedKey
}

func (g *freeGate) pausedUntil() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, g.st.MetaString(g.pauseKey()))
	return t, err == nil
}

// Every runner login gets its own pause and probe election. The legacy free-model
// key continues to represent OpenCode's network-wide allowance across models.
func runnerUsagePauses(st store.MetaKV) map[string]time.Time {
	pauses := map[string]time.Time{}
	for _, runtime := range platform.RunnerNames() {
		if at, err := time.Parse(time.RFC3339, st.MetaString(runnerPauseKey(runtime))); err == nil {
			pauses[runtime] = at
		}
	}
	return pauses
}

func newFreeGate(st store.MetaKV) *freeGate { return &freeGate{st: st, now: time.Now} }

// errRunPanicked is how a panicking call settles: a non-quota failure, so the key stays armed and
// the next call probes.
var errRunPanicked = errors.New("free-model call panicked")

// wrap gates the calls run makes to r's free models. It returns run unchanged when r has no free
// allowance, and passes any non-free model straight through.
func (g *freeGate) wrap(runtime string, r platform.Runner, run distill.MineFunc) distill.MineFunc {
	var uses func(string) bool
	if plan, ok := r.(platform.PlanAllowance); ok {
		g.mu.Lock()
		if g.planGates == nil {
			g.planGates = map[string]*freeGate{}
		}
		gate := g.planGates[runtime]
		if gate == nil {
			gate = &freeGate{st: g.st, now: g.now, key: runnerPauseKey(runtime)}
			g.planGates[runtime] = gate
		}
		g.mu.Unlock()
		return gate.wrapAllowance(runtime, plan.OnPlanAllowance, run)
	}
	if fa, ok := r.(platform.FreeAllowance); ok {
		uses = fa.OnFreeModel
	} else {
		return run
	}
	return g.wrapAllowance(runtime, uses, run)
}

func (g *freeGate) wrapAllowance(runtime string, uses func(string) bool, run distill.MineFunc) distill.MineFunc {
	return func(ctx context.Context, model, prompt, input string) (out string, err error) {
		if !uses(model) {
			return run(ctx, model, prompt, input)
		}
		probe, aerr := g.admit(ctx, runtime)
		if aerr != nil {
			return "", aerr
		}
		// Settle in a defer so a panicking probe still ends: the drain recovers miner panics, and
		// every later free call would otherwise wait on a probeDone that never closes. err holds
		// errRunPanicked until run returns, so a panic settles as a non-quota failure and goes on
		// propagating.
		err = errRunPanicked
		defer func() { g.settle(probe, err) }()
		return run(ctx, model, prompt, input) // outside mu: a generation runs for minutes
	}
}

// admit: unarmed → run; armed and before T → refused with the stored T, never reaching the
// runner; armed and past T → run as the one probe. A call arriving while the probe is in flight
// WAITS for its verdict and re-admits, rather than being refused: a refusal skips its session for
// the rest of the drain, and a probe that succeeds arms no wakeup, so the reset would resume only
// the probe's own session and strand the rest until an unrelated trigger.
func (g *freeGate) admit(ctx context.Context, runtime string) (probe bool, err error) {
	for {
		g.mu.Lock()
		until, armed := g.pausedUntil()
		switch {
		case !armed:
			g.mu.Unlock()
			return false, nil
		case g.now().Before(until):
			g.mu.Unlock()
			return false, &platform.QuotaExhaustedError{ResetAt: until, Runtime: runtime}
		case !g.probing:
			g.probing, g.probeDone = true, make(chan struct{})
			g.mu.Unlock()
			return true, nil
		}
		done := g.probeDone
		g.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (g *freeGate) settle(probe bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if probe {
		// Waiters re-admit only after mu is released, so they read the verdict written below.
		g.probing = false
		close(g.probeDone)
	}
	var qe *platform.QuotaExhaustedError
	scope, calls := "free model", "free-model"
	if g.key != "" {
		scope = strings.TrimPrefix(g.key, "runner_usage_paused_until:") + " plan"
		calls = scope
	}
	switch {
	case errors.As(err, &qe) && qe.ResetAt.After(g.now()):
		until := qe.ResetAt.UTC().Format(time.RFC3339)
		// One line per pause: calls already in flight when the first refusal lands come back
		// refused too, and they find it armed.
		if prev, armed := g.pausedUntil(); !armed || !g.now().Before(prev) {
			slog.Warn("distill: the "+scope+" allowance is used up; pausing every "+calls+" call until the reset (then one call is tried)", "until", until)
		}
		if werr := g.st.SetMetaString(g.pauseKey(), until); werr != nil {
			slog.Error("distill: could not store the "+calls+" pause; calls are not paused", "until", until, "err", werr)
		}
	case probe && err == nil:
		if werr := g.st.SetMetaString(g.pauseKey(), ""); werr != nil {
			slog.Error("distill: could not clear the "+calls+" pause; the next call probes again", "err", werr)
			return
		}
		slog.Info("distill: the " + scope + " allowance is back; calls resume")
	}
}
