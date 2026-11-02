package distill

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Compaction: keep the ACTIVE profile a minimal surface that still describes the person.
//
// WHY THIS EXISTS. Facets were immortal. Nothing in the codebase could remove or combine one,
// and no prompt ever asked whether two facets were the same thing, so a profile could only
// grow. Measured on a real archive: 2,039 facets in one lens, 2,330 of 2,759 overall with
// exactly one version, 495 in a single dimension. Of those 2,039, only 823 were ever the
// nearest match to any of 2,500 sampled observations — 1,216 described nothing — and 86 facets
// accounted for half the corpus. Greedy coverage put a 50-150 facet set within a few percent of
// what all 2,039 achieve. The profile was not rich, it was redundant.
//
// WHAT IT DOES. Two actions, both reversible-by-inspection because nothing is deleted:
//
//	merge   several facets that are one underlying trait collapse into a single facet whose
//	        value is REWRITTEN to carry what each contributed. The survivor may be an existing
//	        key or a new, more abstract one — this is rebalancing, not concatenation.
//	retire  a facet that is noise, an artifact, or too specific to be a trait leaves the
//	        active set.
//
// Archived facets keep every version. The active profile is the derived subset
// (ReadActiveFacets), and MergedInto links each archived facet to its survivor, so any node in
// the compact set unfolds into the full trajectory behind it (store.FacetTrajectory).
//
// WHY THE MODEL DOES THE GROUPING AND NOT COSINE. The obvious design — cluster facet
// embeddings, merge tight clusters — was measured dead on real data. On this corpus every
// facet's nearest OTHER facet sits at median 0.927, p10 0.9076, and ALL 2,039 have a neighbour
// at 0.85+; only 25 have one at 0.95+. Facet values are denser among themselves (0.927) than
// observations are to their own best-matching facet (0.872), because they are all same-register
// analytical prose about one person. Cosine cannot separate "duplicate" from "distinct claim"
// here, so a similarity threshold would merge unrelated traits. Batching by dimension and
// letting the model judge costs more calls but is the only version that can be correct — and it
// skips a 29-minute embedding pass over the facet set.
//
// SAFETY. Every action is validated in Go before anything is written (validate): the model can
// only act on facets it was actually shown, cannot touch another lens or dimension, cannot
// absorb a facet twice, cannot merge a single facet (that is a rename), and cannot archive more
// than compactMaxArchiveFraction of a lens in one pass. This mirrors the division of labour in
// applyFacet — the model judges what is the same, code enforces what may be done about it.
const (
	// compactBatchChars bounds one compaction call's facet payload. Sized like
	// ReviewMaxChars: well inside the runner's 10-minute wall, since the reply is long
	// (merged values are rewritten prose, not verdicts). ~50 average facets per batch.
	compactBatchChars = 60_000

	// compactMinSurvivors is the real guard: how few facets a DIMENSION may be left with.
	//
	// It replaced a cap on the archived FRACTION, which was the wrong shape and measurably so.
	// A fraction limits how much is removed, but the damage case is not "removed a lot" — it is
	// "left almost nothing", and the intended result IS a large removal: the measured target is
	// a ~90% reduction, since a dimension of a hundred facets is really about ten dispositions
	// described repeatedly. A 50% cap refused a correct 29-of-33 plan outright, so the guard was
	// blocking the goal instead of the failure.
	//
	// A floor on survivors encodes the actual risk directly. A model that collapses a whole
	// dimension into one or two facets has lost the distinctions, whatever its reasoning; one
	// that reduces 105 facets to 13 has done the job. Per DIMENSION rather than per lens,
	// because collapse is a within-dimension failure and a lens-wide total hides it.
	compactMinSurvivors = 3

	// compactCollapseGuardMin is the dimension size at which the survivor floor starts to
	// apply. Below it, collapsing to a single facet is a plausible correct answer rather than a
	// runaway, and the dry run shows the entire plan anyway.
	compactCollapseGuardMin = 10

	// compactMinAbsorb is the floor for a merge. Absorbing exactly one facet is a RENAME —
	// it moves a facet's identity without combining anything, which loses the old key with no
	// consolidation gain. Renames are not what compaction is for, so they are refused.
	compactMinAbsorb = 2
)

