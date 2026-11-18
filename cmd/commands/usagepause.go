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

// The usage pause (#218/#232, #236).
//
// Some calls draw on an allowance one refusal exhausts for every such call on the runtime until
// it resets: OpenCode Zen counts free usage per NETWORK, and a Codex plan login is one quota. So
// after the first QuotaExhaustedError every other call on it — mine, review, claims, compaction,
// profile — would be refused too, and retrying into it per session only spends the allowance the
// moment it comes back. The gate wraps each runtime's MineFunc (runnerSet, and `lens try`, which
// opens its own runner), the one place every model call opens through, so nothing reaches a
// paused allowance until the reset. witness never checks the user's plan: a pause only reacts
// to the tool's own refusal, and the runner's SharedAllowance says which calls it covers.
//
// T lives in meta, one key per runtime, so the pause spans the short-lived worker processes and
// `lens try`; this gate is its only writer. Reset times are unreliable (OpenCode #59387: a "7h25m"
// refusal answered by a success seconds later), and nothing probes them: after T every call runs;
// the first refusal re-arms; a false reset costs ≤ mine_concurrency refused calls. A past T reads
// as unarmed everywhere, so a pause is never cleared.
// ponytail: one pause per runtime; key by (runtime, allowance) if a runtime ever gets two.
func usagePauseKey(rt string) string { return "runner_usage_paused_until:" + rt } // RFC3339 UTC; v0.14.0's spelling

// usagePausedUntil reads rt's stored reset; true only for a future T (unset, unparseable and past
// all read as unarmed).
func usagePausedUntil(st store.MetaKV, rt string, now time.Time) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, st.MetaString(usagePauseKey(rt)))
	return t, err == nil && t.After(now)
}

// usagePauses is every registered runtime's armed pause.
func usagePauses(st store.MetaKV, now time.Time) map[string]time.Time {
	pauses := map[string]time.Time{}
	for _, rt := range platform.RunnerNames() {
		if until, ok := usagePausedUntil(st, rt, now); ok {
			pauses[rt] = until
		}
	}
	return pauses
}

type usageGate struct {
	st  store.MetaKV
	now func() time.Time
	mu  sync.Mutex // serializes a refusal's read-decide-write: the drain runs up to 16 miners
}

func newUsageGate(st store.MetaKV) *usageGate { return &usageGate{st: st, now: time.Now} }

// wrap gates the calls run makes to r's shared allowance. It returns run unchanged when r has
// none, and passes any call off the allowance straight through.
func (g *usageGate) wrap(rt string, r platform.Runner, run distill.MineFunc) distill.MineFunc {
	sa, ok := r.(platform.SharedAllowance)
	if !ok {
		return run
	}
	return func(ctx context.Context, model, prompt, input string) (string, error) {
		if !sa.OnSharedAllowance(model) {
			return run(ctx, model, prompt, input)
		}
		if until, paused := usagePausedUntil(g.st, rt, g.now()); paused {
			return "", &platform.QuotaExhaustedError{ResetAt: until, Runtime: rt}
		}
		out, err := run(ctx, model, prompt, input) // outside mu: a generation runs for minutes
		var qe *platform.QuotaExhaustedError
		if errors.As(err, &qe) && qe.ResetAt.After(g.now()) {
			g.mu.Lock()
			defer g.mu.Unlock()
			until := qe.ResetAt.UTC().Format(time.RFC3339)
			// One line per pause: calls already in flight when the first refusal lands come back
			// refused too, and they find it armed.
			if _, armed := usagePausedUntil(g.st, rt, g.now()); !armed {
				slog.Warn("distill: the "+rt+" allowance is used up; pausing its calls until the reset", "until", until)
			}
			if werr := g.st.SetMetaString(usagePauseKey(rt), until); werr != nil {
				slog.Error("distill: could not store the "+rt+" pause; calls are not paused", "until", until, "err", werr)
			}
		}
		return out, err
	}
}
