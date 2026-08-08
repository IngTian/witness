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
// The L4 narrative used to be pushed: every review regenerated every summary whether or not
// anyone would ever read it. That is what made cost, dirty-tracking, and worker liveness the
// dominant concerns of this layer — N+1 serial LLM calls in the worker's tail, a <2-lens rule
// to dodge one duplicate call, and a background sweep to delete summaries that might be stale.
//
// Measured on a real `claude -p` (3 sessions, 4 facets, one lens): a cached read is 0.02s and a
// regeneration is 13.3s. 13s is acceptable to block an interactive command or an MCP tool call,
// so generation moves to the read: `witness profile` and MCP get_profile regenerate what they
// are asked for, if and only if it is stale, and serve it. An unread profile costs nothing.
//
// LAZINESS IS PER SUMMARY. Asking for one profile must not regenerate the whole set, or a
// many-lens archive would pay N*13s for a single read. distill.Summarizer already skips any
// summary whose signature matches, so a targeted read costs exactly the calls it needs.
//
// This widens the MCP surface: it was read-only (never wrote, never locked). Regenerating on
// read gives that path a runner, write access, and lock coordination with the worker — the real
// cost of pull, accepted deliberately and guarded below.

// ensureProfileFresh regenerates stale L4 summaries before a read, and reports whether it ran.
//
// It returns (ran, err). A FAILURE IS NOT FATAL to the caller: the reader still serves whatever
// is on disk, because a derived narrative that cannot be rebuilt right now is strictly better
// served stale than not at all. The error is returned so an interactive caller can say so.
//
// The WorkerLock is the load-bearing guard. `witness profile` blocking ~13s on its own
// regeneration is fine; blocking minutes behind somebody else's backfill is not — and two
// processes writing profiles concurrently would race the file and its signature. So a read that
// cannot take the lock serves the cached profile instead of queueing. The lock is the same
// single-consumer lock the worker and forceReview use, so pull can never race a push.
func ensureProfileFresh(st *store.Store) (bool, error) {
	unlock, ok := st.WorkerLock()
	if !ok {
		// A drain (or another reader) owns the lock. Serving cached is the whole point of the
		// guard, so this is a normal outcome, not an error.
		slog.Debug("profile: a distillation already holds the worker lock; serving the cached profile")
		return false, nil
	}
	defer unlock()

	cfg := st.LoadConfig()
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		return false, err
	}
	// Nothing enabled → nothing to summarize. Bail before opening a runner: on a fresh archive
	// this is the difference between a read that returns instantly and one that starts (and
	// prewarms) a model runtime to produce nothing.
	if len(lenses) == 0 {
		return false, nil
	}

	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts(st.Root)
	if err != nil {
		return false, err
	}

	ctx := context.Background()
	rs, err := newRunnerSet(ctx, st, cfg, lenses)
	if err != nil {
		return false, err
	}
	defer rs.Close()

	sm := &distill.Summarizer{
		Store:         st,
		Config:        cfg,
		Lenses:        lenses,
		LensPrompt:    lensPrompt,
		UnifiedPrompt: unifiedPrompt,
		Run:           rs.RunFor(nil),
		RunFor:        rs.RunFor,
	}
	if err := sm.Summarize(ctx); err != nil {
		return false, err
	}
	return true, nil
}
