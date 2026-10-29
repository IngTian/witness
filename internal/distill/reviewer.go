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
	return resolveRunner(r.RunnerFor, ln, r.Runner)
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
	case rf.Contradicts && cur.Value != rf.Value:
		// Sustained contradiction => record a change arc: close the old, open the new.
		// (The "sustained" judgment is the review prompt's job; code just applies it.)
		f.Versions[len(f.Versions)-1].ValidTo = nowStr
		f.Versions = append(f.Versions, store.FacetVersion{
			Value: rf.Value, ValidFrom: nowStr, RecordedAt: nowStr,
			BecauseOf: rf.BecauseOf, Confidence: clampConf(rf.Confidence),
		})
	default:
		// Same value reaffirmed: reinforce (raise confidence, refresh provenance).
		cur.Confidence = clampConf(max(cur.Confidence, rf.Confidence))
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
//
// Scope, stated plainly because the first bullet above invites the wrong inference: this guard
// stops the L2 CORRUPTION, not the watermark advance. foldLensWindowed still stamps through the
// window even when every assertion in it was dropped, so those observations are not re-offered.
// That is deliberate — withholding the stamp would make one bad review prompt re-fold the same
// window forever, the wedge the drift-advances rule exists to prevent — and it is why the drop is
// logged at WARN rather than swallowed: the log is the only signal that a prompt regression is
// eating a lens's assertions. The emergent path chooses the opposite tradeoff (it withholds
// markSeen until after WriteFacets, so an unwritten arc is re-proposed) because a re-proposed arc
// costs one verify call, while a re-folded window costs a full review of the whole window.
func wellFormed(rf reviewedFacet) bool {
	return strings.TrimSpace(rf.Dimension) != "" &&
		strings.TrimSpace(rf.Key) != "" &&
		strings.TrimSpace(rf.Value) != ""
}

// reviewLens folds ONE window against the current stance.
//
// Block order is deliberate. The prior-facet index is bulk reference material the model scans
// for identity matching, so it sits first/middle where positional attention is weakest; the new
// observations — the only thing this call is actually judging — sit LAST, adjacent to the task.
// Before this was budgeted the proportions were 99.4% prior state to 0.6% new evidence.
func (r *Reviewer) reviewLens(ctx context.Context, ln *lens.Lens, obs []store.Observation, prior []store.Facet) ([]reviewedFacet, error) {
	clip, topFull := r.facetBudget()
	priorText, shownFull, indexN, fullN := renderPriorFacets(prior, ln.Name, clip, topFull)
	obsText := mustJSON(slimObs(obs))
	input := priorText + "\nOBSERVATIONS (L1):\n" + obsText

	reply, err := r.runnerFor(ln)(ctx, ModelFor(r.Config, ln, PhaseReview), ln.Review, input)
	if err != nil {
		return nil, err
	}
	reviewed, err := ParseJSONArray[reviewedFacet](reply)
	if err != nil {
		return nil, err
	}
	// Review logged NOTHING about payload or outcome, which is why an input-size refusal and
	// an output truncation were indistinguishable after the fact for 24 days. mine already
	// logs input_chars; this is the review counterpart.
	slog.Info("review: folded a window",
		"lens", ln.Name, "obs", len(obs), "asserted", len(reviewed),
		"facet_index", indexN, "facet_full", fullN, "clip", clip,
		"prior_chars", len(priorText), "obs_chars", len(obsText), "reply_chars", len(reply))

	return r.guardClippedVerdicts(ctx, ln, obsText, prior, reviewed, shownFull), nil
}

// facetBudget resolves the prior-facet clip and full-prose cap, clamping an unset config to the
// measured defaults the same way foldLensWindowed clamps ReviewMaxChars. A zero-valued Config
// (tests, older config.toml) must NOT mean "unbounded" — unbounded is precisely the wedge.
func (r *Reviewer) facetBudget() (clip, topFull int) {
	clip, topFull = r.Config.ReviewFacetClip, r.Config.ReviewFacetFull
	if clip == 0 && topFull == 0 {
		return store.DefaultReviewFacetClip, store.DefaultReviewFacetFull
	}
	return clip, topFull
}

