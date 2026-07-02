package distill

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/IngTian/claude-witness/internal/store"
)

// SummarizeFunc runs one summarization pass (a `claude -p` call). Injectable so
// tests drive the summarizer without a model. Same shape as Run; nil => Run.
type SummarizeFunc func(ctx context.Context, model, prompt, input string) (string, error)

// Summarizer distills the L2 facets into the L4 narrative profile: one markdown
// summary per lens (profile/<lens>.md) plus a cross-lens portrait
// (profile/unified.md). It runs right after a review updates facets — the profile
// is purely derived, so this never blocks the worker (callers treat it as
// best-effort) and a failed pass leaves the prior summaries in place.
type Summarizer struct {
	Store         *store.Store
	Config        store.Config
	LensPrompt    string        // prompts/summarize/lens.md
	UnifiedPrompt string        // prompts/summarize/unified.md
	Run           SummarizeFunc // nil => the package Run (real claude -p)
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

	runFn := sm.Run
	if runFn == nil {
		runFn = Run
	}

	var portrait strings.Builder
	for _, l := range lenses {
		md, err := runFn(ctx, sm.Config.DistillModel, sm.LensPrompt, renderFacetsForSummary(l, byLens[l]))
		if err != nil {
			return fmt.Errorf("summarize lens %s: %w", l, err)
		}
		if err := sm.Store.WriteProfile(l, md); err != nil {
			return fmt.Errorf("write profile %s: %w", l, err)
		}
		fmt.Fprintf(&portrait, "## %s\n\n%s\n\n", l, md)
	}

	umd, err := runFn(ctx, sm.Config.DistillModel, sm.UnifiedPrompt, portrait.String())
	if err != nil {
		return fmt.Errorf("summarize unified: %w", err)
	}
	return sm.Store.WriteProfile("unified", umd)
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
