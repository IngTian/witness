package distill

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// One failing lens must not starve the others, or the cross-lens portrait (#148).
//
// Summarize used to `return` on the first per-lens failure, so a single lens with a typo'd model,
// an exhausted provider, or a broken runner meant every LATER lens was never attempted and the
// portrait was never built — with the only trace a swallowed Warn from the caller. Lenses are
// iterated in sorted order, so which lenses were starved depended on alphabetical luck.
//
// Asserted here with the failure on the FIRST lens ("aaa"), so a regression to fail-fast cannot
// pass by accident: under the old behavior nothing after it ran at all.
func TestOneFailingLensDoesNotStarveTheOthers(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		facetFor("aaa", "fails to summarize"),
		facetFor("mmm", "summarizes fine"),
		facetFor("zzz", "also fine"),
	}); err != nil {
		t.Fatal(err)
	}

	var called []string
	sm := summarizerFor(s, []string{"aaa", "mmm", "zzz"}, func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			called = append(called, "UNIFIED")
			return "PORTRAIT", nil
		}
		switch {
		case strings.Contains(input, "LENS: aaa"):
			called = append(called, "aaa")
			return "", fmt.Errorf("simulated bad model for aaa")
		case strings.Contains(input, "LENS: mmm"):
			called = append(called, "mmm")
			return "MMM SUMMARY", nil
		case strings.Contains(input, "LENS: zzz"):
			called = append(called, "zzz")
			return "ZZZ SUMMARY", nil
		}
		return "OTHER", nil
	})

	err := sm.Summarize(context.Background())
	if err == nil {
		t.Error("the failure must still be REPORTED so a persistently broken model stays visible")
	} else if !strings.Contains(err.Error(), "aaa") {
		t.Errorf("the reported error should name the failing lens, got %v", err)
	}

	// The other two were attempted AND persisted.
	for _, want := range []string{"mmm", "zzz"} {
		if md, ok, _ := s.ReadProfile(want); !ok || md == "" {
			t.Errorf("lens %s was starved by the aaa failure (ok=%v md=%q); its summary must be "+
				"written regardless of an unrelated lens failing", want, ok, md)
		}
	}
	// And the portrait was still built.
	if !contains(called, "UNIFIED") {
		t.Errorf("the cross-lens portrait was never attempted; calls=%v", called)
	}
	if md, ok, _ := s.ReadProfile(store.ProfileUnified); !ok || md != "PORTRAIT" {
		t.Errorf("portrait missing after a per-lens failure, got ok=%v md=%q", ok, md)
	}
	// The failing lens wrote nothing.
	if md, ok, _ := s.ReadProfile("aaa"); ok && md != "" {
		t.Errorf("the failing lens must not have a summary written, got %q", md)
	}
}

// A portrait containing a FALLBACK section must not be stamped as current.
//
// When a lens fails but has a prior summary, that summary is reused for the portrait so the section
// is not silently missing. The portrait is then correct to SERVE and wrong to VOUCH FOR: stamping
// its signature would make the next pass match and skip the rebuild, freezing the stale section in
// place until the facets happened to change again. Leaving it unstamped costs one regeneration and
// self-heals on the next read.
func TestAPortraitBuiltFromAStaleSectionIsNotStamped(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		facetFor("aaa", "will fail later"),
		facetFor("mmm", "healthy"),
	}); err != nil {
		t.Fatal(err)
	}

	// Pass 1: everything healthy, so both lenses and the portrait are established.
	sm := summarizerFor(s, []string{"aaa", "mmm"}, func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			return "PORTRAIT v1", nil
		}
		return "SUMMARY v1", nil
	})
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// Pass 2: change the facets (so signatures no longer match) and fail lens aaa.
	if err := s.WriteFacets([]store.Facet{
		facetFor("aaa", "changed, and now failing"),
		facetFor("mmm", "changed, healthy"),
	}); err != nil {
		t.Fatal(err)
	}
	var lastUnifiedInput string
	sm.Run = func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			lastUnifiedInput = input
			return "PORTRAIT v2", nil
		}
		if strings.Contains(input, "LENS: aaa") {
			return "", fmt.Errorf("simulated failure")
		}
		return "SUMMARY v2", nil
	}
	if err := sm.Summarize(context.Background()); err == nil {
		t.Error("the aaa failure must be reported")
	}

	// The portrait was rebuilt and INCLUDES the stale aaa section rather than dropping it.
	md, ok, _ := s.ReadProfile(store.ProfileUnified)
	if !ok || md != "PORTRAIT v2" {
		t.Fatalf("portrait should have been rebuilt, got ok=%v md=%q", ok, md)
	}
	// The aaa section must be PRESENT (its prior summary), not silently missing.
	if !strings.Contains(lastUnifiedInput, "aaa") {
		t.Errorf("the failing lens must still appear in the portrait input via its prior summary; "+
			"a silently missing section would be stamped as current. input=%q", lastUnifiedInput)
	}
	if !strings.Contains(lastUnifiedInput, "SUMMARY v1") {
		t.Errorf("the fallback should reuse the PRIOR summary text, got input=%q", lastUnifiedInput)
	}

	// THE PROPERTY THAT MATTERS: a third pass with aaa now HEALTHY must rebuild the portrait, so
	// the stale section self-heals. Asserting the signature is empty would be wrong — a leftover
	// signature from pass 1 is still stored, and cannot match the new portrait text anyway. What
	// must hold is behavioral: the next pass is not skipped.
	rebuilt := false
	sm.Run = func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			rebuilt = true
			lastUnifiedInput = input
			return "PORTRAIT v3", nil
		}
		return "SUMMARY v3", nil
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("third pass (all healthy) should succeed: %v", err)
	}
	if !rebuilt {
		t.Error("the portrait was SKIPPED on the pass after a fallback; the stale section would " +
			"then persist until the facets changed again, which is the freeze this guard prevents")
	}
	if !strings.Contains(lastUnifiedInput, "SUMMARY v3") {
		t.Errorf("the healed portrait should be built from the fresh summaries, got %q", lastUnifiedInput)
	}
}

// (containsStr used to live here — my own duplicate of `contains` in lens_parity_test.go, the same
// eight-line scan in the same package. Removed; the call site uses the existing helper.)
