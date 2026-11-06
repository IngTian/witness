package commands

import (
	"errors"
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
		Use:     "lens",
		GroupID: groupLenses,
		Short:   "Manage observation lenses that extract different views of your sessions.",
		Long: strings.TrimSpace(`
A lens is a point of view for distillation — a set of dimensions + prompts that
decides what witness notices in your sessions. Every enabled lens runs on every
session; an archive can run any number, including none.

The built-in "default" (person-growth) lens is just an ordinary lens: enable,
disable, edit, or remove it like any other. It's added automatically on first
use; 'witness lens load-default' brings it back if you removed it.`),
		RunE: groupRunE,
	}
	lensCmd.AddCommand(
		&cobra.Command{
			Use:   "register <name> <dir>",
			Short: "Register or replace a lens definition.",
			Long:  "Copy a lens definition DIRECTORY (holding lens.json + extract.md + review.md) into the witness store. Later edits to the source directory do not affect the registered snapshot until you register it again. Tune models with `witness config --lens <name>`.",
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
		newLensShowCmd(),
		&cobra.Command{
			Use:   "load-default",
			Short: "Re-seed / restore the built-in \"default\" person-growth lens.",
			Long:  "Register the bundled \"default\" person-growth lens into your archive and enable it, from ANY current state — deregistered, disabled, or already present. This is the explicit restore path for the auto-seeded starter lens: install no longer seeds content (#102), and a deliberately-removed default stays removed, so this command is how you bring it back or re-seed it after editing broke it.",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return cmdLens([]string{"load-default"}) },
		},
		newLensBackfillCmd(),
		newLensTryCmd(),
	)
	return lensCmd
}

