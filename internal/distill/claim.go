package distill

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Claims: the short statement of what a facet asserts, written once per value.
//
// WHY THIS PASS EXISTS. The review prompt is what produces a facet's rich prose — contrasts
// with sibling facets, diagnoses, advice, provenance limits, falsifiers — and that is worth
// keeping, for the portrait and for a human reading their own archive. What is NOT worth
// re-sending on every fold is all of it: measured on a real archive, 2,039 facets carried
// 2,411,507 chars, which was 99.4% of the review call against a 16,000-char observation budget,
// and the lens had not folded for 24 days.
//
// So the review call reads CLAIMS and the archive keeps VALUES. Something has to turn one into
// the other, and it cannot be the review prompt itself: those live in the lens registry, are
// copied into an archive once at seed time, and provably never reach an existing install. It
// also should not be the review call, which already has a hard job. A separate, tiny pass does
// it: one batched call that reads values and writes one or two sentences each.
//
// It is CHEAP because the output is small — the thing that makes compaction expensive. A batch
// of ~25 facets costs one call with ~30k chars in and ~4k out, so the steady state (the handful
// of facets whose value changed in a fold) is a fraction of one call, and a first full pass over
// a mature archive is tens of calls rather than thousands.
//
// Bounded per run, deliberately. A fresh archive has no claims at all, and draining 2,000 of
// them inside one review would turn a routine fold into an hours-long job. Progressive is the
// right shape: every fold fills in a few batches, the profile converges, and nothing is ever
// blocked waiting for it. Until a facet has a claim, every consumer falls back to its value
// (store.FacetVersion.Gist), so a half-claimed archive is correct, just not yet small.
// Claimer writes the short claims. Its own type rather than a method on Reviewer, because
// claims are MAINTENANCE — run when the user asks, like compaction — and the review path
// deliberately no longer triggers them: adding one or two silent model calls to every fold is a
// cost the user did not ask for, and the same reasoning that keeps compaction on-demand applies
// here. The review call READS claims (store.FacetVersion.Gist); it does not produce them.
type Claimer struct {
	Store     store.CompactStore
	Lenses    []*lens.Lens
	Config    store.Config
	Runner    MineFunc
	RunnerFor func(ln *lens.Lens) MineFunc
	// MaxBatches bounds one run. 0 means UNBOUNDED, which is right for an explicit command:
	// the user asked, and a first pass over a mature archive is tens of calls. A caller that
	// wants to drain progressively sets a small number.
	MaxBatches int
}

func (c *Claimer) runnerFor(ln *lens.Lens) MineFunc {
	return resolveRunner(c.RunnerFor, ln, c.Runner)
}

const (
	// claimBatchChars bounds one claim call's input. Small compared with compaction's batches
	// because the work is per-facet rather than cross-facet: nothing is gained by showing the
	// model more of the profile at once, and a tighter batch keeps each call fast.
	claimBatchChars = 30_000

	// claimMaxChars is the ceiling for an accepted claim. A "claim" that runs to a thousand
	// characters is the model ignoring the instruction and handing back the analysis, which
	// would defeat the entire point — the review payload would be unchanged while looking
	// like it had been fixed. Rejected rather than truncated: a half-sentence claim is worse
	// than none, because none falls back to the full value and is at least complete.
	claimMaxChars = 600
)

// claimedFacet is what the claim prompt returns per facet.
type claimedFacet struct {
	Dimension string `json:"dimension"`
	Key       string `json:"key"`
	Claim     string `json:"claim"`
}

// WriteClaims fills in missing claims for one lens, bounded to claimBatchesPerRun calls.
// Returns how many claims were written. It mutates the facets in place; the caller owns the
// WriteFacets that persists them.
//
// Only facets with a value and NO claim are candidates, so this is idempotent and converges:
// each run drains a little more, and a facet whose value later changes becomes a candidate
// again only if its claim is cleared (see applyFacet).
func (c *Claimer) WriteClaims(ctx context.Context, ln *lens.Lens, facets []*store.Facet) int {
	var pending []*store.Facet
	for _, f := range facets {
		if f.Lens != ln.Name || !f.Active() {
			continue
		}
		cur := f.Current()
		if cur == nil || strings.TrimSpace(cur.Value) == "" {
			continue
		}
		if strings.TrimSpace(cur.Claim) == "" {
			pending = append(pending, f)
		}
	}
	if len(pending) == 0 {
		return 0
	}

	written := 0
	for batches := 0; len(pending) > 0; batches++ {
		if c.MaxBatches > 0 && batches >= c.MaxBatches {
			break
		}
		batch, rest := nextClaimBatch(pending)
		pending = rest
		got, err := c.claimBatch(ctx, ln, batch)
		if err != nil {
			// Never fail a fold over this. The facets keep falling back to their values, which
			// is exactly the pre-claim behaviour, and the next run tries again.
			slog.Error("claims: batch failed (facets keep falling back to their full values)",
				"lens", ln.Name, "facets", len(batch), "err", err)
			break
		}
		byRef := map[string]*store.Facet{}
		for _, f := range batch {
			byRef[store.FacetRef(f.Dimension, f.Key)] = f
		}
		for _, c := range got {
			f, ok := byRef[store.FacetRef(c.Dimension, c.Key)]
			if !ok {
				continue // a facet it was not shown; ignore rather than trust
			}
			claim := strings.TrimSpace(c.Claim)
			if claim == "" || len(claim) > claimMaxChars {
				continue
			}
			cur := f.Current()
			if cur == nil || strings.TrimSpace(cur.Claim) != "" {
				continue
			}
			cur.Claim = claim
			written++
		}
	}
	if written > 0 {
		slog.Info("claims: written", "lens", ln.Name, "claims", written, "still_missing", len(pending))
	}
	return written
}

