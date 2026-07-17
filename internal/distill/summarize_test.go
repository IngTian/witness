package distill

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

func seedFacets(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9}}},
		{Lens: "math", Dimension: "resilience", Key: "trip_wire",
			Versions: []store.FacetVersion{{Value: "recovers with arithmetic", Confidence: 0.87}}},
	}); err != nil {
		t.Fatalf("seed facets: %v", err)
	}
}

// The summarizer writes one markdown file per lens (from that lens's facets) plus
// a unified portrait synthesized from the per-lens summaries.
func TestSummarizerWritesPerLensAndUnified(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	lensCalls := 0
	var unifiedInput string
	fake := func(_ context.Context, _ /*model*/, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			unifiedInput = input
			return "# Whole Person\n", nil
		}
		lensCalls++
		return "SUMMARY<" + input + ">", nil
	}
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: fake}

	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if lensCalls != 2 {
		t.Fatalf("want a summary per lens (2), got %d", lensCalls)
	}
	for _, l := range []string{"default", "math"} {
		md, ok, _ := s.ReadProfile(l)
		if !ok || !strings.HasPrefix(md, "SUMMARY<") {
			t.Fatalf("%s summary missing/wrong: ok=%v md=%q", l, ok, md)
		}
	}
	umd, ok, _ := s.ReadProfile("unified")
	if !ok || umd != "# Whole Person\n" {
		t.Fatalf("unified summary missing/wrong: ok=%v md=%q", ok, umd)
	}
	// The unified pass sees the per-lens summaries (which echo the facet values).
	if !strings.Contains(unifiedInput, "recovers with arithmetic") || !strings.Contains(unifiedInput, "stops at good enough") {
		t.Fatalf("unified input should contain the per-lens summaries, got: %q", unifiedInput)
	}
}

// End-to-end per-lens model routing (#75): each per-lens summary must run on THAT
// lens's ReviewModel (via ModelFor); a facet whose lens isn't in the active set (an
// orphan) and the unified cross-lens pass both fall back to the global DistillModel.
func TestSummarizerUsesPerLensReviewModel(t *testing.T) {
	s := newStore(t)
	// Facets for three lenses: math (per-lens override), default (rides global), and
	// "orphan" (no matching active lens → global fallback).
	if err := s.WriteFacets([]store.Facet{
		{Lens: "math", Dimension: "d", Key: "k", Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
		{Lens: "default", Dimension: "d", Key: "k", Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
		{Lens: "orphan", Dimension: "d", Key: "k", Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
	}); err != nil {
		t.Fatalf("seed facets: %v", err)
	}

	modelByLensInput := map[string]string{} // lens value (echoed in input) -> model used
	var unifiedModel string
	fake := func(_ context.Context, model, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			unifiedModel = model
			return "# portrait\n", nil
		}
		// renderFacetsForSummary starts with "LENS: <name>", so recover which lens this is.
		for _, l := range []string{"math", "default", "orphan"} {
			if strings.Contains(input, "LENS: "+l+"\n") {
				modelByLensInput[l] = model
			}
		}
		return "SUMMARY", nil
	}
	sm := &Summarizer{
		Store:  s,
		Config: store.Config{TriageModel: "global-triage", DistillModel: "global-distill"},
		Lenses: []*lens.Lens{
			{Name: "math", ReviewModel: "math-strong"}, // per-lens override
			{Name: "default"},                          // rides the global
			// note: "orphan" is intentionally NOT in the active set
		},
		LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: fake,
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got := modelByLensInput["math"]; got != "math-strong" {
		t.Fatalf("math summary should use its per-lens review model, got %q", got)
	}
	if got := modelByLensInput["default"]; got != "global-distill" {
		t.Fatalf("default (no override) should use the global distill model, got %q", got)
	}
	if got := modelByLensInput["orphan"]; got != "global-distill" {
		t.Fatalf("an orphan lens (not in the active set) should fall back to global, got %q", got)
	}
	if unifiedModel != "global-distill" {
		t.Fatalf("the unified cross-lens pass has no single lens → global distill model, got %q", unifiedModel)
	}
}

// A failed claude -p during regeneration must leave the prior summary intact —
// the profile is derived, so a stale summary is fine until the next review.
func TestSummarizerFailureLeavesPriorFiles(t *testing.T) {
	s := newStore(t)
	if err := s.WriteProfile("default", "OLD"); err != nil {
		t.Fatal(err)
	}
	seedFacets(t, s)
	fail := func(_ context.Context, _, _, _ string) (string, error) {
		return "", fmt.Errorf("simulated summarize failure")
	}
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "L", UnifiedPrompt: "U", Run: fail}

	if err := sm.Summarize(context.Background()); err == nil {
		t.Fatal("expected an error when claude -p fails")
	}
	if md, ok, _ := s.ReadProfile("default"); !ok || md != "OLD" {
		t.Fatalf("prior summary must survive a failed regen: ok=%v md=%q", ok, md)
	}
}