// guardClippedVerdicts is the code half of the clipping contract: a version is never closed on
// the strength of a value the model could only read truncated.
//
// The risk is specific and asymmetric. applyFacet's reinforcement branch touches only
// Confidence and BecauseOf — it never writes Value — so a truncated value echoed back as a
// REINFORCEMENT is harmless. But its change branch (`rf.Contradicts && cur.Value != rf.Value`)
// closes the stored version and opens a new one from rf.Value, so a contradiction judged
// against 160 characters would stamp ValidTo on good full prose and make a stub current. That
// corrupts the change history, which is the product's entire claim.
//
// So: drop any value echoing the clip marker, and hold back any contradiction against a facet
// that was not shown in full, re-asking ONCE with those facets' real text. Bounded by
// construction — the deferred set is only what the model itself flagged.
func (r *Reviewer) guardClippedVerdicts(ctx context.Context, ln *lens.Lens, obsText string, prior []store.Facet, reviewed []reviewedFacet, shownFull map[string]bool) []reviewedFacet {
	byKey := indexFacets(prior)
	out := make([]reviewedFacet, 0, len(reviewed))
	deferredSet := map[string]bool{}
	var deferred []reviewedFacet

	for _, rf := range reviewed {
		if truncationEchoed(rf.Value) {
			slog.Warn("review: dropped an assertion that echoed a clipped value back as its value",
				"lens", ln.Name, "dimension", rf.Dimension, "key", rf.Key, "contradicts_prior", rf.Contradicts)
			continue
		}
		key := facetKey(rf.Dimension, rf.Key)
		_, exists := byKey[ln.Name+"|"+rf.Dimension+"|"+rf.Key]
		if rf.Contradicts && exists && !shownFull[key] {
			deferredSet[key] = true
			deferred = append(deferred, rf)
			continue
		}
		out = append(out, rf)
	}
	if len(deferred) == 0 {
		return out
	}

	reply, err := r.runnerFor(ln)(ctx, ModelFor(r.Config, ln, PhaseReview), ln.Review,
		renderNamedFacets(prior, ln.Name, deferredSet)+"\nOBSERVATIONS (L1):\n"+obsText)
	if err == nil {
		var reJudged []reviewedFacet
		reJudged, err = ParseJSONArray[reviewedFacet](reply)
		if err == nil {
			kept := 0
			for _, rf := range reJudged {
				// Only the facets we asked about, and never an echoed stub.
				if deferredSet[facetKey(rf.Dimension, rf.Key)] && !truncationEchoed(rf.Value) {
					out = append(out, rf)
					kept++
				}
			}
			slog.Info("review: re-asked clipped contradictions with full values",
				"lens", ln.Name, "deferred", len(deferred), "applied", kept)
			return out
		}
	}

	// Re-ask unavailable. Degrade to REINFORCEMENT rather than dropping the evidence or
	// closing blind: applyFacet's default branch merges because_of and raises confidence
	// while leaving the stored value alone. The arc is not recorded this pass — the window's
	// watermark still advances, so it will not be re-offered — but nothing is corrupted and
	// the WARN names what was downgraded.
	slog.Warn("review: re-ask for clipped contradictions failed; applying them as reinforcements instead (no version closed)",
		"lens", ln.Name, "deferred", len(deferred), "err", err)
	for _, rf := range deferred {
		rf.Contradicts = false
		out = append(out, rf)
	}
	return out
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

// slimFacets is GONE — replaced by renderPriorFacets in facetrender.go. It emitted every
// current facet's full value as JSON with no budget, which is what refused a 2,039-facet lens
// outright for 24 days. Its one genuinely load-bearing detail is preserved there: the current
// CONFIDENCE must be visible, because applyFacet keeps max(current, returned) on
// reinforcement, so a reinforcement is a silent no-op unless the model can see and exceed the
// stored number (the keep-everything reinforcement signal, issue #16).

func clampConf(c float64) float64 {
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

// maxF and sameValue used to live here. maxF predated Go 1.21's builtin `max` (go.mod is 1.25.1) and
// sameValue was `return a == b` — each with exactly one caller, so both were indirection that made
// the call sites harder to read rather than easier.
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
