package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// autoClaimer is the bounded claim pass a review runs before each lens's fold, so a lens that was
// never compacted still gets claims and its review payload shrinks instead of timing out. Both
// review paths (`witness worker review` and the drain) build their Reviewer with it, so neither
// can forget it. It rides the same runners as the fold.
func autoClaimer(st *store.Store, cfg store.Config, run distill.MineFunc, runFor func(*lens.Lens) distill.MineFunc) *distill.Claimer {
	return &distill.Claimer{Store: st, Config: cfg, Runner: run, RunnerFor: runFor, MaxBatches: distill.AutoClaimBatches}
}

// review.go holds cmdReviewFull and forceReviewTapped: the implementation functions for the
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
		printProfileCause(tap, profErr)
		return nil
	}
	fmt.Println(okGlyph() + " " + done + " — profile regenerated")
	return nil
}

// printProfileCause prints, under a "profile was NOT regenerated" result, why. A failed portrait
// call carries what it sent and the review_model lever (distill.InputSizeError, F095); both get
// their own line, because the tap noted the bare runner error before distill added them, and the
// one-line cause is cut long before them anyway. That call's own error is then the cause, not the
// tap's first: a best-effort claim batch earlier in the run shares the tap.
func printProfileCause(tap *causeTap, profErr error) {
	var ie *distill.InputSizeError
	if errors.As(profErr, &ie) {
		tap = &causeTap{}
		tap.note(ie.Err)
	} else {
		tap.note(profErr) // a model error during it, if one was seen; else its own (missing prompts …)
	}
	fmt.Println("  " + kvRow("cause", tap.cause(), ""))
	if ie != nil {
		fmt.Println("  " + kvRow("input", ie.Size, ""))
		fmt.Println(hint("if too large", ie.Lever))
	}
	fmt.Println(hint("diagnose", "witness doctor"))
}

// forceReviewTapped runs an L2 review from the current observations, updates facets, and
// regenerates the L4 profiles — the unconditional review `worker review` exposes, shared with
// `lens backfill` (a backfill re-mines a lens's observations and must force a review afterwards
// to keep its facets from drifting — and `--fresh` DELETES the facets outright; the periodic
// ReviewDue triggers may not fire on a small archive). full=true adds the S3 emergent long-arc
// pass. It notes the first model error in tap — a failed fold names it under its headline — and
// returns the profile regeneration's own failure (profErr) separately: the review's facets have
// landed by then, so it is not the review failing, and every caller must report it. ran=false
// means another worker holds the lock. The caller owns st and setupLogging.
func forceReviewTapped(st *store.Store, full bool, tap *causeTap) (ran bool, profErr, err error) {
	// Hold the same single-consumer lock the worker uses so foreground review and a
	// background drain cannot race facet/profile commits or open duplicate runner sets.
	unlock, ok := st.WorkerLock()
	if !ok {
		return false, nil, nil
	}
	defer unlock()

	cfg := st.ResolvedConfig()
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
	// lens had 109 windows waiting. The drain (worker.go) builds its own Reviewer and draws the same
	// line only for a foreground run (lens backfill, worker run); a detached worker stays silent.
	fold := &phaseProgress{start: startProgress}
	claims := autoClaimer(st, cfg, runFn, runFor)
	claims.Progress = fold.update
	r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn, RunnerFor: runFor, Progress: fold.update, Claims: claims}
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
	_ = resolvePortraitSources(ctx, st, cfg, lenses, untappedRunFn, untappedRunFor, func() bool { return true }) // runners open; best-effort: logged
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