// groupRunE is the RunE of a command group (lens, config, observations). A group with no RunE
// printed its help for ANY argument and exited 0 — cobra checks unknown subcommands only on the
// root — so `witness lens enabel x` looked like success to a person and to a script. Bare, it still
// prints help; with an argument it is the same "unknown command" error the root gives, suggestion
// included. (Checked here rather than with Args: cobra.NoArgs, which would also reject the
// alternation prose docs/cli.md writes as `witness lens register|enable|disable|list` and turn
// the doc contract red over prose.)
func groupRunE(c *cobra.Command, args []string) error {
	if len(args) == 0 {
		return c.Help()
	}
	if c.SuggestionsMinimumDistance <= 0 {
		c.SuggestionsMinimumDistance = 2 // cobra's own default, which it applies only on the root
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], c.CommandPath())
	if s := c.SuggestionsFor(args[0]); len(s) > 0 {
		msg += " — did you mean " + strings.Join(s, " or ") + "?"
	}
	return errors.New(msg)
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
		Long: strings.TrimSpace(`
Catch one lens up over your whole history, then refresh its facets.

Re-mines every past session for THIS lens only (others keep their watermark), in
the foreground, and runs a review afterward so the lens's facets + profile match
the re-mined observations. This is the path for a lens you just enabled — cost
scales with one lens × history.

  --fresh   first DELETE this lens's observations + facets, then re-mine from a
            clean slate (for a changed prompt). Raw transcripts are untouched.
            DESTRUCTIVE: mined observations come back, but any you recorded
            in-session are lost. Asks to confirm; --yes skips the prompt.`),
		Args: cobra.ExactArgs(1),
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
	enabled := st.LoadConfig().EnabledLenses
	reg := st.RegisteredLenses()
	switch args[0] {
	case "load-default":
		// Explicit restore, bypassing the first-open one-shot gate: register the bundled
		// default prompts + enable, from any state (deregistered/disabled/present). Since
		// #102 install no longer seeds content and a deliberately-removed default stays
		// gone, this is the sole "bring default back" path.
		if err := seedDefaultLens(st); err != nil {
			return fmt.Errorf("load default lens: %w", err)
		}
		fmt.Println(result(okGlyph(), "loaded the built-in default lens", "registered + enabled"))
	case "register":
		if len(args) < 3 {
			return fmt.Errorf("usage: witness lens register <name> <dir>")
		}
		name, dir := args[1], args[2]
		// The store's own error for a missing directory was a bare "stat …: no such file or
		// directory", which says neither what was wanted nor that the path is the problem.
		const want = "(want lens.json + extract.md + review.md)"
		if fi, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("lens directory %s does not exist %s", dir, want)
		} else if err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("%s is a file, not a lens directory %s", dir, want)
		}
		if err := st.RegisterLens(name, dir); err != nil {
			return err
		}
		note := ""
		if l, err := lens.LoadRegistered(name, st.LensesDir()); err == nil && l.Name != name {
			note = fmt.Sprintf("mines as %q", l.Name)
		}
		fmt.Println(result(okGlyph(), "registered lens "+name, note))
		if !slices.Contains(enabled, name) {
			fmt.Println(hint("turn it on", "witness lens enable "+name))
		}
	case "deregister":
		if len(args) < 2 {
			return fmt.Errorf("usage: witness lens deregister <name>")
		}
		name := args[1]
		if !slices.Contains(reg, name) {
			return fmt.Errorf("lens %q is not registered — nothing to deregister (see `witness lens list`)", name)
		}
		if err := st.DeregisterLens(name); err != nil {
			return err
		}
		// The registry and the enabled set are separate (store.DeregisterLens leaves config
		// alone), so deregistering an enabled lens leaves an orphan the worker skips every pass.
		if slices.Contains(enabled, name) {
			fmt.Println(result(warnGlyph(), "deregistered lens "+name, "still enabled in config.toml"))
			fmt.Println(hint("finish", "witness lens disable "+name))
		} else {
			fmt.Println(result(okGlyph(), "deregistered lens "+name, ""))
		}
		if name == store.LensDefault {
			// It will NOT come back on its own: the first-open seed is one-shot.
			fmt.Println(hint("bring it back", "witness lens load-default"))
		}
	case "enable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens enable <name>")
		}
		name := args[1]
		if !slices.Contains(reg, name) {
			return fmt.Errorf("lens %q is not registered (run: witness lens register %s <dir>)", name, name)
		}
		if slices.Contains(enabled, name) {
			fmt.Println(result(skipGlyph(), "lens "+name+" is already enabled", "nothing changed"))
			return nil
		}
		if err := st.EnableLens(name); err != nil {
			return err
		}
		fmt.Println(result(okGlyph(), "enabled lens "+name, "runs on every session"))
	case "disable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens disable <name>")
		}
		name := args[1]
		if !slices.Contains(enabled, name) {
			// A name that is neither enabled nor registered is a typo, and "disabled lens nope"
			// with exit 0 told the user it worked. Disabling an enabled-but-unregistered name
			// stays allowed: that is how an orphan is cleaned up.
			if !slices.Contains(reg, name) {
				return fmt.Errorf("lens %q is not registered or enabled — nothing to disable (see `witness lens list`)", name)
			}
			fmt.Println(result(skipGlyph(), "lens "+name+" is already disabled", "nothing changed"))
			return nil
		}
		if err := st.DisableLens(name); err != nil {
			return err
		}
		fmt.Println(result(okGlyph(), "disabled lens "+name, "no new work runs it"))
		if name == store.LensDefault {
			fmt.Println(hint("re-enable", "witness lens enable default"))
		}
	case "list":
		renderLensList(st, reg, enabled)
	default:
		return fmt.Errorf("unknown lens subcommand %q (want register|deregister|enable|disable|list|show|load-default|backfill|try)", args[0])
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
	// releaseFresh releases the WorkerLock that --fresh holds across its destructive section.
	// Declared out here (as an idempotent no-op by default) because the lock spans both the
	// `if fresh` block AND the ResetLensWatermark below, but must be released before the drain.
	released := false
	var freshLockRelease func()
	releaseFresh := func() {
		if freshLockRelease != nil && !released {
			released = true
			freshLockRelease()
		}
	}
	if fresh {
		// --fresh is DESTRUCTIVE and #102 folded it in behind a safe-sounding flag, so
		// gate it like every other irreversible op (mirror `witness cleanup`): (1) fail
		// FAST if a worker is live, BEFORE dropping anything — otherwise we delete this
		// lens's obs+facets and then discover we can't re-mine (runWorker returns ran=false),
		// leaving the lens gutted; (2) warn that ACTIVE (hand-recorded) observations are NOT
		// re-derivable from L0 and will be lost — the "safe, re-mined from L0" story only
		// holds for MINED obs; (3) confirm unless --yes. Only then drop.
		// HOLD the lock, don't probe it. WorkerActive is explicitly advisory (see its doc in
		// internal/store/locks.go) and there is an unbounded human pause — the [y/N] prompt —
		// between the probe and the delete, plus ~4 DB queries even under --yes.
		//
		// The race destroys data, verified at the store layer: a worker that starts in that
		// window reads DistilledCount=N into memory; --fresh then deletes the lens's obs/facets
		// and DELETEs its progress rows; the in-flight worker's trailing
		// MarkDistilledIfCurrent re-stamps progress=N with ZERO observations written (its CAS
		// only checks that the raw generation is unchanged, which it is — --fresh never touches
		// raw). Measured outcome: DistilledCount=4, observations=0, PendingSessions=[] — the
		// session is permanently marked fully distilled for this lens with no L1, invisible to
		// every later drain, and the command's own recovery advice (`witness worker review`)
		// cannot bring it back.
		//
		// Taking the real lock closes the window because a worker cannot start while we hold it.
		// It must be released BEFORE runWorker below (see the explicit unlock, not a defer):
		// runWorker takes WorkerLock on a FRESH descriptor in this same process, and flock is
		// per-open-file-description — locks.go documents that a second descriptor is denied even
		// against the process's own held lock — so a deferred unlock would make every --fresh
		// self-deadlock into ran=false and the "another worker took the drain lock" error.
		freshUnlock, gotLock := st.WorkerLock()
		if !gotLock {
			return fmt.Errorf("a distillation worker is running; backfill %q --fresh would drop this lens's data but couldn't re-mine it now — wait until `witness status` is idle, then retry", name)
		}
		freshLockRelease = freshUnlock
		defer releaseFresh() // safety net for the error/abort returns below
		obsAll, facetsN := st.LensDataCounts(minedName)
		active := st.ActiveObservationCount(minedName)
		// The SECOND non-reproducible class: mined obs whose session's L0 was reclaimed by
		// `witness cleanup` (which keeps L1 on purpose). The re-mine enumerates candidates
		// from `raw`, so those sessions are never offered and their obs never come back.
		orphaned := st.OrphanedL0ObservationCount(minedName)
		confLog := st.ConfidenceLogCount(minedName)
		if !assumeYes {
			// Headline + one short line per permanent loss; the prompt is the shared askYesNo, so
			// it accepts y/yes like every other confirmation and says so when nobody can answer.
			fmt.Println()
			fmt.Printf("%s %s\n", warnGlyph(), yellow(fmt.Sprintf("--fresh deletes %s + %s from lens %s, then re-mines it",
				countOf(obsAll, "observation", "observations"), countOf(facetsN, "facet", "facets"), name)))
			if active > 0 {
				fmt.Printf("  %s %s recorded in-session, not mined — no re-mine can re-create them\n", warnGlyph(), humanCount(active))
			}
			if orphaned > 0 {
				fmt.Printf("  %s %s from sessions `witness cleanup` reclaimed — no transcript is left to re-mine\n", warnGlyph(), humanCount(orphaned))
			}
			if confLog > 0 {
				fmt.Printf("  %s %s recorded confidence changes — that history starts over and cannot be re-created\n", warnGlyph(), humanCount(confLog))
			}
			// Only make the re-derivable promise when it actually holds for everything.
			consequence := " Raw transcripts are kept; every observation is re-created from them."
			if active > 0 || orphaned > 0 || confLog > 0 {
				consequence = " Raw transcripts are kept, but the losses listed above are permanent."
			}
			if !askYesNo(bold("Delete and re-mine?") + dim(consequence) + " [y/N]: ") {
				fmt.Println(dim("not deleted — nothing written"))
				return nil
			}
		}
		obs, facets, err := st.DeleteLensData(minedName)
		if err != nil {
			return fmt.Errorf("drop lens %q data: %w", name, err)
		}
		fmt.Println(dim(fmt.Sprintf("dropped %s + %s", countOf(obs, "observation", "observations"), countOf(facets, "facet", "facets"))))
	}
	if _, err := st.ResetLensWatermark(minedName); err != nil {
		return fmt.Errorf("reset lens %q watermark: %w", name, err)
	}
	fmt.Println(dim("re-mining lens " + name + " over the whole history, in the foreground…"))
	// Snapshot the monotonic drift counter before the drain so the completion line can
	// report how many prose_drift events THIS backfill produced (#57) — a below-floor
	// triage model surfaces here, at the moment of the backfill.
	driftBefore := st.DriftTotal()
	// Release the --fresh WorkerLock BEFORE the drain (no-op when !fresh). This ordering is
	// load-bearing, not tidiness: runWorker takes WorkerLock on a fresh descriptor in THIS
	// process, and flock is per-open-file-description, so holding ours would deny the drain its
	// lock and turn every --fresh into ran=false. Releasing here is safe — the destructive
	// section is fully committed, and a worker that starts in this instant sees the already-reset
	// watermark and mines the same delta we were about to.
	releaseFresh()
	// Close our handle before the drain opens its own + takes the WorkerLock. The
	// reset is already committed, so the worker's fresh store snapshot sees it.
	st.Close()

	// A backfill re-mines the lens's whole history — hundreds of sessions, one or more model
	// calls each — so it shows sessions done/total on stderr rather than sitting silent for hours.
	// tap keeps the drain's first runner/embedder/model error so an unfinished backfill can say why.
	tap := &causeTap{}
	ran, err := runWorkerTapped(false, sessionTimeRange{}, name, tap)
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
			// fires — which may not on a small archive. So point the user at `witness worker review`
			// to guarantee the facets refresh, rather than silently promising consistency.
			fmt.Println(result(warnGlyph(), "another worker is already running — it will pick up the re-mine", ""))
			fmt.Println(hint("then refresh", "witness worker review"))
			return nil
		}
		// --fresh is NOT fine: we already dropped this lens's observations AND facets. The
		// running worker will re-mine L1, but the review that rebuilds the dropped facets +
		// profile is ReviewDue-gated and may never fire on a small/low-poignancy archive —
		// so the lens would be left with empty facets + a stale profile. Don't claim
		// success: report the exact state and the recovery step.
		return fmt.Errorf("backfill %q --fresh incomplete: dropped this lens's observations + facets and reset its watermark, but another distillation worker is already running so the re-mine + review could not run here — once it finishes, run `witness worker review` to rebuild the facets/profile (or re-run `witness lens backfill %s --fresh`)", name, name)
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
	if stats.Pending+stats.BackedOff > 0 {
		return backlogUnfinished(name, stats.Pending, stats.BackedOff, false, tap.cause())
	}
	// Always force a review from the freshly re-mined observations (see the function
	// doc): the drain only reviews on a ReviewDue trigger, which may not fire on a small/
	// low-poignancy archive, so without this a plain backfill's facets would drift from
	// its re-mined observations and a --fresh backfill would be left with the empty facets
	// it dropped. Reviewing unconditionally keeps L2 + L4 consistent with L1 either way.
	fmt.Println(dim("re-mine complete; reviewing to refresh the lens's facets + profile…"))
	// A fresh tap: the profile's cause must be the profile's, not an error the drain survived.
	rtap := &causeTap{}
	ranReview, profErr, err := forceReviewTapped(st2, false, rtap)
	if err != nil {
		return fmt.Errorf("backfill %q: review failed; observations were re-mined but facets/profile are not refreshed (run `witness worker review`): %w", name, err)
	}
	if !ranReview {
		// A worker grabbed the drain lock between our drain and this review. Its review is
		// ReviewDue-gated and may not fire, so the facets could stay stale (or empty, for
		// --fresh) — don't claim success; tell the user how to finish.
		return fmt.Errorf("backfill %q: re-mined observations but another worker took the drain lock before the review could run — facets/profile are not yet refreshed; run `witness worker review` to finish", name)
	}
	// The facets landed; a failed narrative is reported the way `worker review` reports it,
	// never as "profile refreshed".
	drifted := st2.DriftTotal() - driftBefore
	if profErr != nil {
		fmt.Println(result(warnGlyph(), "backfilled lens "+name,
			"facets refreshed, but the profile was NOT regenerated (the previous one is kept)"))
		rtap.note(profErr) // a model error during it, if one was seen; else its own (missing prompts …)
		fmt.Println("  " + kvRow("cause", rtap.cause(), ""))
		fmt.Println(hint("diagnose", "witness doctor"))
	} else if drifted == 0 {
		fmt.Println(result(okGlyph(), "backfilled lens "+name, "facets + profile refreshed"))
	}
	// A drifted lens advanced its watermark (not "pending") but distilled to zero
	// observations — report it so a below-floor mine model is visible now.
	if drifted > 0 {
		fmt.Println(result(warnGlyph(), "backfilled lens "+name,
			countOf(drifted, "session", "sessions")+" drifted: the model returned no observations"))
		fmt.Println(hint("raise it", "witness config set mine_model <stronger> --lens "+name))
		fmt.Println(hint("then re-mine", "witness lens backfill "+name+" --fresh"))
	}
	return nil
}

