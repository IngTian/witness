package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// `witness compact` — rebalance the active facet surface: merge facets that are one trait,
// retire ones that were never traits. Nothing is deleted; archived facets keep every version
// and link to the facet that absorbed them.
//
// DRY RUN BY DEFAULT, and that is deliberate rather than cautious boilerplate. This is the one
// command that rewrites the shape of someone's profile, the judgment is a model's, and the
// result is not obvious in advance — so the default prints the plan and changes nothing, and
// --apply is a separate, explicit act.
//
// Within ONE invocation the plan printed is the plan applied (Apply replays the actions Plan
// produced and makes no fresh judgment). Across invocations it is not: --apply re-plans, so its
// proposals can differ from a previous dry run's. That is why the dry-run footer says so rather
// than implying a hand-off. Persisting a plan to disk would buy that hand-off, and is not worth
// a new on-disk format until someone actually wants to approve a plan in one sitting and apply
// it in another.
func newCompactCmd() *cobra.Command {
	var apply bool
	var lensOnly string
	c := &cobra.Command{
		Use:     "compact [--apply]",
		Short:   "Consolidate redundant facets into a minimal profile (dry run unless --apply)",
		GroupID: groupAdmin,
		Long: "Rebalance the active facet profile: merge facets that say the same thing, retire ones that were never really traits.\n\n" +
			"Facets only ever accumulated before this existed — nothing removed or combined one, and no prompt asked " +
			"whether two facets were the same. A long-lived archive ends up with many facets restating each other, " +
			"which makes every review call larger and the profile less useful, not more.\n\n" +
			"Nothing is deleted. A compacted facet is ARCHIVED with its full version history intact and a link to the " +
			"facet that absorbed it, so any node in the live profile can be unfolded into the trajectory behind it.\n\n" +
			"Prints a plan and changes nothing unless you pass --apply. Re-runnable: each pass consolidates what it " +
			"can see, so running it again after new reviews is normal.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmdCompact(apply, lensOnly)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "apply the plan (default: dry run, prints the plan and writes nothing)")
	c.Flags().StringVar(&lensOnly, "lens", "", "compact only this lens (default: every enabled lens)")
	return c
}

func cmdCompact(apply bool, lensOnly string) error {
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
		fmt.Println("a distillation worker is already running; try again once it finishes")
		return nil
	}
	defer unlock()

	cfg := st.LoadConfig()
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		return err
	}
	if lensOnly != "" {
		filtered := lenses[:0]
		for _, ln := range lenses {
			if ln.Name == lensOnly {
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

	c := &distill.Compactor{
		Store: st, Lenses: lenses, Config: cfg,
		Runner: rs.RunFor(nil), RunnerFor: rs.RunFor,
	}

	fmt.Printf("planning compaction over %s...\n", pluralize(len(lenses), "lens", "lenses"))
	plans, err := c.Plan(ctx)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		fmt.Println(okGlyph() + " nothing to compact — no lens proposed a merge or retirement")
		return nil
	}

	printCompactPlans(plans)
	if !apply {
		fmt.Println()
		// Be precise about what --apply does: it plans AGAIN and applies that fresh plan, so
		// the proposals can differ slightly from the ones printed above. Saying "commit this
		// plan" would promise a hand-off between runs that does not exist (Plan and Apply are
		// one invocation; only a single run guarantees the plan shown is the plan applied).
		fmt.Println(dim("dry run — nothing written. --apply re-plans and commits in one run, so"))
		fmt.Println(dim("its proposals may differ slightly from the ones above."))
		return nil
	}
	if err := c.Apply(ctx, plans, time.Now()); err != nil {
		return err
	}
	fmt.Println()
	for _, p := range plans {
		active, archived := st.LensFacetCounts(p.Lens)
		fmt.Printf("%s %s: %d active facets (%d archived, history kept)\n",
			okGlyph(), bold(p.Lens), active, archived)
	}
	fmt.Println()
	fmt.Println(dim("the profile narrative is regenerated on the next review (`witness worker review`)"))
	return nil
}

func printCompactPlans(plans []distill.CompactPlan) {
	for _, p := range plans {
		fmt.Println()
		fmt.Println(header(fmt.Sprintf("%s — %d active facets, %d would be archived",
			p.Lens, p.Before, p.Archived)))
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
			fmt.Printf("  %s %v\n", dim("refused (invalid proposals):"), p.Skipped)
		}
		if p.Failed > 0 {
			// A partial plan must never read as a complete one. Without this, a run where
			// most calls timed out looks identical to a profile that is already tight.
			fmt.Printf("  %s %s failed (model error or timeout) — this plan is PARTIAL; see %s\n",
				warnGlyph(), pluralize(p.Failed, "batch", "batches"), dim("witness.log"))
		}
	}
}