// compactAction is one proposed structural change. A single JSON array carries both kinds so
// the reply parses with the same ParseJSONArray helper every other phase uses.
type compactAction struct {
	Action     string   `json:"action"`     // "merge" | "retire"
	Dimension  string   `json:"dimension"`  // must be the batch's dimension
	Key        string   `json:"key"`        // merge: the survivor; retire: the facet to archive
	Value      string   `json:"value"`      // merge only: the rewritten value
	Claim      string   `json:"claim"`      // merge only: the short statement of the trait
	Absorb     []string `json:"absorb"`     // merge only: keys folded into Key
	Why        string   `json:"why"`        // recorded in the log, and shown in a dry run
	Confidence float64  `json:"confidence"` // merge only: confidence in the survivor
}

// CompactPlan is what one lens's pass proposes. Produced by Plan (which writes nothing) and
// consumed by Apply, so `witness compact` can show the plan before committing to it.
type CompactPlan struct {
	Lens    string          `json:"lens"`
	Actions []compactAction `json:"actions"`
	// Before is the lens's active facet count at plan time; Archived is how many facets the
	// plan would take out of the active set. Reported so a dry run states the shape of the
	// change in one line.
	Before   int `json:"before"`
	Archived int `json:"archived"`
	// Skipped counts actions the validator refused, by reason — a prompt regression shows up
	// here rather than silently shrinking the plan.
	Skipped map[string]int `json:"skipped,omitempty"`
	// InputChars / OutputChars are the merged groups' total size before and after: the
	// SECOND thing a reader needs, because facet count and payload size move independently.
	// A first real run cut the facet count 70% while DOUBLING the characters (merged values
	// came out at 1.98x their inputs), which makes the review payload worse, not better —
	// and nothing in the output said so. Reporting both makes that visible instead of
	// discoverable only by querying the database afterwards.
	InputChars  int `json:"merged_input_chars,omitempty"`
	OutputChars int `json:"merged_output_chars,omitempty"`
	// Failed is how many batches errored (a model timeout, an unparseable reply). The plan is
	// still valid for everything else, but it is PARTIAL — surfaced so "nothing more to
	// compact" is never confused with "half the calls did not run".
	Failed int `json:"failed,omitempty"`
}

// Compactor rebalances the active facet surface of each lens. It is the only writer of
// ArchivedAt/MergedInto.
type Compactor struct {
	Store     store.CompactStore
	Lenses    []*lens.Lens
	Config    store.Config
	Runner    MineFunc
	RunnerFor func(ln *lens.Lens) MineFunc

	// The three knobs that decide how hard compaction consolidates. Zero means "use the
	// package default", so production wires none of them; they exist because how aggressive to
	// be is an empirical question, not a thing to guess once and bake in. Measured answers
	// live in compactstrategy.go.
	Prompt       string // "" → compactGroupPrompt (stage 1)
	WritePrompt  string // "" → compactWritePrompt (stage 2)
	BatchChars   int    // 0 → compactBatchChars
	MinSurvivors int    // 0 → compactMinSurvivors

	// decaySeq is the observation high-water mark, read once by Plan, so every confidence the
	// model is shown is the CURRENT (decayed) one. Both stages used to render the stored value:
	// a facet stored at 0.90 but uncited for 4,400 observations (really ~0.33) sat beside a fresh
	// 0.59 duplicate looking like the stronger claim, the model returned ~0.90 for the survivor,
	// and Apply gave that survivor the fresher member's clock — so a stale confidence came out
	// fresh. The review already sends decayed values (slimFacets); compaction now agrees with it.
	decaySeq int64
}

func (c *Compactor) batchChars() int {
	if c.BatchChars > 0 {
		return c.BatchChars
	}
	return compactBatchChars
}

func (c *Compactor) minSurvivors() int {
	if c.MinSurvivors > 0 {
		return c.MinSurvivors
	}
	return compactMinSurvivors
}

func (c *Compactor) runnerFor(ln *lens.Lens) MineFunc {
	return resolveRunner(c.RunnerFor, ln, c.Runner)
}