// lensShow prints a lens's settings + its two prompts. Both a registered lens and the
// built-in `default` render the same way (default's settings are hardcoded, not a
// lens.json, but the view is identical), so the output is a consistent, copyable
// definition regardless of source.
// newLensShowCmd builds `witness lens show <name> [--prompts]`. By DEFAULT it prints a
// compact card (settings + a one-line excerpt and line count of each prompt) so the
// common "what is this lens" glance is scannable, not a 120-line wall. --prompts prints
// the two prompts (extract.md + review.md) verbatim — the full, copyable definition you
// want when editing or forking a lens.
func newLensShowCmd() *cobra.Command {
	var prompts bool
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a lens's settings (add --prompts for the full extract/review text).",
		Long:  "Print a lens's settings — dimensions, runner, models — plus a short excerpt of each prompt. Pass --prompts to print the full EXTRACT + REVIEW prompts verbatim (copyable as a starting point for a new lens). Use `default` for the built-in lens.",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return lensShow(args[0], prompts) },
	}
	c.Flags().BoolVar(&prompts, "prompts", false, "print the full extract + review prompts verbatim (default: a short excerpt)")
	return c
}

func lensShow(name string, full bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	// Since #44 slice 1a "default" is an ordinary registered lens, so there is no
	// special LoadDefault path — every lens (default included) is looked up in the
	// registry the same way.
	if !slices.Contains(st.RegisteredLenses(), name) {
		return fmt.Errorf("lens %q is not registered (see `witness lens list`)", name)
	}
	enabled := slices.Contains(st.LoadConfig().EnabledLenses, name)
	l, err := lens.LoadRegistered(name, st.LensesDir())
	if err != nil {
		return fmt.Errorf("read lens %q: %w", name, err)
	}
	if full {
		fmt.Print(renderLensPrompts(l))
	} else {
		fmt.Print(renderLensCard(name, l, enabled, effectiveConfig(st, l)))
	}
	return nil
}

