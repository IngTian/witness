package distill

import (
	"context"
	"fmt"
	"log/slog"
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
	nowStr := now.UTC().Format(time.RFC3339)

	// Each lens folds INDEPENDENTLY, in size-bounded WINDOWS (issue #123). A lens whose
	// window read/fold fails (timeout, model error, unparseable reply) must not be
	// swallowed: the earlier #16-C1 bug advanced the watermark past a never-reviewed lens
	// and reported "complete" with zero facets. We keep the windows that DID commit (real
	// facets, watermark advanced through them), but refuse the global stamp and surface
	// the failure so the review stays due and resumes from the watermark next pass.
	var failed []string
	for _, ln := range r.Lenses {
		if err := r.foldLensWindowed(ctx, ln, nowStr); err != nil {
			slog.Error("review: lens fold failed (committed windows kept; watermark left at last good window)",
				"lens", ln.Name, "err", err)
			failed = append(failed, ln.Name)
		}
	}

	// Only advance the GLOBAL review cadence stamp if EVERY active lens folded cleanly. A
	// partial stamp would mark a failed lens reviewed-through-now and let it drift. Per-lens
	// watermarks already advanced per committed window inside foldLensWindowed (the #55
	// per-lens state), so a persistent single-lens failure no longer strands healthy lenses.
	if len(failed) > 0 {
		return fmt.Errorf("review failed for %d lens(es): %s (committed windows kept; review left pending)",
			len(failed), strings.Join(failed, ", "))
	}
	return r.Store.StampReview()
}

// foldLensWindowed folds one lens's unreviewed delta into L2 in size-bounded windows
// (issue #123). It reads the delta in SEQ order (not the ts order of the incremental
// read) so each window is a CONTIGUOUS seq range — advancing the watermark to a window's
// max seq then never skips a low-seq/high-ts obs that a ts-ordered read would place in a
// later window. seq is a monotonic AUTOINCREMENT (never reused after a delete — #125), so
// a re-mined obs always lands above the watermark and is re-read. Per window: fold against
// the CURRENT stance (re-read each iteration, so a later window sees an earlier window's
// just-written facets and can reinforce/contradict them), WriteFacets, then StampReviewLens
// THROUGH that window's max seq. So each window is a durable mini-review: partial progress
// sticks, a failed or crashed window strands only itself, and the next Run resumes from the
// watermark. The window size is store.Config.ReviewMaxChars serialized-slimObs characters —
// a latency ceiling that keeps any one review call well under the runner's 10-min wall.
func (r *Reviewer) foldLensWindowed(ctx context.Context, ln *lens.Lens, nowStr string) error {
	budget := r.Config.ReviewMaxChars
	if budget <= 0 {
		budget = store.DefaultReviewMaxChars // defensive: never fold unbounded (the #123 stall)
	}
	obs, err := r.Store.ReadObservationsSinceOrdered(ln.Name, r.Store.ReviewRowid(ln.Name))
	if err != nil {
		return fmt.Errorf("read L1 delta: %w", err)
	}
	for start := 0; start < len(obs); {
		end := windowEnd(obs, start, budget) // [start,end): a size-bounded, ≥1-obs window
		window := obs[start:end]

		// Fold against the CURRENT stance (includes prior windows' writes this Run).
		prior, err := r.Store.ReadFacets()
		if err != nil {
			return fmt.Errorf("read L2: %w", err)
		}
		reviewed, err := r.reviewLens(ctx, ln, window, prior)
		if err != nil {
			// Surface the cause (was silently discarded pre-#123). Windows already
			// committed stay; the watermark sits at the last good window; resume next pass.
			return fmt.Errorf("review window [%d obs, through seq %d]: %w", len(window), window[len(window)-1].Rowid, err)
		}
		byKey := indexFacets(prior)
		for _, rf := range reviewed {
			// A malformed assertion is dropped, not merged (see wellFormed). Log it: it
			// means this lens's review prompt is returning entries the schema forbids, and
			// silence there is how a prompt regression goes unnoticed for weeks.
			if !r.applyFacet(byKey, ln.Name, rf, nowStr) {
				slog.Warn("review: dropped a malformed facet assertion (empty dimension, key, or value)",
					"lens", ln.Name, "dimension", rf.Dimension, "key", rf.Key,
					"value_empty", strings.TrimSpace(rf.Value) == "", "contradicts_prior", rf.Contradicts)
			}
		}
		if err := r.Store.WriteFacets(collectFacets(byKey)); err != nil {
			return fmt.Errorf("write L2: %w", err)
		}
		// Advance ONLY AFTER the write (crash-safety) and ONLY through this window's max
		// seq (contiguous — never past unfolded later windows).
		if err := r.Store.StampReviewLens(ln.Name, window[len(window)-1].Rowid); err != nil {
			return fmt.Errorf("stamp watermark: %w", err)
		}
		start = end
	}
	return nil
}