// claimPass is the store-touching wrapper: read the profile, fill in missing claims, write it
// back if anything changed. Runs once per lens per review, AFTER all of that lens's windows, so
// a multi-window fold spends one claim budget rather than one per window.
//
// Best-effort by design. A failure here leaves every facet falling back to its full value —
// exactly the behaviour before claims existed — so it must never fail a fold that has already
// committed real facets.
func (c *Claimer) Run(ctx context.Context, ln *lens.Lens) int {
	all, err := c.Store.ReadFacets()
	if err != nil {
		slog.Error("claims: could not read the profile", "lens", ln.Name, "err", err)
		return 0
	}
	byKey := indexFacets(all)
	ptrs := make([]*store.Facet, 0, len(byKey))
	for _, f := range byKey {
		ptrs = append(ptrs, f)
	}
	n := c.WriteClaims(ctx, ln, ptrs)
	if n > 0 {
		if err := c.Store.WriteFacets(collectFacets(byKey)); err != nil {
			slog.Error("claims: could not persist", "lens", ln.Name, "claims", n, "err", err)
			return 0
		}
	}
	return n
}

func nextClaimBatch(pending []*store.Facet) (batch, rest []*store.Facet) {
	size := 0
	for i, f := range pending {
		n := len(f.Current().Value)
		if i > 0 && size+n > claimBatchChars {
			return pending[:i], pending[i:]
		}
		size += n
	}
	return pending, nil
}

func (c *Claimer) claimBatch(ctx context.Context, ln *lens.Lens, batch []*store.Facet) ([]claimedFacet, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "LENS: %s\nFACETS: %d\n\n", ln.Name, len(batch))
	for _, f := range batch {
		fmt.Fprintf(&b, "--- dimension: %s\nkey: %s\nvalue: %s\n\n", f.Dimension, f.Key, f.Current().Value)
	}
	reply, err := c.runnerFor(ln)(ctx, ModelFor(c.Config, ln, PhaseReview), claimPrompt, b.String())
	if err != nil {
		return nil, err
	}
	out, err := ParseJSONArray[claimedFacet](reply)
	if err != nil {
		return nil, err
	}
	slog.Info("claims: batch", "lens", ln.Name, "facets", len(batch),
		"input_chars", b.Len(), "reply_chars", len(reply), "returned", len(out))
	return out, nil
}

// claimPrompt lives in Go, not the lens registry, for the reason this pass exists at all: lens
// prompt files are copied into an archive once at seed time and never updated, so text authored
// there provably never reaches an existing install. Writing "what does this facet assert" is
// also not domain-specific — it does not change between a coding lens and a trading lens.
const claimPrompt = `You are given facets from a behavioural profile. Each one is a long record about one person:
a claim about them, plus the evidence, contrasts with related facets, costs, provenance notes
and the conditions that would change it.

For each facet, write its CLAIM: one or two sentences stating what the facet actually asserts
about this person.

The claim is used to decide, in a later pass, whether new evidence bears on this facet and
whether it contradicts it. So it must stand alone and be judgeable. Name the disposition and its
mechanism — what the person does, and what makes it happen.

LEAVE OUT, because they live in the full record and are not needed to judge new evidence:

  - specific instances, dates, quotes and observation ids
  - confidence numbers and changelog notes ("UPGRADE", "NEW this round", "0.7 to 0.75")
  - references to other facets
  - falsifiers, costs, and "what to test next"
  - advice about how to work with the person

Write the claim in plain, specific, behavioural language, in the third person. Do not hedge it
into vagueness: "defaults to blaming his own state when confused, rather than suspecting the
material" is a claim; "has some patterns around confusion" is not.

Keep it UNDER 400 characters. A claim that runs long is the analysis again, which defeats the
purpose.

Return a JSON array, one entry per facet you were given, in the same order:

[{"dimension": "<dimension>", "key": "<key>", "claim": "<one or two sentences>"}]

Use the dimension and key exactly as given. Output ONLY the JSON array. No prose, no code fence.`