// renderLensCard is the compact default `lens show` view: a settings card + a one-line
// excerpt and line count for each prompt, so a glance fits on screen. The full text is
// one flag away (`--prompts`), pointed to on the hint line.
//
// name is the REGISTERED name — the one every lens command takes. The card used to print the
// lens.json name, so for corpus-example (which mines as "regime") its own footer suggested
// `witness lens show regime --prompts`, which fails with "not registered". The settings rows are
// config list --lens's rows, so the same data reads the same way, effective values included.
func renderLensCard(name string, l *lens.Lens, enabled bool, settings []setting) string {
	var b strings.Builder
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	if l.Name != name {
		state += fmt.Sprintf(" · mines as %q", l.Name)
	}
	fmt.Fprintln(&b, title("lens", name, state))
	fmt.Fprintln(&b)
	if len(l.Dimensions) > 0 {
		// 2 indent + the 14-wide label: continuation lines hang at column 16.
		fmt.Fprintln(&b, "  "+kvRow("dimensions", wrapHanging(strings.Join(l.Dimensions, ", "), 16), ""))
	}
	for _, s := range settings {
		fmt.Fprintln(&b, "  "+kvRow(s.key, settingValue(s), s.from))
	}
	fmt.Fprintln(&b, "  "+kvRow("extract.md", promptExcerpt(l.Extract), ""))
	fmt.Fprintln(&b, "  "+kvRow("review.md", promptExcerpt(l.Review), ""))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, hint("full prompts", "witness lens show "+name+" --prompts"))
	return b.String()
}

