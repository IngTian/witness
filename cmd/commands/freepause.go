package commands

import (
	"context"
	"errors"
	"log/slog"
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
// seconds later), so T is never trusted to release a burst: after T exactly one call probes. A
// refusal re-arms the pause with its own T, a success clears it, and any other failure keeps it
// and lets the next call probe again.
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
	st      store.MetaKV
	now     func() time.Time
	mu      sync.Mutex // serializes probing and each meta read-decide-write: the drain runs up to 16 miners
	probing bool
}

func newFreeGate(st store.MetaKV) *freeGate { return &freeGate{st: st, now: time.Now} }

// wrap gates the calls run makes to r's free models. It returns run unchanged when r has no free
// allowance, and passes any non-free model straight through.
func (g *freeGate) wrap(runtime string, r platform.Runner, run distill.MineFunc) distill.MineFunc {
	fa, ok := r.(platform.FreeAllowance)
	if !ok {
		return run
	}
	return func(ctx context.Context, model, prompt, input string) (string, error) {
		if !fa.OnFreeModel(model) {
			return run(ctx, model, prompt, input)
		}
		probe, err := g.admit(runtime)
		if err != nil {
			return "", err
		}
		out, err := run(ctx, model, prompt, input) // outside mu: a generation runs for minutes
		g.settle(probe, err)
		return out, err
	}
}

// admit: unarmed → run; armed and before T, or past T with the probe already in flight →
// refused with the stored T, never reaching the runner; armed and past T → run as the one probe.
func (g *freeGate) admit(runtime string) (probe bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, armed := freeModelsPausedUntil(g.st)
	if !armed {
		return false, nil
	}
	if g.now().Before(until) || g.probing {
		return false, &platform.QuotaExhaustedError{ResetAt: until, Runtime: runtime}
	}
	g.probing = true
	return true, nil
}

func (g *freeGate) settle(probe bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if probe {
		g.probing = false
	}
	var qe *platform.QuotaExhaustedError
	switch {
	case errors.As(err, &qe):
		until := qe.ResetAt.UTC().Format(time.RFC3339)
		// One line per pause: calls already in flight when the first refusal lands come back
		// refused too, and they find it armed.
		if prev, armed := freeModelsPausedUntil(g.st); !armed || !g.now().Before(prev) {
			slog.Warn("distill: the free model allowance is used up; pausing every free-model call until the reset (then one call is tried)", "until", until)
		}
		if werr := g.st.SetMetaString(freeModelsPausedKey, until); werr != nil {
			slog.Error("distill: could not store the free-model pause; free-model calls are not paused", "until", until, "err", werr)
		}
	case probe && err == nil:
		_ = g.st.SetMetaString(freeModelsPausedKey, "")
		slog.Info("distill: the free model allowance is back; free-model calls resume")
	}
}
