package distill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Reviewer is the slow path: synthesize L1 observations into L2 facets, detecting
// durable change across sessions. It is the SOLE writer of L2.
//
// Division of labor: the LLM judges *what is true now* and *what changed* (it sees
// the observations + the current profile and proposes facet states). Deterministic
// Go code applies the bi-temporal mechanics — setting valid_to, appending versions,
// adjusting confidence — so the invalidation RULE is enforced by code, not left to
// the model's goodwill:
//
//   - supersede (valid_to set) ONLY when the model reports a sustained contradicting
//     value for a facet (a real change arc)
//   - NEVER invalidate on mere absence — that only decays confidence
//   - new facets are added open-ended (valid_to == "")
type Reviewer struct {
	// Store is the narrow L1→L2 review surface (issue #73-C1): read facets + slimmed
	// observations, replace the facet profile, stamp the review cadence — not the whole
	// *store.Store.
	Store  store.ReviewStore
	Lenses []*lens.Lens
	Config store.Config
	Runner MineFunc // required; production wires RunnerMine(NewRunner(cfg)), tests inject a fake
	// RunnerFor, when set, picks the MineFunc for a specific lens — the per-lens RUNNER
	// seam (issue #75 slice 2), mirroring Worker.RunFor. nil → every lens reviews on Runner.
	RunnerFor func(ln *lens.Lens) MineFunc
}

// runnerFor returns the MineFunc for a lens's review: the per-lens runner via RunnerFor
// when wired, else the single default Runner.
func (r *Reviewer) runnerFor(ln *lens.Lens) MineFunc {
	if r.RunnerFor != nil {
		if fn := r.RunnerFor(ln); fn != nil {
			return fn
		}
	}
	return r.Runner
}

// reviewedFacet is what the review prompt returns per facet it asserts.
type reviewedFacet struct {
	Dimension   string   `json:"dimension"`
	Key         string   `json:"key"`
	Value       string   `json:"value"`
	Confidence  float64  `json:"confidence"`
	BecauseOf   []string `json:"because_of"`        // observation IDs supporting THIS value
	Contradicts bool     `json:"contradicts_prior"` // model's judgment: is this a sustained change vs the stored current value?
}

// Run reviews all active lenses and rewrites L2.
func (r *Reviewer) Run(ctx context.Context, now time.Time) error {
	facets, err := r.Store.ReadFacets()
	if err != nil {
		return fmt.Errorf("read L2: %w", err)
	}
	byKey := indexFacets(facets)
	nowStr := now.UTC().Format(time.RFC3339)

	// Track lenses whose review call failed. A read error or an empty obs set is
	// benign (skip), but a FAILED review call (timeout, model error, unparseable
	// reply) must not be swallowed: silently continuing here — then stamping the
	// review and returning nil below — advanced the watermark past a lens that was
	// never actually reviewed and reported "review complete" with zero facets
	// synthesized (issue #16 C1). We still apply the lenses that DID succeed (no
	// data loss; their facets are real), but we refuse to stamp and surface the
	// failure so the review stays due and is retried on the next pass.
	var failed []string
	for _, ln := range r.Lenses {
		obs, err := r.Store.ReadObservationsLite(ln.Name) // reviewer slims embeddings off anyway
		if err != nil {
			failed = append(failed, ln.Name)
			continue
		}
		if len(obs) == 0 {
			continue
		}
		reviewed, err := r.reviewLens(ctx, ln, obs, facets)
		if err != nil {
			failed = append(failed, ln.Name)
			continue
		}
		for _, rf := range reviewed {
			r.applyFacet(byKey, ln.Name, rf, nowStr)
		}
	}

	merged := collectFacets(byKey)
	if err := r.Store.WriteFacets(merged); err != nil {
		return fmt.Errorf("write L2: %w", err)
	}
	// Only advance the review watermark if EVERY active lens was reviewed. A partial
	// stamp would mark the failed lens as reviewed-through-now and let it drift
	// unreviewed until the next unrelated trigger.
	//
	// Tradeoff (bounded, accepted): not stamping keeps ReviewDue() true, so under a
	// PERSISTENTLY-failing lens with concurrent live capture the worker re-reviews
	// the healthy lenses each drain iteration (O(new-sessions) redundant full reviews
	// vs ~1). This terminates when capture stops — it is NOT the #49-C1 unbounded
	// no-progress spin (loop continuation gates on unattempted mining work, never on
	// ReviewDue). Correctly never reporting silent success outweighs the redundant
	// cost; the clean fix is per-lens review state (#55), which lets a healthy lens
	// stamp independently of a failing one.
	if len(failed) > 0 {
		return fmt.Errorf("review failed for %d lens(es): %s (partial facets written; review left pending)",
			len(failed), strings.Join(failed, ", "))
	}
	return r.Store.StampReview()
}

