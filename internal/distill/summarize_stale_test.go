package distill

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	// Under the key the store's DeleteLensData clears, or the check below passes vacuously.
	if s.MetaString(store.ProfileSigKey("math")) == "" {
		t.Fatal("precondition: Summarize did not stamp profile_sig under store.ProfileSigKey")
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
	if sig := s.MetaString(store.ProfileSigKey("math")); sig != "" {
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

// portraitCaller is a fake runner that records which portrait each call was for, so a test can
// compare the set Stale() marks true against the set Summarize() actually calls the model for.
// Each summary echoes its input, so a changed facet changes the summary and therefore the
// unified portrait's input — the real-model behaviour the unified signature depends on.
func portraitCaller(called *[]string) SummarizeFunc {
	return func(_ context.Context, _, prompt, input string) (string, error) {
		if strings.HasPrefix(prompt, "UNIFIED") {
			*called = append(*called, store.ProfileUnified)
			return "PORTRAIT", nil
		}
		first, _, _ := strings.Cut(input, "\n")
		*called = append(*called, strings.TrimPrefix(first, "LENS: "))
		return "SUMMARY<" + input + ">", nil
	}
}

// Stale() is the "run `witness profile` to refresh" line in `witness view`. Its contract is not
// "something changed" but "a Summarize now would call the model for this portrait": a cruder
// test would keep showing the line after the user ran `witness profile`. So each phase asserts
// Stale(), then runs Summarize and checks it called the runner for EXACTLY the portraits Stale
// marked true — no more, no fewer.
func TestStaleMatchesSummarize(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s) // default + math
	var called []string
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED",
		Run: portraitCaller(&called)}

	phase := func(name string, want map[string]bool) {
		t.Helper()
		got, err := sm.Stale()
		if err != nil {
			t.Fatalf("%s: Stale: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: Stale() = %v, want %v", name, got, want)
		}
		var wantCalls []string
		for p, stale := range got {
			if stale {
				wantCalls = append(wantCalls, p)
			}
		}
		called = nil
		if err := sm.Summarize(context.Background()); err != nil {
			t.Fatalf("%s: Summarize: %v", name, err)
		}
		sort.Strings(wantCalls)
		sort.Strings(called)
		if !reflect.DeepEqual(called, wantCalls) {
			t.Fatalf("%s: Summarize called the runner for %v, Stale() said %v", name, called, wantCalls)
		}
	}

	phase("fresh archive", map[string]bool{"default": true, "math": true, store.ProfileUnified: true})
	phase("just summarized", map[string]bool{"default": false, "math": false, store.ProfileUnified: false})

	// Change ONE facet in math (WriteFacets is replace-all): math + unified, never default.
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9}}},
		{Lens: "math", Dimension: "resilience", Key: "trip_wire",
			Versions: []store.FacetVersion{{Value: "recovers with CALCULUS now", Confidence: 0.87}}},
	}); err != nil {
		t.Fatal(err)
	}
	phase("one lens changed", map[string]bool{"default": false, "math": true, store.ProfileUnified: true})
	phase("refreshed", map[string]bool{"default": false, "math": false, store.ProfileUnified: false})

	// A unified-only prompt change: every notebook is current, so only the unified signature
	// itself can see it. A "stale if any notebook is stale" shortcut would miss this.
	sm.UnifiedPrompt = "UNIFIED v2"
	phase("unified prompt changed", map[string]bool{"default": false, "math": false, store.ProfileUnified: true})
}

// profileSigs snapshots every profile_sig:* a Summarize could touch: each lens with facets,
// each portrait on disk, and the unified one.
func profileSigs(t *testing.T, s *store.Store) map[string]string {
	t.Helper()
	keys := map[string]bool{store.ProfileUnified: true}
	facets, err := s.ReadActiveFacets()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facets {
		keys[f.Lens] = true
	}
	onDisk, err := s.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range onDisk {
		keys[p] = true
	}
	sigs := map[string]string{}
	for k := range keys {
		sigs[k] = s.MetaString(store.ProfileSigKey(k))
	}
	return sigs
}

