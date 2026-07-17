package commands

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newLensCmd() *cobra.Command {
	lensCmd := &cobra.Command{
		Use:   "lens",
		Short: "Manage global observation lenses.",
		Long:  "Manage the central lens registry. Registered and enabled lenses run globally across every session alongside the always-on default lens.",
	}
	lensCmd.AddCommand(
		&cobra.Command{
			Use:   "register <name> <dir>",
			Short: "Register or replace a lens definition.",
			Long:  "Copy a lens definition DIRECTORY (holding lens.json + extract.md + review.md) into the witness store. Later edits to the source directory do not affect the registered snapshot until you register it again. Tune models afterward with `witness lens set`.",
			Args:  cobra.ExactArgs(2),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"register"}, args...)) },
		},
		&cobra.Command{
			Use:   "deregister <name>",
			Short: "Remove a registered lens definition.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"deregister"}, args...)) },
		},
		&cobra.Command{
			Use:   "enable <name>",
			Short: "Enable a registered lens for every session.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"enable"}, args...)) },
		},
		&cobra.Command{
			Use:   "disable <name>",
			Short: "Stop running a lens on new distillation work.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"disable"}, args...)) },
		},
		&cobra.Command{
			Use:   "list",
			Short: "List registered lenses and enabled state.",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return cmdLens([]string{"list"}) },
		},
		&cobra.Command{
			Use:   "show <name>",
			Short: "Print a registered lens's definition.",
			Long:  "Print a lens's settings (lens.json) and its EXTRACT + REVIEW prompts. Use `default` to print the built-in lens.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"show"}, args...)) },
		},
		newLensSetCmd(),
		&cobra.Command{
			Use:   "backfill <name>",
			Short: "Mine one lens over the whole history (catch up a newly-enabled lens).",
			Long:  "Reset just this lens's distillation watermark so every past session is re-offered FOR THIS LENS, then drain the backlog in the foreground. Only the named lens is mined — the always-on default lens and every other lens keep their watermarks and are never re-mined. This is the enable-a-new-lens path: cost scales with one lens × history, not all lenses × history.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"backfill"}, args...)) },
		},
		&cobra.Command{
			Use:   "rebuild <name>",
			Short: "Drop one lens's observations + facets, then re-mine it from scratch.",
			Long:  "For when you changed a lens's prompt: delete this lens's derived L1 observations and L2 facets (raw transcripts are untouched), reset its watermark, and re-mine the whole history in the foreground. Only the named lens is affected.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"rebuild"}, args...)) },
		},
		newLensTryCmd(),
	)
	return lensCmd
}

// cmdLens manages the central, global lens registry. Lenses are defined once and
// shared across every session (alongside the always-on "default" lens):
//
//	witness lens register <name> <dir>    add/replace a lens definition
//	witness lens deregister <name>        remove a lens definition
//	witness lens enable <name>            run this lens on every session
//	witness lens disable <name>           stop running it
//	witness lens list                     show registered lenses + enabled state
func cmdLens(args []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(args) == 0 {
		return fmt.Errorf("usage: witness lens <register <name> <dir>|deregister <name>|enable <name>|disable <name>|list>")
	}
	switch args[0] {
	case "register":
		if len(args) < 3 {
			return fmt.Errorf("usage: witness lens register <name> <dir>")
		}
		if err := st.RegisterLens(args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("registered lens %q\n", args[1])
	case "deregister":
		if len(args) < 2 {
			return fmt.Errorf("usage: witness lens deregister <name>")
		}
		if err := st.DeregisterLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("deregistered lens %q\n", args[1])
	case "enable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens enable <name>")
		}
		if !slices.Contains(st.RegisteredLenses(), args[1]) {
			return fmt.Errorf("lens %q is not registered (run: witness lens register %s <dir>)", args[1], args[1])
		}
		if err := st.EnableLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("enabled lens %q (runs on every session)\n", args[1])
	case "disable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens disable <name>")
		}
		if err := st.DisableLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("disabled lens %q\n", args[1])
	case "list":
		enabled := st.LoadConfig().EnabledLenses
		reg := st.RegisteredLenses()
		// The default lens always runs and isn't in the registry; show it first so
		// `lens list` reflects what actually runs, not just the registered extras.
		fmt.Printf("  %s %s  %s\n", green("✓"), store.LensDefault, dim("(built-in, always on)"))
		if len(reg) == 0 {
			fmt.Println(dim("  no additional lenses registered"))
			return nil
		}
		for _, name := range reg {
			if slices.Contains(enabled, name) {
				fmt.Printf("  %s %s  %s\n", green("✓"), name, dim("(enabled — runs on every session)"))
			} else {
				fmt.Printf("  %s %s  %s\n", dim("·"), name, dim("(registered, disabled)"))
			}
		}
	case "show":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens show <name>")
		}
		return lensShow(st, args[1])
	case "backfill":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens backfill <name>")
		}
		return lensBackfill(st, args[1], false)
	case "rebuild":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens rebuild <name>")
		}
		return lensBackfill(st, args[1], true)
	default:
		return fmt.Errorf("unknown lens subcommand %q (want register|deregister|enable|disable|list|show|backfill|rebuild)", args[0])
	}
	return nil
}

