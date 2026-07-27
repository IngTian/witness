package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// review.go holds cmdReview and forceReview: the implementation functions for the
// unconditional review pass. The old top-level `review` command was removed from the
// visible front door (#102); the functionality migrated to the hidden `worker review`
// subcommand, but the implementation functions remain here, called by worker_group.go
// and lens.go (backfill's forced review).

func cmdReview() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()
	ran, err := forceReview(st)
	if err != nil {
		return err
	}
	if !ran {
		fmt.Println("a distillation worker is already running; skipping review (it reviews as part of that drain)")
		return nil
	}
	fmt.Println("review complete; profile regenerated")
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
func forceReview(st *store.Store) (bool, error) {
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
	// Same runner lifecycle as the worker: open the SET of runtimes the active lenses need
	// (issue #75 slice 2), review through the per-lens resolver, Close all after. Close runs
	// each OpenCode runner's self-traffic cleanup sweep — held under WorkerLock above so it
	// can't delete a concurrent worker's live distill session.
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
	regenerateProfile(ctx, st, cfg, lenses, runFn, rs.RunFor)
	return true, nil
}