// Plan proposes compaction for every lens WITHOUT writing anything. Model calls happen here;
// Apply is pure local bookkeeping. Splitting them is what makes a dry run honest — the plan a
// user approves is the exact plan that gets applied, not a re-derived one.
func (c *Compactor) Plan(ctx context.Context) ([]CompactPlan, error) {
	all, err := c.Store.ReadFacets()
	if err != nil {
		return nil, fmt.Errorf("read L2: %w", err)
	}
	if c.decaySeq, err = c.Store.ObservationHighWater(); err != nil {
		return nil, fmt.Errorf("read observation high-water: %w", err)
	}
	var plans []CompactPlan
	for _, ln := range c.Lenses {
		plan, err := c.planLens(ctx, ln, all)
		if err != nil {
			// One lens's failure must not strand the others: compaction is maintenance, and a
			// partial plan is still useful. Surface it and continue.
			slog.Error("compact: lens planning failed", "lens", ln.Name, "err", err)
			continue
		}
		// Keep a plan that found nothing but FAILED or REFUSED something. Dropping it would
		// report "nothing to compact" for a lens whose calls never ran, or whose every
		// proposal was rejected by the validator — and "your profile is already tight" is the
		// one wrong conclusion a maintenance command must not hand back. A pile of refusals is
		// how a prompt regression announces itself.
		if len(plan.Actions) > 0 || plan.Failed > 0 || len(plan.Skipped) > 0 {
			plans = append(plans, plan)
		}
	}
	return plans, nil
}

