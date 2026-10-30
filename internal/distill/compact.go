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

	// compactMaxArchiveFraction caps how much of a lens's active profile ONE pass may
	// archive. A model that mass-merges on a bad batch would otherwise flatten a profile in a
	// single command. Compaction is idempotent and re-runnable, so a cap costs only another
	// pass; without it the blast radius is the whole profile.
	compactMaxArchiveFraction = 0.5

	// compactMinArchiveAllowance is the floor under that fraction, and it exists because a
	// pure percentage makes small profiles impossible to compact: a 3-facet lens cannot merge
	// 2 of them (67%), and a 6-facet lens cannot consolidate 4 duplicates — exactly the
	// legitimate work compaction is for. The fraction is there to bound the blast radius on a
	// LARGE profile, where a runaway would be unreviewable; at this scale the whole plan fits
	// on screen in the dry run, so the user is the better check.
	compactMinArchiveAllowance = 10

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
	var plans []CompactPlan
	for _, ln := range c.Lenses {
		plan, err := c.planLens(ctx, ln, all)
		if err != nil {
			// One lens's failure must not strand the others: compaction is maintenance, and a
			// partial plan is still useful. Surface it and continue.
			slog.Error("compact: lens planning failed", "lens", ln.Name, "err", err)
			continue
		}
		// Keep a plan that found nothing but FAILED somewhere: dropping it would report
		// "nothing to compact" for a lens whose calls never ran, which is the one wrong
		// conclusion a maintenance command must not hand back.
		if len(plan.Actions) > 0 || plan.Failed > 0 {
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
		for _, batch := range compactBatches(inDim) {
			actions, err := c.askBatch(ctx, ln, dim, batch)
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
				if reason := validate(a, dim, batch, byKey, claimed); reason != "" {
					plan.Skipped[reason]++
					continue
				}
				// Claim every facet the action consumes so a later batch or a duplicate
				// entry cannot act on it again.
				switch a.Action {
				case "merge":
					for _, k := range a.Absorb {
						claimed[store.FacetRef(dim, k)] = true
						plan.Archived++
					}
					claimed[store.FacetRef(dim, a.Key)] = true
				case "retire":
					claimed[store.FacetRef(dim, a.Key)] = true
					plan.Archived++
				}
				a.Dimension = dim // normalize: validate accepted it, store it canonically
				plan.Actions = append(plan.Actions, a)
			}
		}
	}

	// Runaway guard. Applied to the whole lens AFTER planning so the limit is on total blast
	// radius, not on any one batch (a single batch is a small slice of the profile anyway).
	limit := int(float64(plan.Before) * compactMaxArchiveFraction)
	if limit < compactMinArchiveAllowance {
		limit = compactMinArchiveAllowance
	}
	if plan.Archived > limit {
		return plan, fmt.Errorf("plan would archive %d of %d active facets (cap %d); refusing — "+
			"re-run to compact incrementally, or review the compaction output for this lens",
			plan.Archived, plan.Before, limit)
	}
	slog.Info("compact: planned", "lens", ln.Name, "active", plan.Before,
		"actions", len(plan.Actions), "would_archive", plan.Archived, "skipped", plan.Skipped)
	return plan, nil
}

// askBatch runs one model call over one dimension's batch.
func (c *Compactor) askBatch(ctx context.Context, ln *lens.Lens, dim string, batch []store.Facet) ([]compactAction, error) {
	input := renderCompactBatch(ln.Name, dim, batch)
	reply, err := c.runnerFor(ln)(ctx, ModelFor(c.Config, ln, PhaseReview), compactPrompt, input)
	if err != nil {
		return nil, err
	}
	actions, err := ParseJSONArray[compactAction](reply)
	if err != nil {
		return nil, err
	}
	slog.Info("compact: batch", "lens", ln.Name, "dimension", dim, "facets", len(batch),
		"input_chars", len(input), "reply_chars", len(reply), "proposed", len(actions))
	return actions, nil
}

// validate is the whole safety story. It returns "" when the action may be applied, else a
// short reason used as a skip-tally key. Every rule here exists because the alternative is a
// model reply silently reshaping a profile in a way the user cannot see or undo.
func validate(a compactAction, dim string, batch []store.Facet, byKey map[string]store.Facet, claimed map[string]bool) string {
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
		if strings.TrimSpace(a.Value) == "" {
			// An empty merged value would make the survivor's current stance blank — the
			// same class of damage wellFormed guards in the review path.
			return "empty_merged_value"
		}
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
				archived++

			case "merge":
				// Provenance first: the survivor's new value is justified by everything the
				// absorbed facets were justified by, so the merged version carries the union
				// of their observation ids. Without this a merge would orphan the evidence.
				var because []string
				absorbed := 0
				for _, k := range a.Absorb {
					src, ok := byKey[plan.Lens+"|"+a.Dimension+"|"+k]
					if !ok {
						continue
					}
					if cur := src.Current(); cur != nil {
						because = mergeIDs(because, cur.BecauseOf)
					}
					src.ArchivedAt = nowStr
					src.MergedInto = store.FacetRef(a.Dimension, a.Key)
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
							Value: a.Value, ValidFrom: nowStr, RecordedAt: nowStr,
							BecauseOf: because, Confidence: clampConf(a.Confidence),
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
					if cur.Value != a.Value {
						f.Versions[len(f.Versions)-1].ValidTo = nowStr
						f.Versions = append(f.Versions, store.FacetVersion{
							Value: a.Value, ValidFrom: nowStr, RecordedAt: nowStr,
							BecauseOf: because, Confidence: clampConf(a.Confidence),
						})
					} else {
						cur.BecauseOf = because
					}
				} else {
					f.Versions = append(f.Versions, store.FacetVersion{
						Value: a.Value, ValidFrom: nowStr, RecordedAt: nowStr,
						BecauseOf: because, Confidence: clampConf(a.Confidence),
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
func compactBatches(facets []store.Facet) [][]store.Facet {
	var out [][]store.Facet
	cur := []store.Facet{}
	size := 0
	for _, f := range facets {
		n := len(f.Current().Value)
		if len(cur) > 0 && size+n > compactBatchChars {
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

// renderCompactBatch is the model-facing view of one dimension's batch. Version count and
// confidence are included because they are the cheap signals for "is this load-bearing": a
// facet that has never changed and sits at low confidence after many sessions is a retirement
// candidate, while one with a real change history is carrying an arc worth keeping.
func renderCompactBatch(lensName, dim string, batch []store.Facet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LENS: %s\nDIMENSION: %s\nFACETS: %d\n\n", lensName, dim, len(batch))
	for _, f := range batch {
		cur := f.Current()
		fmt.Fprintf(&b, "key: %s\nconfidence: %.2f\nversions: %d\nlast_seen: %s\nvalue: %s\n\n",
			f.Key, cur.Confidence, len(f.Versions), f.LastSeen, cur.Value)
	}
	return b.String()
}
