package distill

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// An EMPTY model reply must never replace a good summary, and must never be vouched for.
//
// This was a real defect (#148). The cross-lens skip checked only that a prior file EXISTED,
// while the per-lens skip also required it to be non-empty. So a runner returning "" — a refusal,
// a truncated reply, a provider hiccup — wrote a 0-byte profile/unified.md AND stamped its
// signature; every later pass matched the signature, saw the file exists, and returned. The
// portrait was permanently blank with no self-heal, and `witness profile` showed an empty
// document rather than the friendly "not generated yet".
//
// Read-time generation makes it worse: the read IS the only generation path, so there is no later
// background pass to recover.
//
// Two properties are pinned: the prior summary survives, and the failure is REPORTED (not
// silently skipped) so a model that keeps refusing is visible rather than looking like an archive
// with nothing to say.
func TestEmptySummaryNeverOverwritesOrIsVouchedFor(t *testing.T) {
	for _, empty := range []string{"", "   ", "\n\t\n"} {
		s := newStore(t)
		seedFacets(t, s) // default + math

		// Pass 1: a healthy runner establishes real summaries.
		good := func(_ context.Context, _, prompt, input string) (string, error) {
			if prompt == "UNIFIED" {
				return "GOOD PORTRAIT", nil
			}
			return "GOOD SUMMARY", nil
		}
		sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: good}
		if err := sm.Summarize(context.Background()); err != nil {
			t.Fatalf("precondition Summarize: %v", err)
		}
		if md, ok, _ := s.ReadProfile(store.ProfileUnified); !ok || md != "GOOD PORTRAIT" {
			t.Fatalf("precondition: portrait should exist, got ok=%v md=%q", ok, md)
		}

		// Pass 2: change the facets so the signature no longer matches (forcing real calls),
		// and answer the UNIFIED prompt with an empty reply.
		if err := s.WriteFacets([]store.Facet{
			{Lens: "default", Dimension: "traits", Key: "new",
				Versions: []store.FacetVersion{{Value: "something changed", Confidence: 0.9}}},
			{Lens: "math", Dimension: "traits", Key: "new2",
				Versions: []store.FacetVersion{{Value: "also changed", Confidence: 0.9}}},
		}); err != nil {
			t.Fatal(err)
		}
		sm.Run = func(_ context.Context, _, prompt, input string) (string, error) {
			if prompt == "UNIFIED" {
				return empty, nil
			}
			return "SUMMARY v2", nil
		}
		err := sm.Summarize(context.Background())
		if err == nil {
			t.Errorf("empty reply %q: Summarize must report the empty summary, not swallow it", empty)
		}

		// The PRIOR portrait survives untouched — the whole point.
		md, ok, _ := s.ReadProfile(store.ProfileUnified)
		if !ok || md != "GOOD PORTRAIT" {
			t.Errorf("empty reply %q: the prior portrait must survive, got ok=%v md=%q", empty, ok, md)
		}
		if strings.TrimSpace(md) == "" {
			t.Errorf("empty reply %q: a blank portrait was persisted", empty)
		}
	}
}

// The same guard on the PER-LENS path: an empty reply keeps the prior per-lens summary.
func TestEmptyPerLensSummaryKeepsThePriorOne(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED",
		Run: func(_ context.Context, _, prompt, _ string) (string, error) {
			if prompt == "UNIFIED" {
				return "PORTRAIT", nil
			}
			return "REAL SUMMARY", nil
		}}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// Force regeneration, and answer the per-lens prompt empty.
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "changed",
			Versions: []store.FacetVersion{{Value: "v2", Confidence: 0.9}}},
	}); err != nil {
		t.Fatal(err)
	}
	sm.Run = func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "UNIFIED" {
			return "PORTRAIT v2", nil
		}
		return "", nil
	}
	if err := sm.Summarize(context.Background()); err == nil {
		t.Error("an empty per-lens summary must be reported, not written")
	}
	if md, ok, _ := s.ReadProfile("default"); !ok || md != "REAL SUMMARY" {
		t.Errorf("the prior per-lens summary must survive an empty reply, got ok=%v md=%q", ok, md)
	}
}
