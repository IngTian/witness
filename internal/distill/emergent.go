package distill

import (
	"math"
	"sort"

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