// lensBackfill catches one lens up over the whole history (issue #55). It resets
// ONLY that lens's watermark rows (so every past session re-reads as pending for
// it), optionally drops its derived L1/L2 first (rebuild, for a changed prompt),
// then drains the backlog in the foreground. Because only this lens's watermark is
// cleared, the drain mines just this lens on already-distilled sessions — default
// and every other lens keep their watermarks and are never re-mined.
//
// It requires the lens to be ACTIVE (default, or registered+enabled): the drain's
// pending query cross-joins the active-lens set, so a reset watermark for an
// inactive lens would be invisible and nothing would happen. We fail fast with a
// clear message rather than silently no-op.
func lensBackfill(st *store.Store, name string, rebuild bool) error {
	// Resolve the CLI arg to the name the WORKER actually keys data under. A
	// registered lens's mined observations/facets/progress are tagged with the lens's
	// resolved name (its lens.json `name`, via lens.LoadRegistered), which can differ
	// from the registry/CLI name the user typed. Operating on the CLI name would make
	// DeleteLensData/ResetLensWatermark match ZERO rows and silently "succeed" while
	// the real data persists. So load the lens and use its resolved .Name.
	minedName := name
	if name != store.LensDefault {
		if !slices.Contains(st.RegisteredLenses(), name) {
			return fmt.Errorf("lens %q is not registered (see `witness lens list`)", name)
		}
		if !slices.Contains(st.LoadConfig().EnabledLenses, name) {
			return fmt.Errorf("lens %q is registered but not enabled; enable it first (witness lens enable %s), or it won't be mined", name, name)
		}
		l, err := lens.LoadRegistered(name, st.LensesDir())
		if err != nil {
			return fmt.Errorf("load lens %q: %w", name, err)
		}
		minedName = l.Name // the name the worker tags observations/facets/progress with
	}
	if rebuild {
		obs, facets, err := st.DeleteLensData(minedName)
		if err != nil {
			return fmt.Errorf("drop lens %q data: %w", name, err)
		}
		fmt.Printf("rebuild %q: dropped %d observation(s) + %d facet(s)\n", name, obs, facets)
	}
	n, err := st.ResetLensWatermark(minedName)
	if err != nil {
		return fmt.Errorf("reset lens %q watermark: %w", name, err)
	}
	fmt.Printf("reset watermark for lens %q (%d session row(s)); draining in the foreground…\n", name, n)
	// Snapshot the monotonic drift counter before the drain so the completion line can
	// report how many prose_drift events THIS backfill produced (#57) — a below-floor
	// triage model surfaces here, at the moment of the backfill.
	driftBefore := st.DriftTotal()
	// Close our handle before the drain opens its own + takes the WorkerLock. The
	// reset is already committed, so the worker's fresh store snapshot sees it.
	st.Close()

	ran, err := runWorker(false)
	if err != nil {
		return err
	}
	if !ran {
		// Another worker already holds the drain lock, so our foreground drain didn't run.
		// For a plain BACKFILL that's fine: nothing was dropped, and the reset watermark
		// means that worker re-mines this lens as part of its own drain — nothing to do.
		if !rebuild {
			fmt.Println("another distillation worker is already running; it will pick up the backfill — nothing more to do here")
			return nil
		}
		// For a REBUILD it is NOT fine: we already dropped this lens's observations AND
		// facets. The running worker will re-mine L1, but the review that rebuilds the
		// dropped facets + profile is ReviewDue-gated and may never fire on a small/low-
		// poignancy archive — so the lens would be left with empty facets + a stale
		// profile. Don't claim success: report the exact state and the recovery step.
		return fmt.Errorf("rebuild %q incomplete: dropped this lens's observations + facets and reset its watermark, but another distillation worker is already running so the re-mine + review could not run here — once it finishes, run `witness review` to rebuild the facets/profile (or re-run `witness lens rebuild %s`)", name, name)
	}
	// End-state check (mirrors `distill start --all`): the RESET lens must be caught
	// up. runWorker swallows per-session failures, so a nil error alone isn't "done".
	// Scope the check to JUST this lens (not every active lens): a DIFFERENT lens being
	// pending or backed off is unrelated to whether THIS single-lens backfill finished,
	// and counting it would falsely report the backfill as incomplete (the drain even
	// skips a backed-off sibling by design — worker.go LensBackedOff — so its backoff
	// legitimately persists).
	st2, err := store.Open()
	if err != nil {
		return err
	}
	defer st2.Close()
	stats := st2.Stats([]string{minedName})
	if remaining := stats.Pending + stats.BackedOff; remaining > 0 {
		return fmt.Errorf("backfill incomplete: %d session(s) still pending, %d backed off — mining did not finish (check `witness doctor` / witness.log)", stats.Pending, stats.BackedOff)
	}
	// A rebuild DROPPED this lens's L2 facets (and its L4 profile is now stale). The
	// re-mine above only rebuilt L1 observations; facets are reviewer-owned and are NOT
	// regenerated by the drain unless a periodic ReviewDue trigger happens to fire —
	// which it may not on a small or low-poignancy archive. So force a review now to
	// rebuild the facets + profile from the freshly re-mined observations; otherwise
	// `lens rebuild` would report success while leaving that lens with empty facets and
	// a stale profile. (backfill, which only resets the watermark, keeps its facets, so
	// this is rebuild-only.)
	if rebuild {
		fmt.Println("re-mine complete; running a review to rebuild this lens's facets + profile…")
		ran, err := forceReview(st2)
		if err != nil {
			return fmt.Errorf("rebuild %q: review failed; observations were re-mined but facets/profile are not rebuilt (run `witness review`): %w", name, err)
		}
		if !ran {
			// A worker grabbed the drain lock between our drain and this review. Its review
			// is ReviewDue-gated and may not fire, so the dropped facets could stay empty —
			// don't claim success; tell the user how to finish. (Same failure mode as the
			// !ran rebuild branch above, just a narrower race window.)
			return fmt.Errorf("rebuild %q: re-mined observations but another worker took the drain lock before the review could run — facets/profile are not yet rebuilt; run `witness review` to finish", name)
		}
	}
	msg := fmt.Sprintf("lens %q backfill complete", name)
	// A drifted lens advanced its watermark (not "pending") but distilled to zero
	// observations — report it so a below-floor triage model is visible now.
	if drifted := st2.DriftTotal() - driftBefore; drifted > 0 {
		msg += fmt.Sprintf(" (%d session-lens drifted: model returned no observations — raise triage_model, then re-mine; see `witness doctor`)", drifted)
	}
	fmt.Println(msg)
	return nil
}