func (c *Compactor) planLens(ctx context.Context, ln *lens.Lens, all []store.Facet) (CompactPlan, error) {
	active := activeForLens(all, ln.Name)
	plan := CompactPlan{Lens: ln.Name, Before: len(active), Skipped: map[string]int{}}
	if len(active) == 0 {
		return plan, nil
	}
	// Index the whole lens once: validation needs to know what exists and what is active.
	byKey := make(map[string]store.Facet, len(active))
	for _, f := range active {
		byKey[store.FacetRef(f.Dimension, f.Key)] = f
	}
	claimed := map[string]bool{} // a facet may take part in at most one action per pass

	for _, dim := range dimensionsOf(active) {
		inDim := facetsInDimension(active, dim)
		var grouped []compactAction // stage-1 output for THIS dimension, awaiting values
		for _, batch := range compactBatches(inDim, c.batchChars()) {
			actions, err := c.groupBatch(ctx, ln, dim, batch)
			if err != nil {
				// A failed batch must NOT discard the batches that already succeeded. This
				// aborted the whole lens once and threw away 12 valid actions from 5 good
				// batches because the 6th hit a transient auth timeout — a 45-call lens would
				// never complete if any one call failing cost the entire pass. Same discipline
				// as the review path, which keeps committed windows and resumes. A plan is
				// just a proposal (Plan writes nothing), so a partial one is safe AND useful:
				// apply what was found, re-run for the rest.
				slog.Error("compact: batch failed (keeping the actions already planned)",
					"lens", ln.Name, "dimension", dim, "facets", len(batch), "err", err)
				plan.Failed++
				continue
			}
			for _, a := range actions {
				if reason := validateGrouping(a, dim, batch, byKey, claimed); reason != "" {
					plan.Skipped[reason]++
					continue
				}
				// Claim every facet the action consumes so a later batch or a duplicate
				// entry cannot act on it again.
				switch a.Action {
				case "merge":
					for _, k := range a.Absorb {
						claimed[store.FacetRef(dim, k)] = true
					}
					claimed[store.FacetRef(dim, a.Key)] = true
				case "retire":
					claimed[store.FacetRef(dim, a.Key)] = true
				}
				a.Dimension = dim // normalize: validate accepted it, store it canonically
				grouped = append(grouped, a)
			}
		}

		// STAGE 2 — write one merged value per group, each in its own bounded call.
		//
		// Runs per DIMENSION, after all of that dimension's groups are known, because a group
		// cannot be written well in isolation: a call that sees only its own nine facets is
		// blind to what the dimension's other survivors cover, so it bleeds into their
		// territory and re-creates the overlap compaction exists to remove. The sibling list
		// from stage 1 is exactly the boundary information needed, and it is already in hand.
		written, failures := c.writeValues(ctx, ln, dim, grouped, byKey, plan.Skipped)
		plan.Failed += failures

		// Collapse guard, per dimension and BEFORE the actions are accepted: count what this
		// dimension would be left with. Archiving a lot is the goal; being left with almost
		// nothing is the failure.
		survivors := len(inDim)
		for _, a := range written {
			if a.Action == "merge" {
				survivors -= len(a.Absorb)
				if _, existed := byKey[store.FacetRef(dim, a.Key)]; !existed {
					survivors++ // a new abstraction key adds a facet
				}
			} else {
				survivors--
			}
		}
		floor := c.minSurvivors()
		if len(inDim) < compactCollapseGuardMin {
			// No floor on a small dimension. It may legitimately turn out to BE one
			// disposition, and a dimension holding a single junk facet should be emptiable —
			// retiring it loses no distinction, which is the only thing this guard protects.
			// Enforcing any floor here repeats the mistake the fraction cap made: blocking the
			// work compaction exists to do. The dry run shows the entire plan at this size.
			floor = 0
		}
		if survivors < floor {
			slog.Error("compact: refusing a plan that would collapse a dimension",
				"lens", ln.Name, "dimension", dim, "facets", len(inDim),
				"would_survive", survivors, "floor", floor)
			plan.Skipped["would_collapse_the_dimension"] += len(written)
			continue
		}
		plan.Actions = append(plan.Actions, written...)
	}

	// Archived is counted from the FINAL accepted actions, not during grouping: a group whose
	// stage-2 write failed or was refused is dropped, and its facets must not be reported as
	// about to be archived.
	for _, a := range plan.Actions {
		if a.Action != "merge" {
			plan.Archived++
			continue
		}
		plan.Archived += len(a.Absorb)
		// Size accounting, so "fewer facets" is never reported without "and how much text".
		plan.OutputChars += len(a.Value)
		if surv, ok := byKey[store.FacetRef(a.Dimension, a.Key)]; ok {
			if cur := surv.Current(); cur != nil {
				plan.InputChars += len(cur.Value)
			}
		}
		for _, k := range a.Absorb {
			if f, ok := byKey[store.FacetRef(a.Dimension, k)]; ok {
				if cur := f.Current(); cur != nil {
					plan.InputChars += len(cur.Value)
				}
			}
		}
	}

	slog.Info("compact: planned", "lens", ln.Name, "active", plan.Before,
		"actions", len(plan.Actions), "would_archive", plan.Archived,
		"skipped", plan.Skipped, "failed_batches", plan.Failed)
	return plan, nil
}

// groupBatch is STAGE 1: one model call that decides which facets are the same disposition and
// returns ONLY names. No values.
//
// The split is not stylistic. Measured on a real 105-facet dimension: asking one call to decide
// the grouping AND write every merged value produced ~40,000 chars of output and was KILLED at
// the runner's 10-minute wall, every time, for three different prompts — while the shipped
// conservative prompt survived only because it merged pairs (9m57s, three seconds from the same
// cliff). Generation time scales with output, so single-call compaction cannot merge deeply at
// any prompt. Grouping alone returned 8,191 chars in 3m17s and proposed merges averaging 9.2
// facets, projecting an 87.6% reduction. Large INPUT is cheap; large OUTPUT is what costs.
func (c *Compactor) groupBatch(ctx context.Context, ln *lens.Lens, dim string, batch []store.Facet) ([]compactAction, error) {
	input := renderCompactBatch(ln.Name, dim, batch, c.decaySeq)
	reply, err := c.runnerFor(ln)(ctx, ModelFor(c.Config, ln, PhaseReview), c.groupPrompt(), input)
	if err != nil {
		return nil, err
	}
	actions, err := ParseJSONArray[compactAction](reply)
	if err != nil {
		return nil, err
	}
	slog.Info("compact: grouped", "lens", ln.Name, "dimension", dim, "facets", len(batch),
		"input_chars", len(input), "reply_chars", len(reply), "proposed", len(actions))
	return actions, nil
}

