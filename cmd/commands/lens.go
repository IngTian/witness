package commands

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
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
		Long:  "Manage the central lens registry. Every enabled, registered lens runs globally across every session. The built-in \"default\" person-growth lens is an ordinary registered lens (auto-seeded once on first use, restore with `witness lens load-default`) — enable/disable/edit/re-register it like any other; an archive may run any set of lenses, including none.",
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
		&cobra.Command{
			Use:   "load-default",
			Short: "Re-seed / restore the built-in \"default\" person-growth lens.",
			Long:  "Register the bundled \"default\" person-growth lens into your archive and enable it, from ANY current state — deregistered, disabled, or already present. This is the explicit restore path for the auto-seeded starter lens: install no longer seeds content (#102), and a deliberately-removed default stays removed, so this command is how you bring it back or re-seed it after editing broke it.",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return cmdLens([]string{"load-default"}) },
		},
		newLensSetCmd(),
		newLensBackfillCmd(),
		newLensTryCmd(),
	)
	return lensCmd
}

// newLensBackfillCmd builds `witness lens backfill <name> [--fresh]`, the single
// catch-up command (issue #102 folded the former `rebuild` into `--fresh`). Plain
// backfill resets the lens's watermark and re-mines its whole history; --fresh ALSO
// drops the lens's existing L1 observations + L2 facets first, for when its prompt
// changed and you want a clean slate rather than a merge. Either way it ends with a
// forced review, so the lens's facets + profile never drift from the re-mined
// observations — the consistency the old two-command split lacked.
func newLensBackfillCmd() *cobra.Command {
	var fresh, yes bool
	c := &cobra.Command{
		Use:   "backfill <name>",
		Short: "Re-mine one lens over the whole history, then refresh its facets.",
		Long:  "Reset just this lens's distillation watermark so every past session is re-offered FOR THIS LENS, drain the backlog in the foreground, then run a review so this lens's facets + profile reflect the re-mined observations. Only the named lens is re-mined — every other lens (default included) keeps its watermark. This is the enable-a-new-lens path: cost scales with one lens × history.\n\nWith --fresh, first DELETE this lens's existing L1 observations + L2 facets (raw transcripts are untouched) before re-mining — use it when you changed the lens's prompt and want a clean rebuild instead of merging into the old observations. --fresh is DESTRUCTIVE: mined observations are re-created from L0, but any in-session (active) observations you recorded are lost. It asks for confirmation first; pass --yes to skip the prompt in scripts.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			st, err := store.Open()
			if err != nil {
				return err
			}
			defer st.Close()
			return lensBackfill(st, args[0], fresh, yes)
		},
	}
	c.Flags().BoolVar(&fresh, "fresh", false, "drop this lens's existing observations + facets first, then re-mine from scratch (DESTRUCTIVE; for a changed prompt)")
	c.Flags().BoolVar(&yes, "yes", false, "skip the --fresh confirmation prompt (for non-interactive use)")
	return c
}