// promptExcerpt summarizes a prompt as "<n> lines · \"<first non-empty line, trimmed>\"".
func promptExcerpt(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	n := len(lines)
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	first := ""
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			first = strings.TrimSpace(ln)
			break
		}
	}
	// Cut on runes: a byte cut split a multi-byte character into invalid UTF-8. 48 keeps the row
	// (16-column label + count + quotes) inside an 80-column terminal.
	if r := []rune(first); len(r) > 48 {
		first = string(r[:48]) + glyph("…", "...")
	}
	return fmt.Sprintf("%d lines · %q", n, first)
}

// renderLensList is `witness lens list`. It counts what will actually run — names both registered
// AND enabled — and gives an orphan (enabled in config.toml, but no definition) a row of its own.
// The footer used to count config entries, so "2 lenses · 3 enabled" sat under two rows, and the
// lens the worker skips every pass was invisible.
func renderLensList(st *store.Store, reg, enabled []string) {
	var orphans []string
	for _, name := range enabled {
		if !slices.Contains(reg, name) {
			orphans = append(orphans, name)
		}
	}
	// Since #44 slice 1a "default" has no special status — it is a registered lens like any
	// other, so it appears in this loop naturally. An archive may have ZERO lenses (all
	// disabled / none seeded), a legal "nothing to distill" state.
	if len(reg) == 0 && len(orphans) == 0 {
		fmt.Println(title("lens list", "", ""))
		fmt.Println()
		fmt.Printf("  %s no lenses registered — nothing is distilled\n", skipGlyph())
		fmt.Println(hint("load default", "witness lens load-default"))
		fmt.Println(hint("add your own", "witness lens register <name> <dir>"))
		return
	}
	running := 0
	for _, name := range reg {
		if slices.Contains(enabled, name) {
			running++
		}
	}
	summary := fmt.Sprintf("%s · %d enabled", countOf(len(reg), "lens", "lenses"), running)
	if len(orphans) > 0 {
		summary += " · " + countOf(len(orphans), "orphan", "orphans")
	}
	fmt.Println(title("lens list", "", summary))
	fmt.Println()
	// kvRow's label column is 14 wide, so a longer name (corpus-example is 14) pushed its own
	// value right of every other row's: pad every name to the longest first.
	nameW := 0
	for _, name := range append(slices.Clone(reg), orphans...) {
		nameW = max(nameW, len([]rune(name)))
	}
	row := func(name, value, note string) string {
		return kvRow(name+strings.Repeat(" ", nameW-len([]rune(name))), value, note)
	}
	// The mark column is one character in both modes (warnGlyph's plain "[!]" is three), so a
	// flagged row lines up with the enabled/disabled rows in a pipe too.
	flag := glyph(yellow("!"), "!")
	for _, name := range reg {
		isEnabled := slices.Contains(enabled, name)
		status := "disabled"
		if isEnabled {
			status = "enabled"
		}
		// Best-effort detail. A definition that no longer loads is skipped by the worker, so an
		// enabled one says so rather than reading as healthy.
		l, err := lens.LoadRegistered(name, st.LensesDir())
		switch {
		case err != nil && isEnabled:
			fmt.Printf("  %s %s\n", flag, row(name, yellow("enabled, but its definition does not load"), ""))
		case err != nil:
			fmt.Printf("  %s %s\n", enabledGlyph(false), row(name, status, "definition does not load"))
		default:
			note := countOf(len(l.Dimensions), "dim", "dims")
			if l.Name != name {
				note += fmt.Sprintf(" · mines as %q", l.Name)
			}
			fmt.Printf("  %s %s\n", enabledGlyph(isEnabled), row(name, status, note))
		}
	}
	for _, name := range orphans {
		fmt.Printf("  %s %s\n", flag, row(name, yellow("enabled, but not registered"), ""))
	}
	fmt.Println()
	for _, name := range orphans {
		fmt.Println(hint("clean up", "witness lens disable "+name))
	}
	fmt.Println(hint("inspect one", "witness lens show <name>"))
}