// Stale() runs inside `witness view`, which must write nothing but its HTML. So it may never
// reach a runner (Run and RunFor are nil here — a call would panic) and must skip Summarize's
// write side effects: the orphan reap and the no-lenses unified delete both leave files and
// signatures alone.
func TestStaleNeverRuns(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	var called []string
	if err := (&Summarizer{Store: s, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED",
		Run: portraitCaller(&called)}).Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	sm := &Summarizer{Store: s, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED"} // no Run, no RunFor

	// Drop math: its portrait is now an orphan Summarize would REAP, not regenerate — so it is
	// not stale — and the one-lens archive still has a unified portrait to rebuild.
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9}}},
	}); err != nil {
		t.Fatal(err)
	}
	before := profileSigs(t, s)
	got, err := sm.Stale()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"default": false, store.ProfileUnified: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("one lens left: Stale() = %v, want %v", got, want)
	}
	if after := profileSigs(t, s); !reflect.DeepEqual(before, after) {
		t.Errorf("Stale wrote signatures: before %v, after %v", before, after)
	}
	if _, ok, _ := s.ReadProfile("math"); !ok {
		t.Error("Stale reaped the orphan math portrait; only Summarize may")
	}

	// No facets at all: Summarize would delete the unified portrait without a model call.
	if err := s.WriteFacets(nil); err != nil {
		t.Fatal(err)
	}
	before = profileSigs(t, s)
	got, err = sm.Stale()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{store.ProfileUnified: false}; !reflect.DeepEqual(got, want) {
		t.Errorf("no facets: Stale() = %v, want %v", got, want)
	}
	if after := profileSigs(t, s); !reflect.DeepEqual(before, after) {
		t.Errorf("Stale wrote signatures: before %v, after %v", before, after)
	}
	if _, ok, _ := s.ReadProfile(store.ProfileUnified); !ok {
		t.Error("Stale deleted the unified portrait; only Summarize may")
	}
}

// A matching signature only vouches for a portrait that is still on disk and non-empty —
// Summarize regenerates a deleted or blank file — so Stale must report both.
func TestStaleMissingFileIsStale(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	var called []string
	sm := &Summarizer{Store: s, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: portraitCaller(&called)}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.ProfileDir(), "math.md")); err != nil {
		t.Fatal(err)
	}
	got, err := sm.Stale()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"default": false, "math": true, store.ProfileUnified: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("math.md deleted: Stale() = %v, want %v", got, want)
	}

	// A blank file counts as missing too (Summarize regenerates it, #148).
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteProfile("default", ""); err != nil {
		t.Fatal(err)
	}
	got, err = sm.Stale()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"default": true, "math": false, store.ProfileUnified: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("default.md blank: Stale() = %v, want %v", got, want)
	}
}

// Before #148 an empty runner reply was written to <lens>.md with its signature stamped, and
// the unified signature was stamped over a portrait holding that empty section. The unified
// signature over the on-disk priors then still MATCHES, yet Summarize regenerates the blank
// lens and then the portrait. Only the "unified is stale whenever a notebook is" rule catches
// this, so it is pinned here rather than left to the signature.
func TestStaleUnifiedOverALegacyEmptySection(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	var called []string
	sm := &Summarizer{Store: s, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: portraitCaller(&called)}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Reproduce the legacy state: default.md blank (its signature still stamped by the pass
	// above), and a unified signature over exactly the portrait such a pass would have built.
	if err := s.WriteProfile("default", ""); err != nil {
		t.Fatal(err)
	}
	mathPrior, ok, err := s.ReadProfile("math")
	if err != nil || !ok {
		t.Fatalf("precondition: math profile, ok=%v err=%v", ok, err)
	}
	var legacy strings.Builder
	writeSection(&legacy, "default", "")
	writeSection(&legacy, "math", mathPrior)
	sig := summarySignature(ModelFor(store.Config{}, nil, PhaseReview), "UNIFIED", legacy.String())
	if err := s.SetMetaString(store.ProfileSigKey(store.ProfileUnified), sig); err != nil {
		t.Fatal(err)
	}

	got, err := sm.Stale()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"default": true, "math": false, store.ProfileUnified: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("legacy empty section: Stale() = %v, want %v", got, want)
	}
	called = nil
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(called)
	if want := []string{"default", store.ProfileUnified}; !reflect.DeepEqual(called, want) {
		t.Errorf("Summarize called the runner for %v, want %v", called, want)
	}
}