// writeValues is STAGE 2: one call per merge group, filling in the merged value. Retirements
// pass straight through — they have no value to write.
//
// Returns the actions that are ready to apply, plus how many groups failed. A group whose write
// fails is DROPPED rather than applied with no value: an empty value would blank the survivor's
// stance, which is the same class of damage wellFormed guards on the review path.
func (c *Compactor) writeValues(ctx context.Context, ln *lens.Lens, dim string,
	grouped []compactAction, byKey map[string]store.Facet, skipped map[string]int) ([]compactAction, int) {

	var out []compactAction
	failures := 0
	for _, a := range grouped {
		if a.Action != "merge" {
			out = append(out, a)
			continue
		}
		members := make([]store.Facet, 0, len(a.Absorb)+1)
		if surv, ok := byKey[store.FacetRef(dim, a.Key)]; ok {
			members = append(members, surv) // an existing survivor's own value is input too
		}
		for _, k := range a.Absorb {
			if f, ok := byKey[store.FacetRef(dim, k)]; ok {
				members = append(members, f)
			}
		}
		claim, value, err := c.writeOne(ctx, ln, dim, a, members, grouped)
		if err != nil {
			slog.Error("compact: writing a merged value failed (group dropped, others kept)",
				"lens", ln.Name, "dimension", dim, "survivor", a.Key, "absorbing", len(a.Absorb), "err", err)
			failures++
			continue
		}
		a.Value, a.Claim = value, claim
		if reason := validateMergedValue(a, members); reason != "" {
			skipped[reason]++
			continue
		}
		out = append(out, a)
	}
	return out, failures
}

// writeOne runs the single call that produces one merged facet: a short CLAIM and the full
// value.
func (c *Compactor) writeOne(ctx context.Context, ln *lens.Lens, dim string,
	group compactAction, members []store.Facet, siblings []compactAction) (claim, value string, err error) {

	input := renderMergeInput(ln.Name, dim, group, members, siblings, c.decaySeq)
	reply, err := c.runnerFor(ln)(ctx, ModelFor(c.Config, ln, PhaseReview), c.writePrompt(), input)
	if err != nil {
		return "", "", err
	}
	// The reply is PROSE, not JSON: a merged value is long free text, and wrapping it in JSON
	// only adds an escaping failure mode (one stray newline or quote in a 4,000-char value
	// breaks the parse and loses the whole call). So the two parts are split on a delimiter.
	claim, value = splitClaim(stripFence(reply))
	slog.Info("compact: wrote a merged facet", "lens", ln.Name, "dimension", dim,
		"survivor", group.Key, "absorbed", len(group.Absorb), "input_chars", len(input),
		"claim_chars", len(claim), "value_chars", len(value))
	return claim, value, nil
}

// splitClaim parses the stage-2 reply into its short claim and full value.
//
// Degrades deliberately rather than failing: a reply with no delimiter becomes value-only with
// an empty claim, which every consumer already handles by falling back to the value. Losing a
// whole merge because the model forgot three dashes would be a far worse trade than carrying on
// without the short form.
func splitClaim(reply string) (claim, value string) {
	lines := strings.Split(reply, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "---" {
			continue
		}
		head := strings.TrimSpace(strings.Join(lines[:i], "\n"))
		head = strings.TrimSpace(strings.TrimPrefix(head, "CLAIM:"))
		body := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		if head == "" || body == "" {
			break // a delimiter with nothing either side is not a split; treat it as prose
		}
		return head, body
	}
	return "", strings.TrimSpace(reply)
}