// cmdLens manages the central, global lens registry. Lenses are defined once and
// shared across every session. Since #44 slice 1a "default" is an ordinary registered
// lens (no always-on status), so it is managed through these same commands:
//
//	witness lens register <name> <dir>    add/replace a lens definition
//	witness lens deregister <name>        remove a lens definition
//	witness lens enable <name>            run this lens on every session
//	witness lens disable <name>           stop running it
//	witness lens list                     show registered lenses + enabled state
//	witness lens load-default             re-seed / restore the built-in default lens
func cmdLens(args []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(args) == 0 {
		return fmt.Errorf("usage: witness lens <register <name> <dir>|deregister <name>|enable <name>|disable <name>|list|load-default>")
	}
	switch args[0] {
	case "load-default":
		// Explicit restore, bypassing the first-open one-shot gate: register the bundled
		// default prompts + enable, from any state (deregistered/disabled/present). Since
		// #102 install no longer seeds content and a deliberately-removed default stays
		// gone, this is the sole "bring default back" path.
		if err := seedDefaultLens(st); err != nil {
			return fmt.Errorf("load default lens: %w", err)
		}
		fmt.Println("loaded the built-in 'default' person-growth lens (registered + enabled)")
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
		if args[1] == store.LensDefault {
			fmt.Println("  (this was the built-in person-growth lens — it will NOT come back on its own; restore it any time with `witness lens load-default`)")
		}
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
		if args[1] == store.LensDefault {
			fmt.Println("  (re-enable it with `witness lens enable default`, or `witness lens load-default` to restore the built-in lens)")
		}
	case "list":
		enabled := st.LoadConfig().EnabledLenses
		reg := st.RegisteredLenses()
		// Since #44 slice 1a "default" has no special status — it is a registered lens
		// like any other (auto-seeded on first use, or migrated in), so it appears in this loop
		// naturally. An archive may have ZERO lenses (all disabled / none seeded), a legal
		// "nothing to distill" state.
		if len(reg) == 0 {
			fmt.Println(dim("  no lenses registered — nothing is distilled (register one with `witness lens register <name> <dir>`)"))
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
	default:
		return fmt.Errorf("unknown lens subcommand %q (want register|deregister|enable|disable|list|show|load-default|backfill|set|try)", args[0])
	}
	return nil
}

// lensBackfill catches one lens up over the whole history (issue #55). It resets
// ONLY that lens's watermark rows (so every past session re-reads as pending for it),
// optionally drops its derived L1/L2 first (--fresh, for a changed prompt), drains the
// backlog in the foreground, and ALWAYS ends with a forced review. Because only this
// lens's watermark is cleared, the drain mines just this lens on already-distilled
// sessions — default and every other lens keep their watermarks and are never re-mined.
//
// The forced review is unconditional (issue #102 folded the former `rebuild` into
// `--fresh`): a plain backfill re-mines a lens's L1 observations, but facets are
// reviewer-owned and the drain only reviews when ReviewDue fires — which may not on a
// small/low-poignancy archive — so without a forced review the lens's L2 facets + L4
// profile could drift from the re-mined observations. Reviewing every time keeps them
// consistent; --fresh additionally needs it because it DROPPED the facets outright.
//
// It requires the lens to be registered AND enabled (since #44 slice 1a default has no
// exemption — it's an ordinary lens): the drain's pending query cross-joins the
// active-lens set, so a reset watermark for an inactive lens would be invisible and
// nothing would happen — and for `--fresh` the pre-drop of obs+facets would then be
// irrecoverable. We fail fast with a clear message rather than silently no-op / lose data.
func lensBackfill(st *store.Store, name string, fresh, assumeYes bool) error {
	// Resolve the CLI arg to the name the WORKER actually keys data under. A
	// registered lens's mined observations/facets/progress are tagged with the lens's
	// resolved name (its lens.json `name`, via lens.LoadRegistered), which can differ
	// from the registry/CLI name the user typed. Operating on the CLI name would make
	// DeleteLensData/ResetLensWatermark match ZERO rows and silently "succeed" while
	// the real data persists. So load the lens and use its resolved .Name.
	// Since #44 slice 1a "default" has NO privileged always-active status — it is an
	// ordinary registered lens. So it is subject to the SAME registered+enabled
	// precondition as every other lens: without it, `lens backfill default --fresh` on a
	// disabled/deregistered default would DeleteLensData (drop obs+facets) and then no-op
	// the re-mine (the drain excludes inactive lenses), destroying data irrecoverably. The
	// guard below fails fast in exactly that case, for default like any lens.
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
	minedName := l.Name // the name the worker tags observations/facets/progress with
	if fresh {
		// --fresh is DESTRUCTIVE and #102 folded it in behind a safe-sounding flag, so
		// gate it like every other irreversible op (mirror `witness cleanup`): (1) fail
		// FAST if a worker is live, BEFORE dropping anything — otherwise we delete this
		// lens's obs+facets and then discover we can't re-mine (runWorker returns ran=false),
		// leaving the lens gutted; (2) warn that ACTIVE (hand-recorded) observations are NOT
		// re-derivable from L0 and will be lost — the "safe, re-mined from L0" story only
		// holds for MINED obs; (3) confirm unless --yes. Only then drop.
		if st.WorkerActive() {
			return fmt.Errorf("a distillation worker is running; backfill %q --fresh would drop this lens's data but couldn't re-mine it now — wait until `witness distill status` is idle, then retry", name)
		}
		obsAll, facetsN := st.LensDataCounts(minedName)
		active := st.ActiveObservationCount(minedName)
		if !assumeYes {
			fmt.Printf("\n%s backfill %q --fresh will DELETE %d observation(s) + %d facet(s) for this lens, then re-mine from your raw transcripts.\n",
				warnGlyph(), name, obsAll, facetsN)
			if active > 0 {
				fmt.Printf("  %s\n", yellow(fmt.Sprintf("%d of those observations were recorded in-session (not mined) and are NOT reproducible by a re-mine — they will be lost permanently.", active)))
			}
			fmt.Println(dim("  Raw transcripts (L0) are kept. Mined observations are re-created from them."))
			fmt.Print("Proceed? [y/N]: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				fmt.Println("Aborted; nothing deleted.")
				return nil
			}
		}
		obs, facets, err := st.DeleteLensData(minedName)
		if err != nil {
			return fmt.Errorf("drop lens %q data: %w", name, err)
		}
		fmt.Printf("backfill %q --fresh: dropped %d observation(s) + %d facet(s)\n", name, obs, facets)
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
		// Another worker already holds the drain lock, so our foreground drain (and the
		// forced review below) didn't run.
		if !fresh {
			// Plain backfill dropped NOTHING: the reset watermark means the running worker
			// re-mines this lens as part of its own drain. Its facets survive (just possibly
			// stale vs. the imminent re-mine), and the worker's drain reviews when ReviewDue
			// fires — which may not on a small archive. So point the user at `witness review`
			// to guarantee the facets refresh, rather than silently promising consistency.
			fmt.Println("another distillation worker is already running; it will pick up the re-mine — run `witness review` afterward to refresh this lens's facets/profile")
			return nil
		}
		// --fresh is NOT fine: we already dropped this lens's observations AND facets. The
		// running worker will re-mine L1, but the review that rebuilds the dropped facets +
		// profile is ReviewDue-gated and may never fire on a small/low-poignancy archive —
		// so the lens would be left with empty facets + a stale profile. Don't claim
		// success: report the exact state and the recovery step.
		return fmt.Errorf("backfill %q --fresh incomplete: dropped this lens's observations + facets and reset its watermark, but another distillation worker is already running so the re-mine + review could not run here — once it finishes, run `witness review` to rebuild the facets/profile (or re-run `witness lens backfill %s --fresh`)", name, name)
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
	// Always force a review from the freshly re-mined observations (see the function
	// doc): the drain only reviews on a ReviewDue trigger, which may not fire on a small/
	// low-poignancy archive, so without this a plain backfill's facets would drift from
	// its re-mined observations and a --fresh backfill would be left with the empty facets
	// it dropped. Reviewing unconditionally keeps L2 + L4 consistent with L1 either way.
	fmt.Println("re-mine complete; running a review to refresh this lens's facets + profile…")
	ranReview, err := forceReview(st2)
	if err != nil {
		return fmt.Errorf("backfill %q: review failed; observations were re-mined but facets/profile are not refreshed (run `witness review`): %w", name, err)
	}
	if !ranReview {
		// A worker grabbed the drain lock between our drain and this review. Its review is
		// ReviewDue-gated and may not fire, so the facets could stay stale (or empty, for
		// --fresh) — don't claim success; tell the user how to finish.
		return fmt.Errorf("backfill %q: re-mined observations but another worker took the drain lock before the review could run — facets/profile are not yet refreshed; run `witness review` to finish", name)
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
// clears the field, so the lens rides the default runner/model again.
func newLensSetCmd() *cobra.Command {
	var runner, extractModel, reviewModel string
	c := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a registered lens's per-lens runner + model overrides.",
		Long:  "Tune a registered lens (written to its lens.json). --runner routes this lens's mine+review to a specific runtime (claude/opencode) instead of the default runner; --extract-model overrides the mining (L0→L1) model; --review-model overrides the review (L1→L2) + summary model. Pass an empty value (e.g. --runner \"\") to clear an override so the lens rides the default again. This is what lets a rare heavy lens run a stronger model — or a cheap lens run a free model on another runtime — without paying for it on every session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return lensSet(args[0],
				cmd.Flags().Changed("runner"), runner,
				cmd.Flags().Changed("extract-model"), extractModel,
				cmd.Flags().Changed("review-model"), reviewModel)
		},
	}
	c.Flags().StringVar(&runner, "runner", "", "per-lens runtime (claude/opencode); empty clears → ride the default runner")
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
		name, modelOrDefaultLabel(l.Runner), modelOrDefaultLabel(l.ExtractModel), modelOrDefaultLabel(l.ReviewModel))
	return nil
}

