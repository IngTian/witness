package distill

import (
	"math"
	"testing"

	"github.com/IngTian/witness/internal/store"
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
	cands := Candidates(obs, nil, "math")
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
	cands := Candidates(obs, nil, "math")
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
	cands := Candidates(obs, facets, "math")
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
	cands := Candidates(obs, facets, "math")
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