// stripFence removes a surrounding markdown code fence if the model added one despite being
// told not to. Cheap, and the alternative is a value that begins with ```.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// validateGrouping checks a STAGE-1 action: the structure of a proposed group, before any value
// exists. It returns "" when the grouping may proceed, else a short reason used as a
// skip-tally key. Every rule here exists because the alternative is a model reply silently
// reshaping a profile in a way the user cannot see or undo.
func validateGrouping(a compactAction, dim string, batch []store.Facet, byKey map[string]store.Facet, claimed map[string]bool) string {
	inBatch := make(map[string]bool, len(batch))
	for _, f := range batch {
		inBatch[f.Key] = true
	}
	// A dimension mismatch means the model is acting outside what it was shown. Compaction
	// groups WITHIN a dimension, so this is always a mistake rather than an ambitious merge.
	if a.Dimension != "" && a.Dimension != dim {
		return "wrong_dimension"
	}
	if strings.TrimSpace(a.Key) == "" {
		return "empty_key"
	}

	switch a.Action {
	case "retire":
		if !inBatch[a.Key] {
			return "retire_unseen_facet"
		}
		if claimed[store.FacetRef(dim, a.Key)] {
			return "already_claimed"
		}
		return ""

	case "merge":
		// No value check here: stage 1 returns names only, and the value is written and
		// validated separately (validateMergedValue).
		if len(a.Absorb) < compactMinAbsorb {
			return "merge_too_small"
		}
		// The survivor is either an existing facet in this batch, or a NEW key (true
		// abstraction). It must not be a facet that exists but was NOT shown — that would
		// rewrite a value the model never read.
		survivorRef := store.FacetRef(dim, a.Key)
		if _, exists := byKey[survivorRef]; exists && !inBatch[a.Key] {
			return "survivor_unseen"
		}
		if claimed[survivorRef] {
			return "already_claimed"
		}
		seen := map[string]bool{}
		for _, k := range a.Absorb {
			if strings.TrimSpace(k) == "" {
				return "empty_absorb_key"
			}
			if k == a.Key {
				// Absorbing the survivor into itself would archive the facet the merge is
				// supposed to keep, leaving the trait with no active representative.
				return "absorb_includes_survivor"
			}
			if !inBatch[k] {
				return "absorb_unseen_facet"
			}
			if seen[k] {
				return "absorb_duplicate"
			}
			if claimed[store.FacetRef(dim, k)] {
				return "already_claimed"
			}
			seen[k] = true
		}
		return ""
	}
	return "unknown_action"
}

// validateMergedValue checks a STAGE-2 result: the value that will become the survivor's
// current stance. Two refusals, and the second is the one that matters.
//
// An EMPTY value would blank the survivor — the same class of damage wellFormed guards on the
// review path.
//
// A value SHORTER than the longest facet it absorbs is almost certainly a summary rather than a
// consolidation. That is the central risk of this whole feature: a merge that reads well while
// quietly dropping nine facets' worth of specifics turns "90% fewer facets" into data loss
// dressed as a win. Consolidating N facets honestly needs at least as much room as the largest
// one took alone, so this catches the lossy case cheaply and without judging prose. It can
// refuse a legitimately tight merge of highly redundant facets; that costs one skipped group,
// reported in the tally, and the facets stay exactly as they were.
func validateMergedValue(a compactAction, members []store.Facet) string {
	if strings.TrimSpace(a.Value) == "" {
		return "empty_merged_value"
	}
	longest := 0
	for _, m := range members {
		if cur := m.Current(); cur != nil && len(cur.Value) > longest {
			longest = len(cur.Value)
		}
	}
	if len(a.Value) < longest {
		return "merged_value_shorter_than_its_largest_input"
	}
	return ""
}

