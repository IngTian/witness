package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
)

// emergent.go is the S3 long-arc hypothesis engine (issue #16): it turns one lens's
// L1 observations into candidate emergent-arc clusters — patterns that recur across
// many sessions but that the incremental fold (S2), being Markovian on the current
// stance, never crystallized because each occurrence alone looked sub-threshold.
//
// This file is the PURE, deterministic half: no LLM, no store writes. Candidates()
// takes obs (+ e5 vectors) and the current facets and returns ranked CandidateArcs
// for a later LLM-verify step (S3b) to judge. Being pure, it is fully unit-testable
// with synthetic vectors and can be dry-run/inspected on a real archive clone before
// any LLM budget is spent.
//
// The geometry (see the S3 design spec + internal/vector/cluster.go):
//   - mutual-kNN graph over a k-BAND, keeping only components that PERSIST across the
//     band — base-invariant, so there is no magic "which k / which log base" knob and
//     no cosine-threshold blob;
//   - mandatory centroid RE-EXPANSION per component — re-gathers a split arc's other
//     half before the judge sees it (the fragmentation mitigation);
//   - a >=2-session emergence GATE (recurred across more than one session);
//   - best-single-facet coverage ANNOTATION (never a gate) + a salience sort so the
//     genuinely-uncovered arcs rank first for the budget-bounded verify.

// CandidateArc is one proposed emergent arc: a set of observations that cluster
// together across sessions, annotated with durability + existing-facet-coverage
// signals for the verify step. Nothing here is persisted.
type CandidateArc struct {
	Members           []store.Observation
	Sessions          []string // distinct session ids, sorted
	DistinctDays      int      // distinct calendar days spanned (durability signal)
	SpanFrom, SpanTo  string   // min/max Observation.TS
	BestFacetCoverage float64  // max over facets of |members ∩ facet.because_of| / |members|
	CoveringFacet     string   // "dimension|key" of the argmax facet (for the verify note)
}

// kBand returns the sweep of k values [kLo, kHi] the persistence filter runs over.
// kHi = ceil(log2 n) (clamped to <= n-1); kLo = 2 (the smallest k that can form a
// multi-node component at all — k=1 makes only reciprocal-nearest pairs). Persistence
// across the band makes the result invariant to the log base (the "which k" fix), and
// starting the band LOW is what lets tight distinct clusters separate: a large k links
// across cluster boundaries (percolates), so the low end of the band is where genuine
// components form and the persistence filter keeps only those stable as k rises.
func kBand(n int) []int {
	if n < 3 {
		return []int{1}
	}
	kHi := int(math.Ceil(math.Log2(float64(n))))
	if kHi > n-1 {
		kHi = n - 1
	}
	kLo := 2
	if kLo > kHi {
		kLo = kHi
	}
	band := make([]int, 0, kHi-kLo+1)
	for k := kLo; k <= kHi; k++ {
		band = append(band, k)
	}
	return band
}

