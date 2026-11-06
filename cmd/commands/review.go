package commands

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// review.go holds cmdReviewFull and forceReview: the implementation functions for the
// unconditional review pass. The old top-level `review` command was removed from the
// visible front door (#102); the functionality migrated to the hidden `worker review`
// subcommand, but the implementation functions remain here, called by worker_group.go
// and lens.go (backfill's forced review).

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
	tap := &causeTap{}
	ran, profErr, err := forceReviewTapped(st, full, tap)
	if err != nil {
		return err
	}
	if !ran {
		fmt.Print(workerBusy(st, "review skipped; its drain reviews"))
		return nil
	}
	done := "review complete"
	if full {
		done += ", with the emergent long-arc pass"
	}
	if profErr != nil {
		// The facets landed; only the narrative did not. Say so rather than "profile regenerated".
		fmt.Println(warnGlyph() + " " + done + " — the profile was NOT regenerated (the previous one is kept)")
		tap.note(profErr) // a model error during it, if one was seen; else its own (missing prompts …)
		fmt.Println("  " + kvRow("cause", tap.cause(), ""))
		fmt.Println(hint("diagnose", "witness doctor"))
		return nil
	}
	fmt.Println(okGlyph() + " " + done + " — profile regenerated")
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
	ran, _, err := forceReviewTapped(st, full, &causeTap{})
	return ran, err
}

// forceReviewTapped is forceReviewOpts that notes the first model error in tap — a failed fold
// names it under its headline — and returns the profile regeneration's own failure (profErr)
// separately: the review's facets have landed by then, so it is not the review failing.
func forceReviewTapped(st *store.Store, full bool, tap *causeTap) (ran bool, profErr, err error) {
	// Hold the same single-consumer lock the worker uses so foreground review and a
	// background drain cannot race facet/profile commits or open duplicate runner sets.
	unlock, ok := st.WorkerLock()
	if !ok {
		return false, nil, nil
	}
	defer unlock()

	cfg := st.LoadConfig()
	cfg.Runner = st.ResolveRunner(cfg)
	lenses := activeLenses(st)
	ctx := context.Background()
	// Same runner lifecycle as the worker: open the set of runtimes the active lenses
	// need, review through the per-lens resolver, then close each private runtime.
	rs, err := newRunnerSet(ctx, st, cfg, lenses)
	if err != nil {
		return false, nil, err
	}
	defer rs.Close()
	untappedRunFn := rs.RunFor(nil)       // the default runner (unified summary + default fallback)
	untappedRunFor := rs.RunFor           // per-lens routing
	runFn := tap.wrap(untappedRunFn)      // wrapped for causeTap (mining + review)
	runFor := tap.wrapFor(untappedRunFor) // wrapped for causeTap
	// Progress on stderr: a fold is one model call per window, and a real archive's default
	// lens had 109 windows waiting. This is the foreground path only — the drain (worker.go)
	// builds its own Reviewer and leaves Progress nil.
	fold := &phaseProgress{start: startProgress}
	r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: runFor, Progress: fold.update}
	err = r.Run(ctx, time.Now())
	fold.finish()
	if err != nil {
		if c := tap.cause(); c != "" {
			return false, nil, fmt.Errorf("%w\n%s\n%s", err, errRow("cause", c), errHint("diagnose", "witness doctor"))
		}
		return false, nil, err
	}
	// S3 emergent long-arc pass (issue #16): after the ordinary fold, cluster L1 and
	// verify candidate arcs the fold is blind to, merging accepted ones into L2. Additive
	// and idempotent (its own cluster-signature state); it never advances the review
	// watermark. Runs in the SAME locked runner session so it can't race a background
	// worker. Best-effort: a failure here must not undo the review that already landed.
	if full {
		arcs := &phaseProgress{start: startProgress}
		er := &distill.EmergentReviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: runFor, Progress: arcs.update}
		err := er.RunFull(ctx, time.Now())
		arcs.finish()
		if err != nil {
			slog.Error("emergent long-arc pass failed (ordinary review still applied)", "err", err)
		}
	}
	// The same spinner, and message, as the read-time regeneration (profilepull.go).
	sp := startSpinner("distilling the profile from the latest facets…")
	profErr = regenerateProfile(ctx, st, cfg, lenses, runFn, runFor)
	sp.Stop()
	sp = startSpinner("linking the profile to its sources…")
	_ = resolvePortraitSources(ctx, st, cfg, lenses, untappedRunFn, untappedRunFor) // best-effort: logged
	sp.Stop()
	return true, profErr, nil
}

// phaseProgress draws each phase of a pass — a new Where, or a new set of stages — on a fresh
// progress line. A line estimates time left from its own start, so one line carried across lenses
// would divide the first lens's hours by the second lens's count, and one carried from clustering
// (minutes of CPU per step) into verifying (one model call per step) would price every call at a
// clustering step. Not safe for concurrent use: both review passes are sequential.
type phaseProgress struct {
	start func() *progressLine // startProgress; a test supplies its own
	cur   *progressLine
	key   string
}

func (p *phaseProgress) update(pr distill.Progress) {
	key := pr.Where
	for _, s := range pr.Stages {
		key += "\x00" + s.Name
	}
	if p.cur == nil || key != p.key {
		p.finish()
		p.cur, p.key = p.start(), key
	}
	p.cur.update(pr)
}

func (p *phaseProgress) finish() {
	if p.cur != nil {
		p.cur.finish()
	}
}