// Apply commits a plan. It does no model work and no judgment — every decision was made and
// validated in Plan, so this is deterministic bookkeeping over the facet slice.
//
// It reads the WHOLE profile (archived facets included) because WriteFacets replaces the table:
// handing it only the active set would delete every previously archived facet and destroy the
// history this feature exists to keep.
func (c *Compactor) Apply(ctx context.Context, plans []CompactPlan, now time.Time) error {
	if len(plans) == 0 {
		return nil
	}
	nowStr := now.UTC().Format(time.RFC3339)
	all, err := c.Store.ReadFacets()
	if err != nil {
		return fmt.Errorf("read L2: %w", err)
	}
	byKey := indexFacets(all)

	applied, archived := 0, 0
	for _, plan := range plans {
		for _, a := range plan.Actions {
			id := plan.Lens + "|" + a.Dimension + "|" + a.Key
			switch a.Action {
			case "retire":
				f, ok := byKey[id]
				if !ok {
					continue // the profile moved under us (a review landed); skip, re-plan
				}
				f.ArchivedAt = nowStr
				f.MergedInto = "" // retired outright: no survivor to point at
				f.ArchiveReason = a.Why
				archived++

			case "merge":
				// Provenance first: the survivor's new value is justified by everything the
				// absorbed facets were justified by, so the merged version carries the union
				// of their observation ids. Without this a merge would orphan the evidence.
				var because []string
				// The decay clock (#173), carried the same way as provenance. The survivor
				// speaks for every facet it absorbs, so it was last CONFIRMED when the freshest
				// of them was. Not "now": a rewrite is not new evidence, and restarting the
				// clock would hand every compacted facet a free confidence refresh. And not
				// left at 0, which DecayedConfidence reads as "no clock" — that froze every
				// facet compaction touched, and the ones that never recur are exactly the
				// ones decay exists for.
				var freshest int64
				absorbed := 0
				for _, k := range a.Absorb {
					src, ok := byKey[plan.Lens+"|"+a.Dimension+"|"+k]
					if !ok {
						continue
					}
					if cur := src.Current(); cur != nil {
						because = mergeIDs(because, cur.BecauseOf)
						freshest = max(freshest, cur.ConfidenceSeq)
					}
					src.ArchivedAt = nowStr
					src.MergedInto = store.FacetRef(a.Dimension, a.Key)
					src.ArchiveReason = a.Why
					absorbed++
					archived++
				}
				if absorbed < compactMinAbsorb {
					// The facets vanished between plan and apply; a merge that absorbs
					// nothing would just rewrite a value with no consolidation.
					continue
				}
				f, ok := byKey[id]
				if !ok {
					// A new abstraction node: the survivor key did not exist.
					byKey[id] = &store.Facet{
						Lens: plan.Lens, Dimension: a.Dimension, Key: a.Key, LastSeen: nowStr,
						Versions: []store.FacetVersion{{
							Value: a.Value, Claim: a.Claim, ValidFrom: nowStr, RecordedAt: nowStr,
							BecauseOf: because, Confidence: clampConf(a.Confidence),
							ConfidenceSeq: freshest,
						}},
					}
					applied++
					continue
				}
				// An existing survivor's value is REWRITTEN to cover more ground, which is a
				// genuine change arc: close the old version, open the new one. The old
				// wording stays readable in the facet's own history.
				if cur := f.Current(); cur != nil {
					because = mergeIDs(because, cur.BecauseOf)
					freshest = max(freshest, cur.ConfidenceSeq)
					if cur.Value != a.Value {
						f.Versions[len(f.Versions)-1].ValidTo = nowStr
						f.Versions = append(f.Versions, store.FacetVersion{
							Value: a.Value, Claim: a.Claim, ValidFrom: nowStr, RecordedAt: nowStr,
							BecauseOf: because, Confidence: clampConf(a.Confidence),
							ConfidenceSeq: freshest,
						})
					} else {
						cur.BecauseOf = because
						cur.ConfidenceSeq = freshest
						// Same value, but a claim may be NEW: an existing facet whose text
						// did not change still gains its short form on first compaction,
						// which is how a profile acquires claims without a rewrite.
						if strings.TrimSpace(cur.Claim) == "" {
							cur.Claim = a.Claim
						}
					}
				} else {
					f.Versions = append(f.Versions, store.FacetVersion{
						Value: a.Value, Claim: a.Claim, ValidFrom: nowStr, RecordedAt: nowStr,
						BecauseOf: because, Confidence: clampConf(a.Confidence),
						ConfidenceSeq: freshest,
					})
				}
				f.LastSeen = nowStr
				f.ArchivedAt = "" // a survivor is active by definition
				f.MergedInto = ""
				applied++
			}
		}
	}
	if err := c.Store.WriteFacets(collectFacets(byKey)); err != nil {
		return fmt.Errorf("write L2: %w", err)
	}
	slog.Info("compact: applied", "plans", len(plans), "merges_and_retires", applied, "archived", archived)
	return nil
}

// --- batching -----------------------------------------------------------------