// newLensSetCmd builds `witness lens set <name> [--runner R] [--extract-model M]
// [--review-model M]`, the safe way to tune a registered lens's per-lens runner + models
// (issue #75). It writes only lens.json fields via a struct round-trip (store.SetLens*)
// — never text surgery on the prompt files — so it can't corrupt a lens. An empty value
// clears the field, so the lens rides the global runner/model again.
func newLensSetCmd() *cobra.Command {
	var runner, extractModel, reviewModel string
	c := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a registered lens's per-lens runner + model overrides.",
		Long:  "Tune a registered lens (written to its lens.json). --runner routes this lens's mine+review to a specific runtime (claude/opencode) instead of the global runner; --extract-model overrides the mining (L0→L1) model; --review-model overrides the review (L1→L2) + summary model. Pass an empty value (e.g. --runner \"\") to clear an override so the lens rides the global again. This is what lets a rare heavy lens run a stronger model — or a cheap lens run a free model on another runtime — without paying for it on every session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return lensSet(args[0],
				cmd.Flags().Changed("runner"), runner,
				cmd.Flags().Changed("extract-model"), extractModel,
				cmd.Flags().Changed("review-model"), reviewModel)
		},
	}
	c.Flags().StringVar(&runner, "runner", "", "per-lens runtime (claude/opencode); empty clears → ride the global runner")
	c.Flags().StringVar(&extractModel, "extract-model", "", "per-lens model for mining (L0→L1); empty clears the override")
	c.Flags().StringVar(&reviewModel, "review-model", "", "per-lens model for review + summary (L1→L2); empty clears the override")
	return c
}

