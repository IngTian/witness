package commands

import (
	"cmp"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// `witness compact` — rebalance the active facet surface: merge facets that are one trait,
// retire ones that were never traits. Nothing is deleted; archived facets keep every version
// and link to the facet that absorbed them.
//
// IT SHOWS THE PLAN, THEN ASKS. This is the one command that rewrites the shape of someone's
// profile, the judgment is a model's, and the result is not obvious in advance — so nothing is
// applied without a yes. The plan applied is exactly the one shown: Apply replays the actions
// Plan produced and makes no fresh judgment, and the worker lock is held across the question, so
// no review rewrites the facets in between.
//
// It used to be a dry run unless --apply, and --apply PLANNED AGAIN: two rounds of model calls,
// and what it applied could differ from the dry run the user had approved. Asking inside one
// invocation removes both. --apply still works, as --yes, for scripts written against it.
func newCompactCmd() *cobra.Command {
	var yes bool
	var lensOnly string
	c := &cobra.Command{
		Use:     "compact",
		Short:   "Consolidate redundant facets into a minimal profile (shows the plan, then asks).",
		GroupID: groupAdmin,
		Long: "Rebalance the active facet profile: merge facets that say the same thing, retire ones that were never really traits.\n\n" +
			"Facets only ever accumulated before this existed — nothing removed or combined one, and no prompt asked " +
			"whether two facets were the same. A long-lived archive ends up with many facets restating each other, " +
			"which makes every review call larger and the profile less useful, not more.\n\n" +
			"Nothing is deleted. A compacted facet is ARCHIVED with its full version history intact and a link to the " +
			"facet that absorbed it, so any node in the live profile can be unfolded into the trajectory behind it.\n\n" +
			"Prints the plan, then asks before applying it; the plan applied is exactly the one shown. Anything but " +
			"'y' writes nothing. --yes applies without asking, for scripts. Re-runnable: each pass consolidates what " +
			"it can see, so running it again after new reviews is normal.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmdCompact(yes, lensOnly, askYesNo)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "apply the plan without asking, for scripts")
	// The old spelling, from when compact was a dry run unless told. Same variable, so a script
	// written against it keeps working; hidden so help shows one way to say it.
	c.Flags().BoolVar(&yes, "apply", false, "same as --yes")
	_ = c.Flags().MarkHidden("apply")
	c.Flags().StringVar(&lensOnly, "lens", "", "compact only this lens (default: every enabled lens)")
	return c
}

// cmdCompact plans, shows the plan, asks, and applies that same plan. ask is injected so tests can
// answer; yes skips it. Declining exits 0, as a dry run always did: unlike a merge, nothing a
// following command does can lose data because the plan went unapplied.
func cmdCompact(yes bool, lensOnly string, ask func(prompt string) bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()

	// The same single-consumer lock the worker and foreground review take: compaction
	// rewrites L2, so it must not interleave with a drain's facet commit.
	unlock, ok := st.WorkerLock()
	if !ok {
		fmt.Print(workerBusy(st, "compact skipped"))
		return nil
	}
	defer unlock()

	cfg := st.ResolvedConfig()
	lenses, err := activeLenses(st)
	if err != nil {
		return err
	}
	if len(lenses) == 0 {
		fmt.Println(skipGlyph() + " no enabled lenses — nothing to compact")
		fmt.Println(hint("enable one", "witness lens enable <name>"))
		return nil
	}
	if lensOnly != "" {
		// Loaded lenses carry their MINED name; the flag takes the registered one.
		mined := lens.DataName(st, lensOnly)
		filtered := lenses[:0]
		for _, ln := range lenses {
			if ln.Name == mined {
				filtered = append(filtered, ln)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("lens %q is not enabled (see `witness lens list`)", lensOnly)
		}
		lenses = filtered
	}

	ctx := context.Background()
	rs, err := newRunnerSet(ctx, st, cfg, lenses)
	if err != nil {
		return err
	}
	defer rs.Close()

	// tap keeps the first model error: a batch that fails is counted in the plan, but WHY it
	// failed was only in witness.log.
	tap := &causeTap{}
	c := &distill.Compactor{
		Store: st, Lenses: lenses, Config: cfg,
		Runner: tap.wrap(rs.RunFor(nil)), RunnerFor: tap.wrapFor(rs.RunFor),
	}

	fmt.Println(dim(fmt.Sprintf("planning compaction over %s…", pluralize(len(lenses), "lens", "lenses"))))
	pl := startProgress()
	c.Progress = pl.update
	plans, err := c.Plan(ctx)
	pl.finish()
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		fmt.Println(skipGlyph() + " nothing to compact — no lens proposed a merge or retirement")
		return nil
	}
	// Nothing proposed AND calls failed: there is no plan, only an outage. Asking "Apply this
	// plan?" over it offered to apply nothing, and the reason (runner disabled, provider down) was
	// in witness.log alone. Fail, with the cause, and do not ask.
	if failed, proposed := compactTotals(plans); proposed == 0 && failed > 0 {
		return errLines("compaction could not plan: "+pluralize(failed, "batch", "batches")+
			" failed, nothing proposed — nothing written",
			errRow("cause", cmp.Or(tap.cause(), "no runner error — see witness.log")), errHint("diagnose", "witness doctor"))
	}

	printCompactPlans(plans, tap.cause())
	fmt.Println()
	if !yes && !ask(bold("Apply this plan?")+dim(" Nothing is deleted; archived facets keep their history.")+" [y/N]: ") {
		fmt.Println(dim("not applied — nothing written"))
		return nil
	}
	if err := c.Apply(ctx, plans, time.Now()); err != nil {
		return err
	}

	// The second half of "make the profile compact": give every facet still lacking one its
	// short CLAIM. Merged survivors already got theirs from the merge call; this covers the
	// facets compaction left alone.
	//
	// It belongs here rather than in the review because it is maintenance, the same as
	// merging: adding model calls to every fold is a cost the user did not ask for. And the
	// two halves do different work — merging cuts the facet COUNT, claims cut the per-facet
	// payload — so a profile needs both to actually get smaller. Measured: compaction alone
	// took a dimension 73% fewer facets but only 15% less text.
	cl := &distill.Claimer{
		Store: st, Lenses: lenses, Config: cfg,
		Runner: rs.RunFor(nil), RunnerFor: rs.RunFor,
	}
	for _, ln := range lenses {
		cp := startProgress()
		cl.Progress = cp.update
		n := cl.Run(ctx, ln)
		cp.finish()
		if n > 0 {
			fmt.Println(resultRow(okGlyph(), ln.Name, "wrote "+pluralize(n, "short claim", "short claims"), ""))
		}
	}

	for _, p := range plans {
		active, archived := st.LensFacetCounts(p.Lens)
		fmt.Println(resultRow(okGlyph(), p.Lens, countOf(active, "active facet", "active facets"),
			humanCount(archived)+" archived, history kept"))
	}
	fmt.Println()
	fmt.Println(okGlyph() + " compacted — the profile narrative is regenerated on the next review")
	fmt.Println(hint("regenerate", "witness worker review"))
	return nil
}

// compactTotals sums the plans: batches that failed, and actions proposed.
func compactTotals(plans []distill.CompactPlan) (failed, proposed int) {
	for _, p := range plans {
		failed += p.Failed
		proposed += len(p.Actions)
	}
	return failed, proposed
}

// refusals renders a plan's refused-proposal counts in a stable order: "2 absorb unseen facet ·
// 1 merge too small". It printed a Go map literal (map[absorb_unseen_facet:2 …]) before.
func refusals(skipped map[string]int) string {
	reasons := make([]string, 0, len(skipped))
	for r := range skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = fmt.Sprintf("%s %s", humanCount(skipped[r]), strings.ReplaceAll(r, "_", " "))
	}
	return strings.Join(parts, " · ")
}

// printCompactPlans shows each lens's plan. cause is the first model error this run hit, named
// under a PARTIAL plan's warning ("" when none was seen).
func printCompactPlans(plans []distill.CompactPlan, cause string) {
	for _, p := range plans {
		fmt.Println()
		fmt.Println(header(fmt.Sprintf("%s — %s active facets, %s would be archived",
			p.Lens, humanCount(p.Before), humanCount(p.Archived))))
		// Facet count and payload size move independently: a run once cut the count 70% while
		// DOUBLING the text, which makes the review payload worse. Print both or the headline
		// is misleading.
		if p.InputChars > 0 {
			fmt.Printf("  %s %s chars of merged facets → %s (%.2fx)\n", dim("text:"),
				humanCount(p.InputChars), humanCount(p.OutputChars),
				float64(p.OutputChars)/float64(p.InputChars))
		}
		for _, a := range p.Actions {
			switch a.Action {
			case "merge":
				fmt.Printf("  %s %s/%s\n", cyan("merge →"), a.Dimension, bold(a.Key))
				for _, k := range a.Absorb {
					fmt.Printf("      %s %s\n", dim("absorbs"), k)
				}
				fmt.Printf("      %s %s\n", dim("why"), a.Why)
			case "retire":
				fmt.Printf("  %s %s/%s — %s\n", yellow("retire"), a.Dimension, a.Key, dim(a.Why))
			}
		}
		if len(p.Actions) == 0 {
			fmt.Println(dim("  no merges or retirements proposed"))
		}
		if len(p.Skipped) > 0 {
			// A prompt regression shows up as a pile of skips. Surfacing it beats a quietly
			// smaller plan that looks like "the profile is already tight".
			fmt.Printf("  %s %s\n", dim("refused"), refusals(p.Skipped))
		}
		if p.Failed > 0 {
			// A partial plan must never read as a complete one. Without this, a run where
			// most calls timed out looks identical to a profile that is already tight.
			fmt.Printf("  %s %s failed — this plan is PARTIAL\n", warnGlyph(), pluralize(p.Failed, "batch", "batches"))
			if cause != "" {
				fmt.Printf("    %s %s\n", dim("first error"), cause)
			}
		}
	}
}