// Candidates builds ranked emergent-arc candidates for one lens. Pure over its inputs.
func Candidates(obs []store.Observation, facets []store.Facet, lens string) []CandidateArc {
	band := kBand(len(obs))

	// Persistence at the PAIR level (base-invariant, and robust to a cluster gaining or
	// shedding fringe members as k rises — exact member-set identity is NOT, since a
	// component that grows by one obs across the band looks like a different cluster each
	// k). Two observations are "stably together" if they co-occur in the same connected
	// component at >= half the band's k values; connected components of that co-membership
	// graph are the persistent clusters. A transient blob that only forms at the top of
	// the band never accrues enough co-occurrences to survive.
	idx := indexObs(obs, lens) // stable index over the lens's (embeddable) obs
	m := len(idx.obs)
	need := (len(band) + 1) / 2 // >= half the band (ceil)
	together := make(map[int64]int)
	for _, k := range band {
		nodes, adj := vector.MutualKNNAdj(obs, lens, k)
		for _, cc := range vector.ConnectedComponents(adj) {
			if len(cc) < 2 {
				continue
			}
			// map node indices (into `nodes`) to our stable index, then count all pairs.
			gi := make([]int, 0, len(cc))
			for _, ni := range cc {
				if si, ok := idx.byID[nodes[ni].ID]; ok {
					gi = append(gi, si)
				}
			}
			for a := 0; a < len(gi); a++ {
				for b := a + 1; b < len(gi); b++ {
					together[pairKey(gi[a], gi[b])]++
				}
			}
		}
	}
	// Build the co-membership adjacency (edge iff the pair was together at >= need k's).
	adj := make([][]int, m)
	for key, cnt := range together {
		if cnt < need {
			continue
		}
		a, b := unpairKey(key)
		adj[a] = append(adj[a], b)
		adj[b] = append(adj[b], a)
	}

	var out []CandidateArc
	for _, cc := range vector.ConnectedComponents(adj) {
		if len(cc) < 2 {
			continue // a lone obs is not an arc
		}
		members := make([]store.Observation, 0, len(cc))
		for _, si := range cc {
			members = append(members, idx.obs[si])
		}
		members = reexpand(members, obs, lens) // mandatory fragmentation mitigation

		sessions := distinctSessions(members)
		if len(sessions) < 2 {
			continue // emergence floor: must have recurred across >1 session
		}
		cov, coveringFacet := bestFacetCoverage(members, facets, lens)
		days, from, to := timeSpan(members)
		out = append(out, CandidateArc{
			Members:           members,
			Sessions:          sessions,
			DistinctDays:      days,
			SpanFrom:          from,
			SpanTo:            to,
			BestFacetCoverage: cov,
			CoveringFacet:     coveringFacet,
		})
	}

	// De-dup arcs that re-expanded to the same member set (different base components can
	// converge after re-expansion).
	out = dedupArcs(out)

	// Salience sort: low coverage first (genuinely-new arcs verify first under a budget),
	// then more distinct sessions, then higher Σpoignancy, then larger — all parameter-free.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.BestFacetCoverage != b.BestFacetCoverage {
			return a.BestFacetCoverage < b.BestFacetCoverage
		}
		if len(a.Sessions) != len(b.Sessions) {
			return len(a.Sessions) > len(b.Sessions)
		}
		if pa, pb := sumPoignancy(a.Members), sumPoignancy(b.Members); pa != pb {
			return pa > pb
		}
		return len(a.Members) > len(b.Members)
	})
	return out
}

// reexpand re-gathers observations near a component's centroid whose cosine to it is
// >= the component's own minimum member-to-centroid cosine (the cluster's data-own
// radius — no magic constant). This converts a hard partition into a soft one so a
// split arc's other half rejoins before the verify step.
func reexpand(members, all []store.Observation, lens string) []store.Observation {
	if len(members) < 2 {
		return members
	}
	vecs := make([][]float32, 0, len(members))
	for _, m := range members {
		if len(m.Embedding) > 0 {
			vecs = append(vecs, m.Embedding)
		}
	}
	centroid := vector.Centroid(vecs)
	if len(centroid) == 0 {
		return members
	}
	// The cluster's own radius: the smallest cosine of any member to the centroid.
	radius := math.MaxFloat64
	for _, m := range members {
		if len(m.Embedding) == 0 {
			continue
		}
		if s := cosine(centroid, m.Embedding); s < radius {
			radius = s
		}
	}
	have := map[string]bool{}
	for _, m := range members {
		have[m.ID] = true
	}
	expanded := append([]store.Observation(nil), members...)
	for _, o := range all {
		if lens != "" && o.Lens != lens || have[o.ID] || len(o.Embedding) == 0 {
			continue
		}
		if cosine(centroid, o.Embedding) >= radius {
			expanded = append(expanded, o)
			have[o.ID] = true
		}
	}
	return expanded
}

// bestFacetCoverage = max over facets of the fraction of members cited by that single
// facet's because_of, plus the "dimension|key" of the argmax facet. Best SINGLE facet
// (not the union): a cross-cutting arc whose members are each attached to DIFFERENT
// facets is correctly still uncovered by any one of them.
func bestFacetCoverage(members []store.Observation, facets []store.Facet, lens string) (float64, string) {
	if len(members) == 0 {
		return 0, ""
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m.ID] = true
	}
	best := 0.0
	bestKey := ""
	for _, f := range facets {
		if f.Lens != lens {
			continue
		}
		cited := map[string]bool{}
		for _, v := range f.Versions {
			for _, id := range v.BecauseOf {
				cited[id] = true
			}
		}
		hit := 0
		for id := range memberSet {
			if cited[id] {
				hit++
			}
		}
		if cov := float64(hit) / float64(len(members)); cov > best {
			best = cov
			bestKey = f.Dimension + "|" + f.Key
		}
	}
	return best, bestKey
}

