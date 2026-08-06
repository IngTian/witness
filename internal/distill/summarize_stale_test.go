package distill

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// facetFor builds one current-version facet for a lens.
func facetFor(lensName, value string) store.Facet {
	return store.Facet{
		Lens: lensName, Dimension: "d", Key: "k", LastSeen: "2026-01-01T00:00:00Z",
		Versions: []store.FacetVersion{{
			Value: value, ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z",
			Confidence: 0.9,
		}},
	}
}

func summarizerFor(s *store.Store, lensNames []string, run SummarizeFunc) *Summarizer {
	lns := make([]*lens.Lens, 0, len(lensNames))
	for _, n := range lensNames {
		lns = append(lns, &lens.Lens{Name: n})
	}
	return &Summarizer{
		Store: s, Lenses: lns, Config: store.Config{},
		LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: run,
	}
}

// A lens whose facets were dropped must not keep serving its old narrative.
//
// Summarize iterates only lenses that HAVE facets, so a lens dropped by `lens backfill
// --fresh` / `lens deregister` was never visited and its profile/<lens>.md stayed on disk
// forever. `witness profile <lens>` and the MCP get_profile tool read that file directly, so
// an agent was served a narrative built from facets that no longer exist — with nothing
// marking it stale. Reproduced before the fix: the old summary was still readable, byte for
// byte, after the facets were gone.
func TestSummarizeRemovesTheProfileOfALensThatLostItsFacets(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		facetFor("math", "loves topology"),
		facetFor("code", "writes dense comments"),
	}); err != nil {
		t.Fatal(err)
	}
	sm := summarizerFor(s, []string{"math", "code"}, func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "LENS: math") {
			return "MATH SUMMARY", nil
		}
		if strings.Contains(input, "LENS: code") {
			return "CODE SUMMARY", nil
		}
		return "UNIFIED PORTRAIT", nil
	})
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if md, ok, _ := s.ReadProfile("math"); !ok || md != "MATH SUMMARY" {
		t.Fatalf("precondition: math profile should exist, got ok=%v md=%q", ok, md)
	}

	// Drop ONLY math's derived data, exactly as `lens backfill --fresh` does.
	if _, _, err := s.DeleteLensData("math"); err != nil {
		t.Fatal(err)
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}

	// math's stale narrative must be GONE, so readers show "not generated yet".
	if md, ok, _ := s.ReadProfile("math"); ok {
		t.Errorf("the stale math profile survived and is still served to agents: %q", md)
	}
	// And its signature must not vouch for the deleted file.
	if sig := s.MetaString("profile_sig:math"); sig != "" {
		t.Errorf("profile_sig:math still vouches for a deleted profile: %q", sig)
	}
	// The lens that KEPT its facets must be untouched.
	if md, ok, _ := s.ReadProfile("code"); !ok || md != "CODE SUMMARY" {
		t.Errorf("a live lens's profile was collateral damage: ok=%v md=%q", ok, md)
	}
}

// Dropping the LAST lens's facets must also clear the cross-lens portrait.
//
// With zero facets Summarize returns early, BEFORE the <2-lens unified cleanup — so the
// unified portrait describing lenses that no longer have any facets was stranded on disk.
func TestSummarizeClearsTheUnifiedPortraitWhenNoFacetsRemain(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		facetFor("math", "loves topology"),
		facetFor("code", "writes dense comments"),
	}); err != nil {
		t.Fatal(err)
	}
	sm := summarizerFor(s, []string{"math", "code"}, func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "UNIFIED" {
			return "UNIFIED PORTRAIT", nil
		}
		return "A SUMMARY", nil
	})
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadProfile(store.ProfileUnified); !ok {
		t.Fatal("precondition: a unified portrait should exist with 2 lenses")
	}

	// Drop EVERYTHING.
	for _, l := range []string{"math", "code"} {
		if _, _, err := s.DeleteLensData(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if md, ok, _ := s.ReadProfile(store.ProfileUnified); ok {
		t.Errorf("the unified portrait survived with zero facets anywhere: %q", md)
	}
	for _, l := range []string{"math", "code"} {
		if md, ok, _ := s.ReadProfile(l); ok {
			t.Errorf("%s profile survived: %q", l, md)
		}
	}
}

// The reap must NOT delete a profile for a lens that still has facets but was skipped by the
// signature fast path — that file is the reused summary the portrait depends on.
func TestSummarizeKeepsProfilesReusedBySignatureSkip(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		facetFor("math", "loves topology"),
		facetFor("code", "writes dense comments"),
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	sm := summarizerFor(s, []string{"math", "code"}, func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		return "SUMMARY", nil
	})
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := calls
	if first == 0 {
		t.Fatal("fixture did not summarize anything")
	}
	// Second pass with NOTHING changed: every lens hits the signature skip.
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != first {
		t.Errorf("an unchanged pass cost %d extra LLM calls; the dirty-tracking regressed", calls-first)
	}
	for _, l := range []string{"math", "code", store.ProfileUnified} {
		if _, ok, _ := s.ReadProfile(l); !ok {
			t.Errorf("%s profile was reaped even though its lens still has facets", l)
		}
	}
}

// A facet whose only version is EXPIRED counts as "no facets" for that lens (Summarize
// already skips it), so its profile must be reaped too — otherwise a lens that went quiet
// keeps serving a narrative it no longer supports.
func TestSummarizeReapsALensWhoseOnlyFacetVersionExpired(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		facetFor("math", "loves topology"),
		facetFor("code", "writes dense comments"),
	}); err != nil {
		t.Fatal(err)
	}
	sm := summarizerFor(s, []string{"math", "code"}, func(_ context.Context, _, _, _ string) (string, error) {
		return "SUMMARY", nil
	})
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Close math's only version (valid_to set) — Current() now returns nil for it.
	expired := facetFor("math", "loves topology")
	expired.Versions[0].ValidTo = "2026-06-01T00:00:00Z"
	if err := s.WriteFacets([]store.Facet{expired, facetFor("code", "writes dense comments")}); err != nil {
		t.Fatal(err)
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if md, ok, _ := s.ReadProfile("math"); ok {
		t.Errorf("a lens with only an expired facet version kept its profile: %q", md)
	}
	if _, ok, _ := s.ReadProfile("code"); !ok {
		t.Error("the still-active lens lost its profile")
	}
}

// ListProfiles underpins the reap: it must enumerate what is on disk, include the unified
// portrait, tolerate a missing dir, and never hand back a name that could escape the dir.
func TestListProfilesEnumeratesWhatIsOnDisk(t *testing.T) {
	s := newStore(t)
	// No profile dir yet: empty, not an error.
	got, err := s.ListProfiles()
	if err != nil {
		t.Fatalf("a missing profile dir must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}

	for _, l := range []string{"math", "code", store.ProfileUnified} {
		if err := s.WriteProfile(l, "x"); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"math": true, "code": true, store.ProfileUnified: true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %q", g)
		}
	}
	// Sorted, so the reap order is deterministic.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("not sorted: %v", got)
		}
	}
	// A deleted profile drops out.
	if err := s.DeleteProfile("math"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListProfiles()
	for _, g := range got {
		if g == "math" {
			t.Error("a deleted profile is still enumerated")
		}
	}
}
