package distill

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
)

// Candidates() is the pure S3 hypothesis engine (issue #16 long-arc retrieval): given
// a lens's observations (+ e5 vectors) and the current facets, propose emergent-arc
// clusters WITHOUT any LLM or store write. These tests pin its contract with synthetic
// vectors so the geometry is provable offline.

// dirObs builds an observation with an L2-normalized embedding pointing in `dir`, so
// cosine neighborhoods are controllable by construction.
func dirObs(id, session string, dir []float32) store.Observation {
	return store.Observation{ID: id, Lens: "math", Session: session, Dimension: "thinking",
		Observation: id, Poignancy: 3, TS: "2026-01-01T00:00:00Z", Embedding: normVec(dir)}
}

func normVec(dir []float32) []float32 {
	var norm float64
	for _, x := range dir {
		norm += float64(x) * float64(x)
	}
	emb := make([]float32, len(dir))
	if norm > 0 {
		inv := float32(1.0 / math.Sqrt(norm))
		for i, x := range dir {
			emb[i] = x * inv
		}
	}
	return emb
}

// genArc builds a tight cluster of `count` observations near `dir` (tiny jitter keeps
// them close but distinct), one per session starting at sessOffset. Realistic cluster
// sizes matter: at very small n the k-band is forced into the percolation regime, which
// is a property of tiny inputs, not of the algorithm (S3 targets large archives).
func genArc(prefix string, count int, dir []float32, sessOffset int) []store.Observation {
	out := make([]store.Observation, 0, count)
	for i := 0; i < count; i++ {
		d := append([]float32(nil), dir...)
		d[1] += float32(i) * 0.004 // tiny jitter within the cluster
		out = append(out, store.Observation{
			ID: prefix + string(rune('0'+i)), Lens: "math",
			Session: "s" + string(rune('a'+sessOffset+i)), Dimension: "thinking",
			Observation: prefix + string(rune('0'+i)), Poignancy: 3,
			TS: "2026-01-01T00:00:00Z", Embedding: normVec(d),
		})
	}
	return out
}

func arcIDs(prefix string, count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = prefix + string(rune('0'+i))
	}
	return ids
}

// TestCandidatesCrossSessionDenseClusterSurfaces: a tight cluster of obs spanning
// several DISTINCT sessions, not covered by any facet, must be proposed as a candidate.
func TestCandidatesCrossSessionDenseClusterSurfaces(t *testing.T) {
	var obs []store.Observation
	obs = append(obs, genArc("a", 6, []float32{1, 0, 0}, 0)...) // the tight cross-session arc
	// A few unrelated scattered obs far away (distinct directions, distinct sessions).
	obs = append(obs,
		dirObs("x0", "sx", []float32{0, 1, 0}),
		dirObs("x1", "sy", []float32{0, 0, 1}),
	)
	cands := Candidates(obs, nil, "math", nil)
	if len(cands) == 0 {
		t.Fatal("expected at least one candidate arc from the dense cross-session cluster")
	}
	top := cands[0]
	if len(top.Members) < 3 {
		t.Fatalf("top candidate should gather the dense arc (>=3 members), got %d", len(top.Members))
	}
	if len(top.Sessions) < 2 {
		t.Fatalf("candidate must span >=2 sessions, got %d", len(top.Sessions))
	}
	// Its members should be the arc (a*), not the scattered x*.
	for _, m := range top.Members {
		if m.ID[0] != 'a' {
			t.Fatalf("candidate contaminated by a scattered obs %q", m.ID)
		}
	}
}

// TestCandidatesSingleSessionClusterFiltered: a dense cluster ALL in one session is
// NOT an emergent arc (it's that session's topic — per-session mining + the fold
// already handle it). The >=2-session floor must drop it.
func TestCandidatesSingleSessionClusterFiltered(t *testing.T) {
	obs := []store.Observation{
		dirObs("a1", "s1", []float32{1, 0, 0}),
		dirObs("a2", "s1", []float32{0.99, 0.01, 0}),
		dirObs("a3", "s1", []float32{0.98, 0.02, 0}),
		dirObs("a4", "s1", []float32{0.985, 0.015, 0}),
		// a second distinct cluster to keep the graph non-trivial, also single-session
		dirObs("b1", "s2", []float32{0, 1, 0}),
	}
	cands := Candidates(obs, nil, "math", nil)
	for _, c := range cands {
		if len(c.Sessions) < 2 {
			t.Fatalf("a single-session cluster must be filtered out; got candidate with sessions %v", c.Sessions)
		}
	}
}