func distinctSessions(members []store.Observation) []string {
	set := map[string]bool{}
	for _, m := range members {
		set[m.Session] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// timeSpan returns distinct calendar days (date prefix of TS) and the min/max TS.
func timeSpan(members []store.Observation) (days int, from, to string) {
	dset := map[string]bool{}
	for _, m := range members {
		if m.TS == "" {
			continue
		}
		date := m.TS
		if len(date) >= 10 {
			date = date[:10]
		}
		dset[date] = true
		if from == "" || m.TS < from {
			from = m.TS
		}
		if to == "" || m.TS > to {
			to = m.TS
		}
	}
	return len(dset), from, to
}

func sumPoignancy(members []store.Observation) int {
	n := 0
	for _, m := range members {
		n += m.Poignancy
	}
	return n
}

func memberIDs(members []store.Observation) []string {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

// obsIndex is a stable indexing of a lens's embeddable observations, so the pair-level
// persistence graph can use dense int indices regardless of MutualKNNAdj's own ordering.
type obsIndex struct {
	obs  []store.Observation
	byID map[string]int
}

func indexObs(obs []store.Observation, lens string) obsIndex {
	ix := obsIndex{byID: map[string]int{}}
	for _, o := range obs {
		if lens != "" && o.Lens != lens || len(o.Embedding) == 0 {
			continue
		}
		ix.byID[o.ID] = len(ix.obs)
		ix.obs = append(ix.obs, o)
	}
	return ix
}

// pairKey/unpairKey pack an unordered index pair (a<b) into one int64 key.
func pairKey(a, b int) int64 {
	if a > b {
		a, b = b, a
	}
	return int64(a)<<32 | int64(uint32(b))
}
func unpairKey(k int64) (int, int) { return int(k >> 32), int(uint32(k)) }

// signature is the order-independent identity of a component: its sorted member ids.
func signature(members []store.Observation) string {
	ids := memberIDs(members)
	s := ""
	for _, id := range ids {
		s += id + "\x00"
	}
	return s
}

func dedupArcs(arcs []CandidateArc) []CandidateArc {
	seen := map[string]bool{}
	var out []CandidateArc
	for _, a := range arcs {
		sig := signature(a.Members)
		if seen[sig] {
			continue
		}
		seen[sig] = true
		out = append(out, a)
	}
	return out
}

// cosine of two L2-normalized float32 vectors (dot product). Local helper mirroring
// embed.Cosine's contract without importing it here (vector.go already re-normalizes).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// --- S3b: verify + merge + idempotency -------------------------------------------

// EmergentReviewer runs the S3 long-arc retrieval pass: for each lens it clusters L1
// (Candidates, the pure hypothesis engine above), verifies each candidate arc with one
// bounded LLM call (the judge), and merges accepted arcs into L2 via the same
// bi-temporal applyFacet path the ordinary review uses. It keeps its OWN idempotency
// state (cluster signatures in meta-KV) and NEVER advances the S2 review watermark
// (issue #16 §5c) — the sequential fold remains the sole owner of review_rowid.
type EmergentReviewer struct {
	Store     store.EmergentStore
	Lenses    []*lens.Lens
	Config    store.Config
	Runner    MineFunc                     // required; production wires a real runner, tests inject a fake
	RunnerFor func(ln *lens.Lens) MineFunc // per-lens runner seam (#75); nil → Runner
	// MaxVerify caps verify calls per lens per pass (the explicit budget knob, §2d).
	// 0 = unbounded (the first cold-start pass); a periodic re-run sets a ceiling.
	MaxVerify int
}

func (r *EmergentReviewer) runnerFor(ln *lens.Lens) MineFunc {
	if r.RunnerFor != nil {
		if fn := r.RunnerFor(ln); fn != nil {
			return fn
		}
	}
	return r.Runner
}

// RunFull runs the emergent pass over every lens. Errors from one lens/candidate are
// logged and skipped (best-effort, like the ordinary review's per-lens isolation) so a
// single bad verify never sinks the whole pass.
func (r *EmergentReviewer) RunFull(ctx context.Context, now time.Time) error {
	nowStr := now.UTC().Format(time.RFC3339)
	facets, err := r.Store.ReadFacets()
	if err != nil {
		return fmt.Errorf("read L2: %w", err)
	}
	byKey := indexFacets(facets)
	rv := &Reviewer{} // borrow applyFacet's bi-temporal merge logic (no store/runner needed)

	// ACCEPTED signatures are held back until L2 is durable. markSeen used to run for both
	// outcomes right after verify, but the arc facets are only written at the END of the
	// pass — so a failing WriteFacets, or a crash/SIGKILL anywhere in the remaining lenses,
	// left the cluster recorded as verified-and-accepted with its facet never stored.
	// seenUnchanged then skips it on every future pass: the arc is lost permanently, and
	// silently, because the signature is a pure function of the member ids (a re-mine
	// reproduces identical obs ids, so nothing ever perturbs it back into the queue).
	//
	// REJECTIONS are stamped immediately and deliberately: there is nothing to persist for
	// them, so they carry no such ordering hazard, and stamping them as we go means a later
	// failure doesn't buy a re-run of every expensive no-op verify.
	type pendingMark struct {
		lens, sig string
		cand      CandidateArc
	}
	var accepted []pendingMark

	dirty := false
	for _, ln := range r.Lenses {
		obs, err := r.Store.ReadObservations(ln.Name)
		if err != nil {
			slog.Error("emergent: read observations", "lens", ln.Name, "err", err)
			continue
		}
		cands := Candidates(obs, facets, ln.Name)
		verified := 0
		for _, c := range cands {
			if r.MaxVerify > 0 && verified >= r.MaxVerify {
				break // budget exhausted — remaining candidates are DEFERRED (not marked seen)
			}
			sig := arcSignature(ln.Name, c.Members)
			if r.seenUnchanged(ln.Name, sig, c) {
				continue // already verified this exact cluster; skip (idempotency)
			}
			rf, ok, err := r.verify(ctx, ln, c)
			if err != nil {
				slog.Error("emergent: verify candidate", "lens", ln.Name, "err", err)
				continue // NOT marked seen → re-proposed next pass
			}
			verified++
			if !ok {
				r.markSeen(ln.Name, sig, c, false) // rejected: nothing to persist, safe to stamp now
				continue
			}
			rf.BecauseOf = memberIDs(c.Members) // ground the facet in the whole cluster
			// verify() already screens for a well-formed facet, so applyFacet's own guard
			// should never fire here; only set dirty when something actually merged, so a
			// rejected assertion can't trigger a no-op L2 rewrite.
			if !rv.applyFacet(byKey, ln.Name, rf, nowStr) {
				slog.Warn("emergent: dropped a malformed arc facet", "lens", ln.Name,
					"dimension", rf.Dimension, "key", rf.Key)
				continue
			}
			dirty = true
			accepted = append(accepted, pendingMark{ln.Name, sig, c})
		}
	}
	if dirty {
		if err := r.Store.WriteFacets(collectFacets(byKey)); err != nil {
			// Leave every accepted signature UNSTAMPED so the next pass re-verifies and
			// re-merges these arcs. Re-running verify costs model calls; losing the arc
			// forever does not cost anything visible, which is exactly why it is worse.
			return fmt.Errorf("write L2: %w", err)
		}
	}
	// L2 is durable — now it is safe to say these clusters were handled.
	for _, m := range accepted {
		r.markSeen(m.lens, m.sig, m.cand, true)
	}
	return nil
}

// verify asks the judge whether a candidate cluster is a real coherent arc. Returns the
// facet to merge (ok=true) or a rejection (ok=false). Reuses the reviewedFacet contract
// and the per-lens review model; the prompt is the lens's Emerge prompt, falling back to
// Review when the lens has no emerge.md.
func (r *EmergentReviewer) verify(ctx context.Context, ln *lens.Lens, c CandidateArc) (reviewedFacet, bool, error) {
	prompt := ln.Emerge
	if strings.TrimSpace(prompt) == "" {
		prompt = ln.Review
	}
	input := emergeInput(c)
	reply, err := r.runnerFor(ln)(ctx, ModelFor(r.Config, ln, PhaseReview), prompt, input)
	if err != nil {
		return reviewedFacet{}, false, err
	}
	arr, perr := ParseJSONArray[reviewedFacet](reply)
	// A TRUNCATED reply is not a rejection — it is a retryable output failure, and the two must
	// not collapse. RunFull calls markSeen(…, false) on a rejection, which stamps this exact
	// cluster signature as judged FOREVER: a reply cut off mid-array (an output-token cap, a
	// killed child, a dropped stream) would permanently bury an arc the judge never actually
	// ruled on. Returning the error instead takes the `err != nil` path in RunFull, which
	// deliberately does NOT mark seen, so the arc is re-proposed next pass. This mirrors the mine
	// path, where ErrTruncatedJSONArray is explicitly retryable rather than treated as drift.
	if errors.Is(perr, ErrTruncatedJSONArray) {
		return reviewedFacet{}, false, fmt.Errorf("verify reply truncated (retryable, arc not judged): %w", perr)
	}
	// Everything else — no array at all (drift/prose), or an explicit empty array — IS a
	// rejection: the judge answered, and the answer was "not a real arc".
	if perr != nil || len(arr) == 0 {
		return reviewedFacet{}, false, nil
	}
	// The verify judges ONE cluster; take the first well-formed facet it returns.
	for _, rf := range arr {
		// wellFormed (reviewer.go) is the SAME rule on the SAME type — call it rather than
		// re-inlining, so the two paths cannot drift on what "well-formed" means.
		if wellFormed(rf) {
			return rf, true, nil
		}
	}
	return reviewedFacet{}, false, nil
}

// emergeInput renders the candidate for the judge: its member observations (full text)
// plus the durability + coverage annotations. The coverage note tells the judge to
// REUSE an overlapping facet's dimension|key if this is that arc extended (so a re-run
// reinforces rather than duplicates), or decline if it adds nothing.
func emergeInput(c CandidateArc) string {
	note := ""
	if c.CoveringFacet != "" && c.BestFacetCoverage > 0 {
		note = fmt.Sprintf("\n\nNOTE: an existing facet (%s) already cites %.0f%% of these observations. "+
			"If this is that facet's pattern extended, REUSE its exact dimension+key so it reinforces; "+
			"if the observations add nothing new, decline (return []).", c.CoveringFacet, c.BestFacetCoverage*100)
	}
	return fmt.Sprintf("CANDIDATE ARC: %d observations across %d sessions, %d distinct days (%s..%s).%s\n\nOBSERVATIONS (L1):\n%s",
		len(c.Members), len(c.Sessions), c.DistinctDays, shortDate(c.SpanFrom), shortDate(c.SpanTo), note,
		mustJSON(slimObs(c.Members)))
}

func shortDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// --- idempotency state (meta-KV signatures; issue #16 §5b) -------------------------

// arcSignature is the stable identity of a cluster: sha256 of its sorted member ids,
// namespaced by lens. Persisted so a re-run over the SAME cluster is skipped rather than
// re-verified (and never mints a duplicate facet).
func arcSignature(lens string, members []store.Observation) string {
	h := sha256.Sum256([]byte(lens + "\x00" + strings.Join(memberIDs(members), ",")))
	return hex.EncodeToString(h[:12])
}

func emergeSeenKey(lens, sig string) string { return "emerge_seen:" + lens + ":" + sig }

type emergeSeen struct {
	Outcome     string `json:"outcome"` // "accepted" | "rejected"
	MemberCount int    `json:"members"` // for the membership-delta re-verify test
}

// seenUnchanged reports whether this exact cluster signature was already verified and its
// membership has not grown — in which case verifying again is wasted (idempotency). A
// grown cluster (more members than last time) is re-verified so an S1 append-only
// recurrence joining the arc is folded in.
func (r *EmergentReviewer) seenUnchanged(lens, sig string, c CandidateArc) bool {
	raw := r.Store.MetaString(emergeSeenKey(lens, sig))
	if raw == "" {
		return false
	}
	var s emergeSeen
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return false
	}
	// Same signature == same member-id set, so the count cannot have grown under an
	// unchanged signature; the count guard is belt-and-suspenders for a future looser
	// signature. Unchanged → skip.
	return len(c.Members) <= s.MemberCount
}

func (r *EmergentReviewer) markSeen(lens, sig string, c CandidateArc, accepted bool) {
	outcome := "rejected"
	if accepted {
		outcome = "accepted"
	}
	b, _ := json.Marshal(emergeSeen{Outcome: outcome, MemberCount: len(c.Members)})
	if err := r.Store.SetMetaString(emergeSeenKey(lens, sig), string(b)); err != nil {
		slog.Warn("emergent: could not persist cluster signature", "lens", lens, "err", err)
	}
}
