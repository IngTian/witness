package commands

import (
	"context"
	"log/slog"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Read-time (PULL) profile regeneration — issue #100.
//
// The L4 narrative used to be only pushed: every review regenerated every summary whether or not
// anyone would ever read it. #100 added regeneration on read, but the push stayed (and #195
// chained the sentence-source resolver onto it), so BOTH paths regenerate: the worker rebuilds
// every stale summary after each review, and `witness profile` rebuilds every stale one before
// printing. MCP get_profile does not: it serves the file as written and notes when Summarize's
// own skip test says the facets moved on (mcpOptions). An unread profile is therefore NOT free:
// each review pays for its rebuild. The push is kept on purpose (#199 F019): it keeps `witness
// view` and the sentence links current, and reads after a review are cached.
//
// Measured on a real `claude -p` (3 sessions, 4 facets, one lens): a cached read is 0.02s and a
// regeneration is 13.3s, acceptable to block an interactive command. A rebuild's input grows
// with the facets, so on a large archive it takes minutes, a wait a read pays only when the push
// has not already refreshed that portrait.
//
// `witness profile <lens>` rewrites EVERY stale portrait, not just the one asked for:
// distill.Summarizer skips each summary whose signature matches, so the cost is exactly the due
// calls. The runner set opens only on the first of them, so a read of current portraits starts
// no model runtime.

// ensureProfileFresh regenerates stale L4 summaries before a read.
//
// A FAILURE IS NOT FATAL to the caller: the reader still serves whatever is on disk, because a
// derived narrative that cannot be rebuilt right now is strictly better served stale than not at
// all. The error is returned so an interactive caller can say so.
//
// The WorkerLock is the load-bearing guard. `witness profile` blocking ~13s on its own
// regeneration is fine; blocking minutes behind somebody else's backfill is not — and two
// processes writing profiles concurrently would race the file and its signature. So a read that
// cannot take the lock serves the cached profile instead of queueing. The lock is the same
// single-consumer lock the worker and forceReviewTapped use, so pull can never race a push.
func ensureProfileFresh(st *store.Store) error {
	unlock, ok := st.WorkerLock()
	if !ok {
		// A drain (or another reader) owns the lock. Serving cached is the whole point of the
		// guard, so this is a normal outcome, not an error.
		slog.Debug("profile: a distillation already holds the worker lock; serving the cached profile")
		return nil
	}
	defer unlock()

	sm, err := profileSummarizer(st)
	// Nothing enabled (sm == nil) → nothing to summarize. Bail before opening a runner: on a
	// fresh archive this is the difference between a read that returns instantly and one that
	// starts (and prewarms) a model runtime to produce nothing.
	if err != nil || sm == nil {
		return err
	}
	// newRunnerSet refuses a newer archive too, but it now opens on the first due call, after
	// Summarize has reaped orphan portraits: an older binary must not delete a newer one's files.
	if err := st.ArchiveNewer(); err != nil {
		return err
	}

	// Opened on the first model call, so a pass whose portraits are all current starts nothing.
	ctx := context.Background()
	var rs *runnerSet
	var rsErr error
	defer func() {
		if rs != nil {
			rs.Close()
		}
	}()
	sm.RunFor = func(ln *lens.Lens) distill.SummarizeFunc {
		if rs == nil && rsErr == nil {
			rs, rsErr = newRunnerSet(ctx, st, sm.Config, sm.Lenses)
		}
		if rsErr != nil {
			return brokenRun(sm.Config.Runner, rsErr)
		}
		return rs.RunFor(ln)
	}
	sm.Run = func(ctx context.Context, m, p, in string) (string, error) { return sm.RunFor(nil)(ctx, m, p, in) }

	// A ~13s silent wait is indistinguishable from a hang, so animate it.
	//
	// Two properties make this safe to start unconditionally, rather than needing to predict
	// whether Summarize will actually call a model:
	//
	//   - The first frame is one tick (90ms) away, and a fully-cached pass returns in ~20ms
	//     (measured). So the fast path prints NOTHING — no frame, no escape codes, no flicker.
	//     Only a pass slow enough to need feedback ever shows any.
	//   - startSpinner returns nil unless stdout and stderr are both terminals, so every redirect
	//     gets silence for free.
	sp := startSpinner("distilling the profile from the latest facets…")
	err = sm.Summarize(ctx)
	sp.Stop() // nil-safe; blocks until the animation stops writing, so no frame interleaves
	if rsErr != nil {
		return rsErr // Summarize wraps it once per portrait as an InputSizeError
	}
	return err
}

// profileSummarizer is the Summarizer `witness profile` regenerates with, minus its runner, or
// nil when no lens is enabled. One construction for every reader, because Stale compares
// signatures that embed the model, and the model depends on the RESOLVED runner (ModelFor reads
// cfg.Runner against each lens's own): a copy that skipped ResolveRunner would compute other
// signatures and call fresh portraits stale. ensureProfileFresh adds the runner; `witness view`
// and MCP get_profile only ask Stale, which never calls one.
func profileSummarizer(st *store.Store) (*distill.Summarizer, error) {
	cfg := st.ResolvedConfig()
	lenses := activeLenses(st)
	if len(lenses) == 0 {
		return nil, nil
	}
	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts(st.Root)
	if err != nil {
		return nil, err
	}
	return &distill.Summarizer{Store: st, Config: cfg, Lenses: lenses, LensPrompt: lensPrompt, UnifiedPrompt: unifiedPrompt}, nil
}
