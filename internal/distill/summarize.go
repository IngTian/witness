package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	// Store is the narrow L2→L4 summary surface (issue #73-C1): read facets, read/write
	// the narrative profile files, and its own meta watermark — not the whole
	// *store.Store.
	Store         store.SummaryStore
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
// (A witness upgrade that ships a new lens prompt changes the signature, so every
// lens regenerates once — no manual rebuild.) The unified portrait carries its OWN
// signature over its model + unified prompt + the whole portrait text (which embeds
// every per-lens summary), so it is skipped only when all of those are unchanged and
// profile/unified.md exists — this catches a unified-only prompt change, a
// DistillModel change, and an added/removed lens, none of which a "did any per-lens
// summary change?" heuristic would. So an unchanged review costs 0 calls, and a
// one-lens change costs 2 (that lens + unified), not N+1.
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

	// Reap summaries whose lens no longer has facets, BEFORE the early return below.
	//
	// This loop only ever visits lenses that HAVE facets, so a lens whose facets were
	// dropped — `lens backfill --fresh`, `lens deregister`, a cleanup that reclaimed its
	// source — was never visited and its profile/<lens>.md stayed on disk forever. That file
	// is what `witness profile <lens>` and the MCP get_profile tool read, so an agent was
	// served a narrative built from facets that no longer exist, with nothing marking it
	// stale. Verified before this fix: dropping a lens's facets and re-running Summarize left
	// the old summary readable and unchanged.
	//
	// Deleting (not blank-writing) is deliberate and matches what the unified portrait
	// already does below: readers then show the friendly "not generated yet" message instead
	// of an empty document. The signature is cleared too, so if the lens is re-mined later
	// nothing vouches for the deleted file and it regenerates.
	sm.reapOrphanProfiles(byLens)

	if len(lenses) == 0 {
		// No facets anywhere. The unified cleanup below is unreachable from here, so do it
		// now — otherwise dropping the LAST lens's facets stranded a cross-lens portrait
		// describing lenses that no longer have any.
		sm.deleteProfileAndSignature(store.ProfileUnified)
		return nil
	}

	// Index the active lenses by name so each per-lens summary uses that lens's own
	// runner + review model (#75). A facet whose lens isn't in the active set (an orphan
	// from a since-deregistered lens) maps to nil → default runner + default model.
	byName := map[string]*lens.Lens{}
	for _, l := range sm.Lenses {
		byName[l.Name] = l
	}

	var portrait strings.Builder
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
		fmt.Fprintf(&portrait, "## %s\n\n%s\n\n", l, md)
	}

	// The unified portrait is a CROSS-lens synthesis — it only means something with ≥2
	// lenses. With a single lens (common since #44 slice 1a: an install may run just one
	// domain lens, e.g. a market-news archive) the "unified" portrait would merely restate
	// that lone lens's summary, burning an extra LLM call for a duplicate. So skip it when
	// <2 lenses have facets, and clear any stale unified profile left from when there were
	// more (so a lens removal doesn't strand a portrait describing lenses that are gone).
	if len(lenses) < 2 {
		// DELETE (not blank-write) any stale portrait so readers (get_profile /
		// `witness profile`) show the clean "not generated yet" message rather than an
		// empty document. Clear its signature too so a later return to ≥2 lenses rebuilds.
		sm.deleteProfileAndSignature(store.ProfileUnified)
		return nil
	}
	// The unified portrait spans all lenses → no single lens → the default DistillModel
	// (ModelFor with a nil lens).
	unifiedModel := ModelFor(sm.Config, nil, PhaseReview)
	// Sign the unified pass over its OWN inputs: the model, the unified prompt, and the
	// whole portrait (which already embeds every per-lens summary, so any per-lens
	// change, a dropped/added lens, or a prompt/model change all flow through here). This
	// replaces an earlier "skip if no per-lens changed" heuristic that missed a unified-
	// only prompt change, a DistillModel change under all-lens-overrides, and a removed
	// lens (which would leave the portrait describing a lens that no longer exists).
	unifiedSig := summarySignature(unifiedModel, sm.UnifiedPrompt, portrait.String())
	if sm.Store.MetaString(profileSigKey(store.ProfileUnified)) == unifiedSig {
		if _, ok, _ := sm.Store.ReadProfile(store.ProfileUnified); ok {
			return nil
		}
	}
	umd, err := sm.Run(ctx, unifiedModel, sm.UnifiedPrompt, portrait.String())
	if err != nil {
		return fmt.Errorf("summarize unified: %w", err)
	}
	if err := sm.Store.WriteProfile(store.ProfileUnified, umd); err != nil {
		return err
	}
	// Stamp after the write succeeds (same crash-safety as the per-lens stamp).
	_ = sm.Store.SetMetaString(profileSigKey(store.ProfileUnified), unifiedSig)
	return nil
}

// deleteProfileAndSignature removes a profile file that has stopped being applicable and
// clears the signature vouching for it.
//
// Both halves matter. Deleting the file makes readers (`witness profile`, the MCP
// get_profile tool) show "not generated yet" instead of a stale narrative. Clearing the
// signature means that if the lens later regains facets, nothing claims the deleted file is
// current — otherwise the skip-the-LLM-call fast path would match a signature for a file
// that no longer exists. Best-effort: a failure here must never fail a review whose real
// work already landed, and the next pass retries.
func (sm *Summarizer) deleteProfileAndSignature(lens string) {
	if _, ok, _ := sm.Store.ReadProfile(lens); !ok {
		return // nothing on disk; don't churn the signature
	}
	if err := sm.Store.DeleteProfile(lens); err != nil {
		slog.Warn("summarize: could not remove a stale profile", "lens", lens, "err", err)
		return // leave the signature alone: the file is still there
	}
	_ = sm.Store.SetMetaString(profileSigKey(lens), "")
}

// reapOrphanProfiles deletes per-lens summaries whose lens has no facets in THIS pass.
//
// withFacets is the set the caller is about to summarize. Anything else on disk is an
// orphan: its facets were dropped (`lens backfill --fresh`, `lens deregister`) and the main
// loop, which iterates only lenses that HAVE facets, would never visit it again.
//
// The unified portrait is deliberately skipped here — it is not a per-lens summary and has
// its own <2-lens rule downstream, so reaping it on this basis would fight that logic.
func (sm *Summarizer) reapOrphanProfiles(withFacets map[string][]store.Facet) {
	onDisk, err := sm.Store.ListProfiles()
	if err != nil {
		// Read-only enumeration failed: skip the reap rather than fail the review. The
		// worst case is the pre-existing behavior (a stale file lingers one more pass).
		slog.Warn("summarize: could not enumerate profiles to reap stale ones", "err", err)
		return
	}
	for _, lens := range onDisk {
		if lens == store.ProfileUnified {
			continue
		}
		if _, ok := withFacets[lens]; ok {
			continue // still has facets — the main loop owns it
		}
		slog.Info("summarize: removing a profile whose lens no longer has facets", "lens", lens)
		sm.deleteProfileAndSignature(lens)
	}
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