func activeForLens(all []store.Facet, lensName string) []store.Facet {
	var out []store.Facet
	for _, f := range all {
		if f.Lens == lensName && f.Active() && f.Current() != nil {
			out = append(out, f)
		}
	}
	return out
}

func dimensionsOf(facets []store.Facet) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range facets {
		if !seen[f.Dimension] {
			seen[f.Dimension] = true
			out = append(out, f.Dimension)
		}
	}
	sort.Strings(out) // deterministic order so a dry run is reproducible
	return out
}

func facetsInDimension(facets []store.Facet, dim string) []store.Facet {
	var out []store.Facet
	for _, f := range facets {
		if f.Dimension == dim {
			out = append(out, f)
		}
	}
	// Sort by key so related facets land in the SAME batch: keys are descriptive snake_case
	// slugs the model minted, so lexical adjacency correlates with topical adjacency
	// (grounds_state_in_verification next to grounds_state_in_measurement). A free heuristic
	// that raises the chance a duplicate pair is visible in one call — and compaction is
	// re-runnable, so a pair split across batches is merged on a later pass rather than lost.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// compactBatches splits one dimension's facets into char-bounded batches, always at least one
// facet per batch (the lone-giant floor windowEnd uses).
func compactBatches(facets []store.Facet, budget int) [][]store.Facet {
	var out [][]store.Facet
	cur := []store.Facet{}
	size := 0
	for _, f := range facets {
		n := len(f.Current().Value)
		if len(cur) > 0 && size+n > budget {
			out = append(out, cur)
			cur, size = []store.Facet{}, 0
		}
		cur = append(cur, f)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// renderMergeInput is the stage-2 payload for ONE group: the facets being combined, in full,
// plus the BOUNDARIES of the dimension's other survivors.
//
// The sibling list is the part that is easy to leave out and wrong to. A call that sees only
// its own members writes a facet blind to what the other survivors cover, so it drifts into
// their territory and the dimension ends up with coarse-grained overlap — the same redundancy
// compaction removed, re-created one level up. Names and one-line reasons are enough to fence
// each call; their full values are not needed and would blow the payload back up.
func renderMergeInput(lensName, dim string, group compactAction, members []store.Facet, siblings []compactAction, decaySeq int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LENS: %s\nDIMENSION: %s\n\n", lensName, dim)
	fmt.Fprintf(&b, "THE TRAIT YOU ARE WRITING: %s\nWHY THESE WERE GROUPED: %s\n\n", group.Key, group.Why)

	fmt.Fprintf(&b, "FACETS TO COMBINE (%d) — everything below must survive in your value:\n\n", len(members))
	for _, m := range members {
		cur := m.Current()
		if cur == nil {
			continue
		}
		fmt.Fprintf(&b, "--- %s (confidence %.2f, %d version(s), last_seen %s)\n%s\n\n",
			m.Key, cur.DecayedConfidence(decaySeq), len(m.Versions), m.LastSeen, cur.Value)
	}

	var others []compactAction
	for _, s := range siblings {
		if s.Action == "merge" && s.Key != group.Key {
			others = append(others, s)
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(&b, "OTHER TRAITS IN THIS DIMENSION (%d), each being written by a separate call.\n"+
			"Do NOT cover their ground — if something belongs to one of these, leave it to them:\n\n", len(others))
		for _, s := range others {
			fmt.Fprintf(&b, "  %s — %s\n", s.Key, s.Why)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderCompactBatch is the model-facing view of one dimension's batch. Version count and
// confidence are included because they are the cheap signals for "is this load-bearing": a
// facet that has never changed and sits at low confidence after many sessions is a retirement
// candidate, while one with a real change history is carrying an arc worth keeping.
func renderCompactBatch(lensName, dim string, batch []store.Facet, decaySeq int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LENS: %s\nDIMENSION: %s\nFACETS: %d\n\n", lensName, dim, len(batch))
	for _, f := range batch {
		cur := f.Current()
		fmt.Fprintf(&b, "key: %s\nconfidence: %.2f\nversions: %d\nlast_seen: %s\nvalue: %s\n\n",
			f.Key, cur.DecayedConfidence(decaySeq), len(f.Versions), f.LastSeen, cur.Value)
	}
	return b.String()
}
