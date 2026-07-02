package distill

import (
	"context"
	"fmt"
	"time"

	"github.com/IngTian/claude-witness/internal/lens"
	"github.com/IngTian/claude-witness/internal/store"
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
	Store  *store.Store
	Lenses []*lens.Lens
	Config store.Config
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

	for _, ln := range r.Lenses {
		obs, err := r.Store.ReadObservationsLite(ln.Name) // reviewer slims embeddings off anyway
		if err != nil || len(obs) == 0 {
			continue
		}
		reviewed, err := r.reviewLens(ctx, ln, obs, facets)
		if err != nil {
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
	reply, err := Run(ctx, r.Config.DistillModel, ln.Review, input)
	if err != nil {
		return nil, err
	}
	return ParseJSONArray[reviewedFacet](reply)
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
