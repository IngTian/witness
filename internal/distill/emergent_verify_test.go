package distill

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// S3b: EmergentReviewer.RunFull verifies each candidate arc (LLM judge) and merges the
// accepted ones into L2, with its OWN idempotency state — never touching the S2 review
// watermark. These tests drive it against a real store + a fake verify runner.

// seedArc writes `count` tight cross-session observations near `dir` into L1 so
// Candidates() proposes them as one emergent arc.
func seedArc(t *testing.T, s *store.Store, prefix string, count int, dir []float32) {
	t.Helper()
	obs := make([]store.Observation, 0, count)
	for i := 0; i < count; i++ {
		d := append([]float32(nil), dir...)
		d[1] += float32(i) * 0.004
		var norm float64
		for _, x := range d {
			norm += float64(x) * float64(x)
		}
		emb := make([]float32, len(d))
		inv := float32(1.0 / math.Sqrt(norm))
		for j, x := range d {
			emb[j] = x * inv
		}
		obs = append(obs, store.Observation{
			ID: prefix + string(rune('0'+i)), Lens: "math",
			Session: "s" + string(rune('a'+i)), Dimension: "thinking",
			Observation: prefix + " observation " + string(rune('0'+i)), Poignancy: 3,
			TS: "2026-01-01T00:00:00Z", Embedding: emb,
		})
	}
	if err := s.AppendObservations(obs); err != nil {
		t.Fatalf("seed %s: %v", prefix, err)
	}
}

func emergentReviewer(s *store.Store, run MineFunc) *EmergentReviewer {
	return &EmergentReviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "math", Emerge: "EMERGE", Review: "REVIEW"}},
		Config: store.Config{},
		Runner: run,
	}
}

// TestRunFullAcceptedArcMergesViaApplyFacet: a verify that returns a facet writes it to
// L2 (open-ended new facet) with the candidate's member ids as because_of.
func TestRunFullAcceptedArcMergesViaApplyFacet(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})

	verify := func(_ context.Context, _, _, _ string) (string, error) {
		return `[{"dimension":"thinking","key":"abstracts_to_structure","value":"reflexively abstracts to formal structure","confidence":0.8,"because_of":["a0","a1"],"contradicts_prior":false}]`, nil
	}
	r := emergentReviewer(s, verify)
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("RunFull: %v", err)
	}
	facets, _ := s.ReadFacets()
	var found bool
	for _, f := range facets {
		if f.Lens == "math" && f.Key == "abstracts_to_structure" {
			found = true
		}
	}
	if !found {
		t.Fatal("accepted emergent arc must be written as a facet")
	}
}

// TestRunFullVerifyRejectsNoFacetWritten: a verify that returns an empty array (reject)
// writes nothing.
func TestRunFullVerifyRejectsNoFacetWritten(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})

	verify := func(_ context.Context, _, _, _ string) (string, error) { return `[]`, nil }
	r := emergentReviewer(s, verify)
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("RunFull: %v", err)
	}
	facets, _ := s.ReadFacets()
	if len(facets) != 0 {
		t.Fatalf("a rejected candidate must write no facet; got %d", len(facets))
	}
}

// TestRunFullReRunSameClusterSkipsVerify (idempotency flagship): running twice over the
// same obs must verify each candidate ONCE — the second run makes zero verify calls for
// unchanged cluster signatures and writes no duplicate facet.
func TestRunFullReRunSameClusterSkipsVerify(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})

	var calls int
	verify := func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		return `[{"dimension":"thinking","key":"k","value":"v","confidence":0.7,"because_of":["a0"],"contradicts_prior":false}]`, nil
	}
	r := emergentReviewer(s, verify)
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("first RunFull: %v", err)
	}
	first := calls
	if first == 0 {
		t.Fatal("first run should verify at least one candidate")
	}
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("second RunFull: %v", err)
	}
	if calls != first {
		t.Fatalf("second run over unchanged clusters must make ZERO new verify calls; got %d extra", calls-first)
	}
	// And no duplicate facet.
	facets, _ := s.ReadFacets()
	n := 0
	for _, f := range facets {
		if f.Key == "k" {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("re-run minted a duplicate facet: %d copies of key 'k'", n)
	}
}

// TestRunFullDoesNotAdvanceReviewRowid (watermark-conflation lock): the emergent pass
// must NEVER stamp the S2 per-lens review watermark — else the unclustered majority of
// obs would be marked "reviewed" and skipped by S2's sequential fold.
func TestRunFullDoesNotAdvanceReviewRowid(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})
	before := s.ReviewRowid("math")

	verify := func(_ context.Context, _, _, _ string) (string, error) {
		return `[{"dimension":"thinking","key":"k","value":"v","confidence":0.7,"because_of":["a0"],"contradicts_prior":false}]`, nil
	}
	r := emergentReviewer(s, verify)
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("RunFull: %v", err)
	}
	if after := s.ReviewRowid("math"); after != before {
		t.Fatalf("emergent pass must NOT advance review_rowid: was %d, now %d", before, after)
	}
}
