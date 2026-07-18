package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// profileSigKey is the meta key holding the content signature of the last summary
// written for a lens (issue #73-S5). Namespaced per lens so the dirty check is
// O(1) and independent per lens.
func profileSigKey(lens string) string { return "profile_sig:" + lens }

// summarySignature is the fingerprint of everything a per-lens summary depends on:
// the model that produces it, the summarizer PROMPT, and the exact facet text fed
// to it. If it matches the stored signature AND the prior profile file still exists,
// the summary cannot have changed, so the (expensive) LLM call is skipped and the
// prior summary is reused. Including the model AND the prompt means switching a
// lens's review model — or a witness upgrade shipping a new summarize prompt —
// correctly invalidates the signature and forces a one-time regen on the next
// review, with no manual rebuild needed.
func summarySignature(model, prompt, renderedFacets string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + prompt + "\x00" + renderedFacets))
	return hex.EncodeToString(sum[:])
}

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
	Lenses        []*lens.Lens  // active lenses, so a per-lens summary uses that lens's ReviewModel (#75); a facet whose lens isn't here (orphan) falls back to the default DistillModel
	LensPrompt    string        // prompts/summarize/lens.md
	UnifiedPrompt string        // prompts/summarize/unified.md
	Run           SummarizeFunc // required; production wires RunnerMine(NewRunner(cfg)), tests inject a fake
	// RunFor, when set, picks the SummarizeFunc for a specific lens — the per-lens RUNNER
	// seam (issue #75 slice 2). A per-lens summary then runs on that lens's own runtime,
	// like its review. nil → every summary uses Run. The unified cross-lens portrait has no
	// single lens, so it always uses Run (the default runner).
	RunFor func(ln *lens.Lens) SummarizeFunc
}

// runFor returns the SummarizeFunc for a lens: the per-lens runner via RunFor when wired,
// else the default Run.
func (sm *Summarizer) runFor(ln *lens.Lens) SummarizeFunc {
	if sm.RunFor != nil {
		if fn := sm.RunFor(ln); fn != nil {
			return fn
		}
	}
	return sm.Run
}

// Summarize regenerates each per-lens summary from current facets, then the unified
// portrait from those summaries. Each file is overwritten only after its summary
// succeeds, so a mid-pass failure returns an error while leaving already-written
// (and not-yet-touched) summaries intact.
//
// Dirty-tracking (issue #73-S5): a per-lens summary is an LLM call, and a review
// burst at N lenses used to fire N+1 calls even when only one lens's facets changed.
// Each lens's summary now carries a content signature (its model + summarizer prompt
// + facet text); if the signature is unchanged AND the prior profile/<lens>.md still
// exists, its LLM call is SKIPPED and the prior summary is reused for the portrait.
// (A witness upgrade that ships a new summarize prompt changes the signature, so
// every lens regenerates once — no manual rebuild.) The unified
// portrait — which depends on every lens's summary — is likewise skipped when NO
// lens changed and profile/unified.md exists. So an unchanged review costs 0 calls,
// and a one-lens change costs 2 (that lens + unified), not N+1.
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
	// from a since-deregistered lens) maps to nil → default runner + default model.
	byName := map[string]*lens.Lens{}
	for _, l := range sm.Lenses {
		byName[l.Name] = l
	}

	var portrait strings.Builder
	anyChanged := false
	for _, l := range lenses {
		ln := byName[l]
		model := ModelFor(sm.Config, ln, PhaseReview)
		rendered := renderFacetsForSummary(l, byLens[l])
		sig := summarySignature(model, sm.LensPrompt, rendered)

		// Skip the LLM call when this lens's inputs are unchanged AND its prior summary
		// is still on disk — reuse that summary for the portrait. A missing/failed prior
		// file (prev == "" || !ok) falls through to regenerate, so a deleted profile
		// self-heals. Signature-read/-write is via meta (a config read error just means
		// we don't skip — safe, only costs a call).
		if sm.Store.MetaString(profileSigKey(l)) == sig {
			if prev, ok, _ := sm.Store.ReadProfile(l); ok && prev != "" {
				fmt.Fprintf(&portrait, "## %s\n\n%s\n\n", l, prev)
				continue
			}
		}

		md, err := sm.runFor(ln)(ctx, model, sm.LensPrompt, rendered)
		if err != nil {
			return fmt.Errorf("summarize lens %s: %w", l, err)
		}
		if err := sm.Store.WriteProfile(l, md); err != nil {
			return fmt.Errorf("write profile %s: %w", l, err)
		}
		// Stamp the signature only AFTER the summary is safely on disk, so a crash
		// between write and stamp just regenerates next time (never skips a stale file).
		_ = sm.Store.SetMetaString(profileSigKey(l), sig)
		anyChanged = true
		fmt.Fprintf(&portrait, "## %s\n\n%s\n\n", l, md)
	}

	// The unified portrait is a function of every per-lens summary. If NO lens changed
	// and a prior unified profile exists, it is identical — skip its LLM call too. Any
	// single lens changing (or a missing unified file) regenerates it.
	if !anyChanged {
		if _, ok, _ := sm.Store.ReadProfile(store.ProfileUnified); ok {
			return nil
		}
	}
	// The unified portrait spans all lenses → no single lens → the default DistillModel
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