// windowEnd returns the exclusive end index of the next fold window starting at `start`:
// the largest end such that the serialized size of obs[start:end] stays within budget,
// but ALWAYS at least one obs (the lone-giant floor — a single obs larger than the whole
// budget folds alone rather than wedging the loop, mirroring drainWindow's in-flight==0
// floor). Sizes each obs's serialized slimObs once and accumulates (no O(n²) re-marshal).
func windowEnd(obs []store.Observation, start, budget int) int {
	total := 0
	end := start
	for end < len(obs) {
		sz := len(mustJSON(slimObs(obs[end : end+1])))
		if end > start && total+sz > budget {
			break // adding this obs would overflow; cut the window before it
		}
		total += sz
		end++
	}
	return end
}

// applyFacet enforces the bi-temporal rule deterministically. It reports whether the
// asserted facet was applied; a malformed one is REJECTED rather than merged (see
// wellFormed) so the caller can log it instead of silently corrupting L2.
func (r *Reviewer) applyFacet(byKey map[string]*store.Facet, lensName string, rf reviewedFacet, nowStr string) bool {
	if !wellFormed(rf) {
		return false
	}
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
		return true
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
	return true
}

// wellFormed rejects a facet assertion that cannot identify or state anything.
//
// A reply is model output, so every field is optional in practice, and applyFacet used to
// trust all three. Two ways that destroyed L2, both reproduced before this guard existed:
//
//   - contradicts_prior:true with an EMPTY value closed the good current version (stamping
//     ValidTo) and opened an empty one — so the facet's current stance became "", and the
//     real value was now historical. The profile then rendered a blank, and because the
//     watermark had advanced past those observations, nothing would ever re-assert it.
//   - an empty dimension/key minted a junk facet under the id "<lens>||", which no lens
//     prompt can ever reinforce or supersede, so it lingers in L2 and in the profile input
//     forever.
//
// Dimension and key are the facet's IDENTITY and value is its entire content, so an empty
// one is not a weak assertion — it is not an assertion. The emergent path already applied
// exactly this check on its own verify replies (emergent.go); this moves the rule to where
// BOTH paths pass through. Confidence is deliberately NOT screened: 0 is a meaningful
// "asserted but unsure", and clampConf already bounds it.
func wellFormed(rf reviewedFacet) bool {
	return strings.TrimSpace(rf.Dimension) != "" &&
		strings.TrimSpace(rf.Key) != "" &&
		strings.TrimSpace(rf.Value) != ""
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
// `session` is included so the reviewer can weight INDEPENDENT recurrence across
// sessions (strong reinforcement) more heavily than repeats inside one episode — the
// signal the append-only L1 now preserves (issue #16 / the keep-everything world).
func slimObs(obs []store.Observation) []map[string]any {
	out := make([]map[string]any, 0, len(obs))
	for _, o := range obs {
		out = append(out, map[string]any{
			"id": o.ID, "ts": o.TS, "session": o.Session, "dimension": o.Dimension,
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
		conf := 0.0
		if cur != nil {
			val = cur.Value
			conf = cur.Confidence
		}
		// current_confidence is REQUIRED, not decorative: applyFacet keeps
		// max(current, returned) confidence on reinforcement (reviewer.go), so a
		// reinforcement is a silent no-op unless the model can see the current value
		// and return one above it. Exposing it is what lets repeated evidence actually
		// raise confidence (the keep-everything reinforcement signal, issue #16).
		out = append(out, map[string]any{
			"dimension": f.Dimension, "key": f.Key,
			"current_value": val, "current_confidence": conf,
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
