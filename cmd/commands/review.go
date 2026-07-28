package commands

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// review.go holds cmdReview and forceReview: the implementation functions for the
// unconditional review pass. The old top-level `review` command was removed from the
// visible front door (#102); the functionality migrated to the hidden `worker review`
// subcommand, but the implementation functions remain here, called by worker_group.go
// and lens.go (backfill's forced review).

func cmdReview() error { return cmdReviewFull(false) }

// cmdReviewFull runs the ordinary review; with full=true it ALSO runs the S3 emergent
// long-arc retrieval pass (cluster L1 → verify → merge) in the same locked runner
// session (issue #16). The emergent pass is additive: it never advances the review
// watermark, so it composes with the ordinary review without disturbing S2's fold.
func cmdReviewFull(full bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()
	ran, err := forceReviewOpts(st, full)
	if err != nil {
		return err
	}
	if !ran {
		fmt.Println("a distillation worker is already running; skipping review (it reviews as part of that drain)")
		return nil
	}
	if full {
		fmt.Println("review complete (incl. emergent long-arc pass); profile regenerated")
	} else {
		fmt.Println("review complete; profile regenerated")
	}
	return nil
}

// forceReview runs an L2 review from the current observations, updates facets, and
// regenerates the L4 profiles — the unconditional review the `witness review` command
// exposes, factored out so `lens backfill` can reuse it (a backfill re-mines a lens's
// observations and must force a review afterwards to keep its facets from drifting —
// and `--fresh` DELETES the facets outright, so they must be rebuilt from the freshly
// re-mined observations; the periodic ReviewDue triggers may not fire on a small
// archive, which would otherwise leave that lens with stale/empty facets + a stale
// profile while the command reported success). Returns whether the review ran (false = another
// worker holds the lock, so it will review as part of its own drain). The caller owns
// st and setupLogging; this only borrows st for the review pass.
func forceReview(st *store.Store) (bool, error) { return forceReviewOpts(st, false) }

// forceReviewOpts is forceReview with an optional S3 emergent long-arc pass (full=true).
func forceReviewOpts(st *store.Store, full bool) (bool, error) {
	// Hold the same single-consumer lock the worker uses so foreground review and a
	// background drain cannot race facet/profile commits or open duplicate runner sets.
	unlock, ok := st.WorkerLock()
	if !ok {
		return false, nil
	}
	defer unlock()

	cfg := st.LoadConfig()
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		return false, err
	}
	ctx := context.Background()
	// Same runner lifecycle as the worker: open the set of runtimes the active lenses
	// need, review through the per-lens resolver, then close each private runtime.
	rs, err := newRunnerSet(ctx, st, cfg, lenses)
	if err != nil {
		return false, err
	}
	defer rs.Close()
	runFn := rs.RunFor(nil) // the default runner (unified summary + default fallback)
	r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: rs.RunFor}
	if err := r.Run(ctx, time.Now()); err != nil {
		return false, err
	}
	// S3 emergent long-arc pass (issue #16): after the ordinary fold, cluster L1 and
	// verify candidate arcs the fold is blind to, merging accepted ones into L2. Additive
	// and idempotent (its own cluster-signature state); it never advances the review
	// watermark. Runs in the SAME locked runner session so it can't race a background
	// worker. Best-effort: a failure here must not undo the review that already landed.
	if full {
		er := &distill.EmergentReviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: rs.RunFor}
		if err := er.RunFull(ctx, time.Now()); err != nil {
			slog.Error("emergent long-arc pass failed (ordinary review still applied)", "err", err)
		}
	}
	regenerateProfile(ctx, st, cfg, lenses, runFn, rs.RunFor)
	return true, nil
}
