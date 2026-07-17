package distill

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// SummarizeFunc runs one summarization pass. Same shape as MineFunc so a shared
// runner (for example one OpenCode serve process) can cover mining, review, and
// profile regeneration.
type SummarizeFunc = MineFunc

// Summarizer distills the L2 facets into the L4 narrative profile: one markdown
// summary per lens (profile/<lens>.md) plus a cross-lens portrait
// (profile/unified.md). It runs right after a review updates facets — the profile
// is purely derived, so this never blocks the worker (callers treat it as
// best-effort) and a failed pass leaves the prior summaries in place.
type Summarizer struct {
	Store         *store.Store
	Config        store.Config
	Lenses        []*lens.Lens  // active lenses, so a per-lens summary uses that lens's ReviewModel (#75); a facet whose lens isn't here (orphan) falls back to the global DistillModel
	LensPrompt    string        // prompts/summarize/lens.md
	UnifiedPrompt string        // prompts/summarize/unified.md
	Run           SummarizeFunc // required; production wires RunnerMine(NewRunner(cfg)), tests inject a fake
	// RunFor, when set, picks the SummarizeFunc for a specific lens — the per-lens RUNNER
	// seam (issue #75 slice 2). A per-lens summary then runs on that lens's own runtime,
	// like its review. nil → every summary uses Run. The unified cross-lens portrait has no
	// single lens, so it always uses Run (the global runner).
	RunFor func(ln *lens.Lens) SummarizeFunc
}

// runFor returns the SummarizeFunc for a lens: the per-lens runner via RunFor when wired,
// else the global Run.
func (sm *Summarizer) runFor(ln *lens.Lens) SummarizeFunc {
	if sm.RunFor != nil {
		if fn := sm.RunFor(ln); fn != nil {
			return fn
		}
	}
	return sm.Run
}

// Summarize regenerates every per-lens summary from current facets, then the
// unified portrait from those summaries. Each file is overwritten only after its
// summary succeeds, so a mid-pass failure returns an error while leaving already-
// written (and not-yet-touched) summaries intact.
func (sm *Summarizer) Summarize(ctx context.Context) error {
	facets, err := sm.Store.ReadFacets()
	if err != nil {
		return fmt.Errorf("read facets: %w", err)
	}
	byLens := map[string][]store.Facet{}
	for _, f := range facets {
		if f.Current() == nil {
			continue // no active version -> nothing to say
		}
		byLens[f.Lens] = append(byLens[f.Lens], f)
	}
	lenses := make([]string, 0, len(byLens))
	for l := range byLens {
		lenses = append(lenses, l)
	}
	sort.Strings(lenses) // deterministic order
	if len(lenses) == 0 {
		return nil // no facets yet — nothing to summarize
	}

	// Index the active lenses by name so each per-lens summary uses that lens's own
	// runner + review model (#75). A facet whose lens isn't in the active set (an orphan
	// from a since-deregistered lens) maps to nil → global runner + global model.
	byName := map[string]*lens.Lens{}
	for _, l := range sm.Lenses {
		byName[l.Name] = l
	}

	var portrait strings.Builder
	for _, l := range lenses {
		ln := byName[l]
		model := ModelFor(sm.Config, ln, PhaseReview)
		md, err := sm.runFor(ln)(ctx, model, sm.LensPrompt, renderFacetsForSummary(l, byLens[l]))
		if err != nil {
			return fmt.Errorf("summarize lens %s: %w", l, err)
		}
		if err := sm.Store.WriteProfile(l, md); err != nil {
			return fmt.Errorf("write profile %s: %w", l, err)
		}
		fmt.Fprintf(&portrait, "## %s\n\n%s\n\n", l, md)
	}

	// The unified portrait spans all lenses → no single lens → the global DistillModel
	// (ModelFor with a nil lens).
	umd, err := sm.Run(ctx, ModelFor(sm.Config, nil, PhaseReview), sm.UnifiedPrompt, portrait.String())
	if err != nil {
		return fmt.Errorf("summarize unified: %w", err)
	}
	return sm.Store.WriteProfile(store.ProfileUnified, umd)
}

// renderFacetsForSummary formats one lens's active facets as readable input for
// the summarizer prompt.
func renderFacetsForSummary(lens string, facets []store.Facet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LENS: %s\n\nFACETS (dimension/key, confidence, value):\n", lens)
	for _, f := range facets {
		cur := f.Current()
		if cur == nil {
			continue
		}
		fmt.Fprintf(&b, "- %s/%s (%.2f): %s\n", f.Dimension, f.Key, cur.Confidence, cur.Value)
	}
	return b.String()
}