func modelOrDefaultLabel(m string) string {
	if strings.TrimSpace(m) == "" {
		return dim("(default)")
	}
	return m
}

// lensShow prints a lens's settings + its two prompts. Both a registered lens and the
// built-in `default` render the same way (default's settings are hardcoded, not a
// lens.json, but the view is identical), so the output is a consistent, copyable
// definition regardless of source.
func lensShow(st *store.Store, name string) error {
	// Since #44 slice 1a "default" is an ordinary registered lens, so there is no
	// special LoadDefault path — every lens (default included) is looked up in the
	// registry the same way.
	if !slices.Contains(st.RegisteredLenses(), name) {
		return fmt.Errorf("lens %q is not registered (see `witness lens list`)", name)
	}
	l, err := lens.LoadRegistered(name, st.LensesDir())
	if err != nil {
		return fmt.Errorf("read lens %q: %w", name, err)
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

// activeLenses returns every enabled, registered lens — all global, all run on every
// session. Since #44 slice 1a "default" has NO special status: it is an ordinary
// registered lens that appears in EnabledLenses only if it was seeded+enabled (fresh
// tool install or the pre-1a migration) and not since disabled. So an install runs
// exactly the lenses its config enables — including ZERO (a valid state: nothing to
// distill; the queue short-circuits, see store.emptyLensSet). No lens is force-added.
func activeLenses(st *store.Store) ([]*lens.Lens, error) {
	out := []*lens.Lens{}
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
// NAMES the pending/stats queries cross-join sessions against. It returns only the
// lenses that actually LOAD (enabled-and-loadable), matching activeLenses, so the queue
// never offers a (session,lens) pair the worker can't mine — which would otherwise be a
// no-progress cycle (config says active, mining always skips).
//
// Since #44 slice 1a an EMPTY result is legal and correct — an install with no enabled
// lenses distills nothing, and the queue short-circuits cleanly on an empty set (see
// store.emptyLensSet). We must NOT force ["default"] here: default has no always-on
// status, so injecting it when it isn't enabled would offer sessions for a lens that
// never runs (the very no-progress cycle above) and resurrect a lens the user disabled.
// Only a hard error loading the config is unexpected — return empty there too and let
// the drain no-op rather than fabricate a lens set.
func activeLensNames(st *store.Store) []string {
	lenses, err := activeLenses(st)
	if err != nil {
		slog.Warn("could not load active lenses; treating as none this pass", "err", err)
		return nil
	}
	names := make([]string, 0, len(lenses))
	for _, l := range lenses {
		names = append(names, l.Name)
	}
	return names
}