// TestCandidatesCoverageAnnotatedNotGated: a cluster whose members are ALL cited by
// an existing facet is still returned (annotate-don't-gate), but with high coverage
// and the covering facet named — so the salience rank sinks it, without dropping it.
func TestCandidatesCoverageAnnotated(t *testing.T) {
	obs := genArc("c", 7, []float32{1, 0, 0}, 0)
	facets := []store.Facet{{
		Lens: "math", Dimension: "thinking", Key: "known_thing",
		Versions: []store.FacetVersion{{Value: "v", BecauseOf: arcIDs("c", 7)}},
	}}
	cands := Candidates(obs, facets, "math", nil)
	if len(cands) == 0 {
		t.Fatal("a fully-covered cluster must still be RETURNED (annotate, not gate)")
	}
	top := cands[0]
	if top.BestFacetCoverage < 0.99 {
		t.Fatalf("fully-cited cluster should have coverage ~1.0, got %.2f", top.BestFacetCoverage)
	}
	if top.CoveringFacet == "" {
		t.Fatal("covering facet key must be annotated")
	}
}

// TestCandidatesSalienceRanksUncoveredFirst: an uncovered emergent cluster must rank
// AHEAD of a covered one (low coverage first), so a budget cutoff verifies the
// genuinely-new arcs first.
func TestCandidatesSalienceRanksUncoveredFirst(t *testing.T) {
	var obs []store.Observation
	obs = append(obs, genArc("u", 6, []float32{1, 0, 0}, 0)...)  // uncovered arc
	obs = append(obs, genArc("k", 6, []float32{0, 1, 0}, 10)...) // covered arc (orthogonal)
	facets := []store.Facet{{
		Lens: "math", Dimension: "thinking", Key: "known",
		Versions: []store.FacetVersion{{Value: "v", BecauseOf: arcIDs("k", 6)}},
	}}
	cands := Candidates(obs, facets, "math", nil)
	if len(cands) < 2 {
		t.Fatalf("expected both arcs as candidates, got %d", len(cands))
	}
	// The first candidate must be the UNCOVERED one (lower coverage ranks first).
	if cands[0].BestFacetCoverage > cands[len(cands)-1].BestFacetCoverage {
		t.Fatalf("salience must rank low-coverage first; got first=%.2f last=%.2f",
			cands[0].BestFacetCoverage, cands[len(cands)-1].BestFacetCoverage)
	}
	if cands[0].BestFacetCoverage > 0.5 {
		t.Fatalf("top candidate should be the uncovered arc (cov~0), got %.2f", cands[0].BestFacetCoverage)
	}
}

// clusteredObs is n seeded observations around a few random centres: 1 in 4 copies an earlier
// row's embedding exactly (a cosine tie), 1 in 15 is unembedded, 1 in 15 is on another lens.
func clusteredObs(n int, seed uint64) []store.Observation {
	rng := rand.New(rand.NewPCG(seed, uint64(n)))
	const d = 12
	centers := make([][]float32, 2+rng.IntN(8))
	for c := range centers {
		centers[c] = make([]float32, d)
		for x := range centers[c] {
			centers[c][x] = float32(rng.NormFloat64())
		}
	}
	obs := make([]store.Observation, 0, n)
	for i := range n {
		c, spread := centers[rng.IntN(len(centers))], 0.05+0.6*rng.Float64()
		v := make([]float32, d)
		for x := range v {
			v[x] = c[x] + float32(spread*rng.NormFloat64())
		}
		o := dirObs(fmt.Sprintf("o%04d", i), fmt.Sprintf("s%d", rng.IntN(30)), v)
		if i > 0 && rng.IntN(4) == 0 {
			o.Embedding = obs[rng.IntN(i)].Embedding
		}
		switch rng.IntN(15) {
		case 0:
			o.Embedding = nil
		case 1:
			o.Lens = "other"
		}
		obs = append(obs, o)
	}
	return obs
}

// TestBandTogetherMatchesPerKReference pins bandTogether — one ranking for the band's top k, each
// k read off a prefix — to the loop it replaced: a fresh MutualKNNAdj per k. The vector test
// proves MutualAt == MutualKNNAdj for any k <= kMax; this proves the band asks for the right
// kMax (MutualAt clamps, so a short one would silently give every larger k a smaller graph).
func TestBandTogetherMatchesPerKReference(t *testing.T) {
	for _, n := range []int{3, 5, 17, 60, 250} {
		for seed := uint64(1); seed <= 4; seed++ {
			obs := clusteredObs(n, seed)
			band, idx := kBand(len(obs)), indexObs(obs, "math")
			want := make(map[int64]int)
			for _, k := range band {
				nodes, adj := vector.MutualKNNAdj(obs, "math", k)
				for _, cc := range vector.ConnectedComponents(adj) {
					for a := range cc {
						for b := a + 1; b < len(cc); b++ {
							want[pairKey(idx.byID[nodes[cc[a]].ID], idx.byID[nodes[cc[b]].ID])]++
						}
					}
				}
			}
			if got := bandTogether(obs, "math", band, idx, nil); !maps.Equal(got, want) {
				t.Fatalf("n=%d seed=%d band=%v: %d pairs counted, per-k MutualKNNAdj counts %d (or the counts differ)",
					n, seed, band, len(got), len(want))
			}
		}
	}
}