// lensDataName maps the name a lens is registered under (the one every command takes) to the name
// the worker tags its observations, facets and profile with — the lens.json `name`, which may
// differ: corpus-example mines as "regime". A name that is not registered, or does not load, is
// returned as given, so data that outlived its lens's registration stays readable by its own name.
func lensDataName(st *store.Store, name string) string {
	if !slices.Contains(st.RegisteredLenses(), name) {
		return name
	}
	if l, err := lens.LoadRegistered(name, st.LensesDir()); err == nil {
		return l.Name
	}
	return name
}

// lensRegisteredName is lensDataName in reverse, for display: the registered name of the lens that
// mines under `mined`, or `mined` itself when no registered lens does.
func lensRegisteredName(st *store.Store, mined string) string {
	reg := st.RegisteredLenses()
	if slices.Contains(reg, mined) && lensDataName(st, mined) == mined {
		return mined
	}
	for _, name := range reg {
		if l, err := lens.LoadRegistered(name, st.LensesDir()); err == nil && l.Name == mined {
			return name
		}
	}
	return mined
}

// renderLensPrompts is the `--prompts` view: the two prompts verbatim, under plain
// section rules, so the output stays copyable as a STARTING POINT for a new lens
// directory. A settings line precedes them for context.
func renderLensPrompts(l *lens.Lens) string {
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