// applyFacet enforces the bi-temporal rule deterministically.
func (r *Reviewer) applyFacet(byKey map[string]*store.Facet, lensName string, rf reviewedFacet, nowStr string) {
	id := lensName + "|" + rf.Dimension + "|" + rf.Key
	f, ok := byKey[id]
	if !ok {
		// Brand-new facet: open-ended first version.
		byKey[id] = &store.Facet{
			Lens: lensName, Dimension: rf.Dimension, Key: rf.Key, LastSeen: nowStr,
			Versions: []store.FacetVersion{{
				Value: rf.Value, ValidFrom: nowStr, RecordedAt: nowStr,
				BecauseOf: rf.BecauseOf, Confidence: clampConf(rf.Confidence),
			}},
		}
		return
	}

	cur := f.Current()
	f.LastSeen = nowStr

	switch {
	case cur == nil:
		// Facet existed but had no open version (all expired) — reopen.
		f.Versions = append(f.Versions, store.FacetVersion{
			Value: rf.Value, ValidFrom: nowStr, RecordedAt: nowStr,
			BecauseOf: rf.BecauseOf, Confidence: clampConf(rf.Confidence),
		})
	case rf.Contradicts && !sameValue(cur.Value, rf.Value):
		// Sustained contradiction => record a change arc: close the old, open the new.
		// (The "sustained" judgment is the review prompt's job; code just applies it.)
		f.Versions[len(f.Versions)-1].ValidTo = nowStr
		f.Versions = append(f.Versions, store.FacetVersion{
			Value: rf.Value, ValidFrom: nowStr, RecordedAt: nowStr,
			BecauseOf: rf.BecauseOf, Confidence: clampConf(rf.Confidence),
		})
	default:
		// Same value reaffirmed: reinforce (raise confidence, refresh provenance).
		cur.Confidence = clampConf(maxF(cur.Confidence, rf.Confidence))
		cur.BecauseOf = mergeIDs(cur.BecauseOf, rf.BecauseOf)
	}
}

func (r *Reviewer) reviewLens(ctx context.Context, ln *lens.Lens, obs []store.Observation, prior []store.Facet) ([]reviewedFacet, error) {
	input := "OBSERVATIONS (L1):\n" + mustJSON(slimObs(obs)) +
		"\n\nCURRENT PROFILE (L2, this lens):\n" + mustJSON(slimFacets(prior, ln.Name))
	reply, err := r.runnerFor(ln)(ctx, ModelFor(r.Config, ln, PhaseReview), ln.Review, input)
	if err != nil {
		return nil, err
	}
	return ParseJSONArray[reviewedFacet](reply)
}

// PreviewFacet is one facet the REVIEW prompt asserted in a read-only preview — the
// L2 counterpart to a mined Observation in a preview. Never persisted.
type PreviewFacet struct {
	Dimension  string
	Key        string
	Value      string
	Confidence float64
	BecauseOf  []string // observation IDs this facet cites
	// Contradicts is the model's claim that this is a SUSTAINED change vs the stored
	// current value. In a candidate-lens preview `prior` is usually empty (an
	// unregistered lens has no accumulated facets), so change-detection has nothing to
	// contradict — a caveat inherent to previewing before backfill, not a bug.
	Contradicts bool
}

// PreviewReview runs a lens's REVIEW (L1→L2) prompt over a set of observations WITHOUT
// writing any facets — the read-only synthesis half of `witness lens try`. It is a
// twin of reviewLens (same input shaping, same DistillModel, same parse), built on a
// Store-nil Reviewer so it is STRUCTURALLY unable to touch the archive: it never calls
// ReadFacets/WriteFacets/StampReview/applyFacet. `prior` is the current facet set to
// diff against (nil for an unregistered candidate); `obs` are the observations to
// synthesize (in the tuning loop, the ones the EXTRACT preview just produced in-memory).
func PreviewReview(ctx context.Context, run MineFunc, cfg store.Config, ln *lens.Lens, obs []store.Observation, prior []store.Facet) ([]PreviewFacet, error) {
	rv := &Reviewer{Config: cfg, Runner: run} // Store nil: reviewLens reads only Config+Runner
	reviewed, err := rv.reviewLens(ctx, ln, obs, prior)
	if err != nil {
		return nil, err
	}
	out := make([]PreviewFacet, 0, len(reviewed))
	for _, rf := range reviewed {
		out = append(out, PreviewFacet{
			Dimension: rf.Dimension, Key: rf.Key, Value: rf.Value,
			Confidence: rf.Confidence, BecauseOf: rf.BecauseOf, Contradicts: rf.Contradicts,
		})
	}
	return out, nil
}

// --- helpers ----------------------------------------------------------------

func indexFacets(facets []store.Facet) map[string]*store.Facet {
	m := make(map[string]*store.Facet, len(facets))
	for i := range facets {
		f := facets[i]
		m[f.Lens+"|"+f.Dimension+"|"+f.Key] = &f
	}
	return m
}

func collectFacets(m map[string]*store.Facet) []store.Facet {
	out := make([]store.Facet, 0, len(m))
	for _, f := range m {
		out = append(out, *f)
	}
	return out
}

// slimObs strips embeddings before sending observations to the prompt (save tokens).
func slimObs(obs []store.Observation) []map[string]any {
	out := make([]map[string]any, 0, len(obs))
	for _, o := range obs {
		out = append(out, map[string]any{
			"id": o.ID, "ts": o.TS, "dimension": o.Dimension,
			"observation": o.Observation, "evidence": o.Evidence, "poignancy": o.Poignancy,
		})
	}
	return out
}

func slimFacets(facets []store.Facet, lensName string) []map[string]any {
	out := []map[string]any{}
	for _, f := range facets {
		if f.Lens != lensName {
			continue
		}
		cur := f.Current()
		val := ""
		if cur != nil {
			val = cur.Value
		}
		out = append(out, map[string]any{
			"dimension": f.Dimension, "key": f.Key, "current_value": val,
		})
	}
	return out
}

func clampConf(c float64) float64 {
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func sameValue(a, b string) bool { return a == b }
func mergeIDs(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range append(append([]string{}, a...), b...) {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