// lensSet applies the flags that were actually PASSED (cobra's Changed) so an unpassed
// flag leaves that field untouched, while an explicit empty value clears it.
func lensSet(name string, setRunner bool, runner string, setExtract bool, extractModel string, setReview bool, reviewModel string) error {
	if !setRunner && !setExtract && !setReview {
		return fmt.Errorf("nothing to set: pass --runner, --extract-model and/or --review-model")
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if setRunner {
		if err := st.SetLensRunner(name, runner); err != nil {
			return err
		}
	}
	if setExtract {
		if err := st.SetLensModel(name, "extract", extractModel); err != nil {
			return err
		}
	}
	if setReview {
		if err := st.SetLensModel(name, "review", reviewModel); err != nil {
			return err
		}
	}
	// Re-read so the confirmation reflects what's now on disk (incl. a cleared field).
	l, err := lens.LoadRegistered(name, st.LensesDir())
	if err != nil {
		return err
	}
	fmt.Printf("lens %q: runner=%s extract-model=%s review-model=%s\n",
		name, modelOrGlobal(l.Runner), modelOrGlobal(l.ExtractModel), modelOrGlobal(l.ReviewModel))
	return nil
}

func modelOrGlobal(m string) string {
	if strings.TrimSpace(m) == "" {
		return dim("(global)")
	}
	return m
}

// lensShow prints a lens's settings + its two prompts. Both a registered lens and the
// built-in `default` render the same way (default's settings are hardcoded, not a
// lens.json, but the view is identical), so the output is a consistent, copyable
// definition regardless of source.
func lensShow(st *store.Store, name string) error {
	var l *lens.Lens
	var err error
	if name == store.LensDefault {
		l, err = lens.LoadDefault()
		if err != nil {
			return fmt.Errorf("load default lens: %w", err)
		}
	} else {
		if !slices.Contains(st.RegisteredLenses(), name) {
			return fmt.Errorf("lens %q is not registered (see `witness lens list`)", name)
		}
		l, err = lens.LoadRegistered(name, st.LensesDir())
		if err != nil {
			return fmt.Errorf("read lens %q: %w", name, err)
		}
	}
	fmt.Print(renderLensDefinition(l))
	return nil
}

// renderLensDefinition renders a lens as its settings header + the two prompts, in a
// plain, copyable shape (no styling → pipeable). It reflects the on-disk directory: a
// lens.json-style settings block, then extract.md and review.md. Emitted verbatim so it
// can be read or used as a STARTING POINT for a new lens directory.
func renderLensDefinition(l *lens.Lens) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", l.Name)
	if len(l.Dimensions) > 0 {
		fmt.Fprintf(&b, "dimensions: %s\n", strings.Join(l.Dimensions, ", "))
	}
	if strings.TrimSpace(l.Runner) != "" {
		fmt.Fprintf(&b, "runner: %s\n", l.Runner)
	}
	if strings.TrimSpace(l.ExtractModel) != "" {
		fmt.Fprintf(&b, "extract-model: %s\n", l.ExtractModel)
	}
	if strings.TrimSpace(l.ReviewModel) != "" {
		fmt.Fprintf(&b, "review-model: %s\n", l.ReviewModel)
	}
	fmt.Fprintf(&b, "\n--- extract.md ---\n%s\n", strings.TrimRight(l.Extract, "\n"))
	fmt.Fprintf(&b, "\n--- review.md ---\n%s\n", strings.TrimRight(l.Review, "\n"))
	return b.String()
}

// activeLenses returns the default lens (always on) + every enabled, registered
// lens. All are global — they run on every session.
func activeLenses(st *store.Store) ([]*lens.Lens, error) {
	out := []*lens.Lens{}
	if p, err := lens.LoadDefault(); err == nil {
		out = append(out, p)
	} else {
		return nil, fmt.Errorf("load default lens: %w", err)
	}
	for _, name := range st.LoadConfig().EnabledLenses {
		l, err := lens.LoadRegistered(name, st.LensesDir())
		if err != nil {
			slog.Warn("enabled lens not loadable; skipping", "lens", name, "err", err)
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// activeLensNames is the per-lens-watermark (#55) view of the active lens set: the
// NAMES the pending/stats queries cross-join sessions against. It returns only
// lenses that actually LOAD (default + enabled-and-loadable), matching activeLenses,
// so the queue never offers a (session,lens) pair the worker can't mine — which
// would otherwise be a no-progress cycle (config says active, mining always skips).
// On a total default-load failure it falls back to ["default"] so the always-on
// lens is never dropped from the watermark accounting.
func activeLensNames(st *store.Store) []string {
	lenses, err := activeLenses(st)
	if err != nil || len(lenses) == 0 {
		return []string{store.LensDefault}
	}
	names := make([]string, 0, len(lenses))
	for _, l := range lenses {
		names = append(names, l.Name)
	}
	return names
}
