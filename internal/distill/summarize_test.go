package distill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// Dirty-tracking (#73-S5): re-running Summarize with UNCHANGED facets must make ZERO
// LLM calls — every per-lens summary is signature-matched and reused, and the unified
// portrait is skipped because no lens changed. The prior files stay intact.
func TestSummarizerSkipsUnchanged(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	calls := 0
	fake := func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		return "OUT", nil
	}
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: fake}

	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("first Summarize: %v", err)
	}
	first := calls // 2 lenses + 1 unified = 3
	if first != 3 {
		t.Fatalf("first pass: want 3 calls (2 lenses + unified), got %d", first)
	}

	// Second pass, nothing changed → no calls at all.
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("second Summarize: %v", err)
	}
	if calls != first {
		t.Fatalf("unchanged re-run must make 0 new calls, made %d", calls-first)
	}
}

// A changed summarizer PROMPT (e.g. a witness upgrade ships a new prompts/summarize/
// lens.md) must invalidate every lens's signature and force a one-time regen, even
// with unchanged facets — the prompt is part of the signature (#73-S5).
func TestSummarizerRegeneratesWhenPromptChanges(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	calls := 0
	fake := func(_ context.Context, _, _, _ string) (string, error) { calls++; return "OUT", nil }

	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS-v1", UnifiedPrompt: "UNIFIED", Run: fake}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("first Summarize: %v", err)
	}
	calls = 0

	// Same facets, but a NEW summarize prompt → all lenses must regenerate.
	sm.LensPrompt = "LENS-v2"
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("second Summarize: %v", err)
	}
	// 2 lenses regenerate + unified = 3 calls; nothing was skipped.
	if calls != 3 {
		t.Fatalf("a changed prompt must regenerate all lenses (want 3 calls), got %d", calls)
	}
}

// A change to ONE lens's facets regenerates exactly that lens + the unified portrait
// (which depends on it) — not the other, unchanged lens (#73-S5).
func TestSummarizerRegeneratesOnlyChangedLens(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	var lensInputs []string
	unifiedCalls := 0
	fake := func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "UNIFIED" {
			unifiedCalls++
			return "PORTRAIT", nil
		}
		lensInputs = append(lensInputs, input)
		return "SUMMARY<" + input + ">", nil
	}
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: fake}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("first Summarize: %v", err)
	}
	lensInputs = nil
	unifiedCalls = 0

	// Change ONLY the math lens's facet value (WriteFacets is replace-all).
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9}}},
		{Lens: "math", Dimension: "resilience", Key: "trip_wire",
			Versions: []store.FacetVersion{{Value: "recovers with CALCULUS now", Confidence: 0.87}}},
	}); err != nil {
		t.Fatalf("mutate facets: %v", err)
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("second Summarize: %v", err)
	}

	// Exactly one per-lens call, and it must be for math (its new value present).
	if len(lensInputs) != 1 {
		t.Fatalf("want exactly 1 per-lens regen (math), got %d: %v", len(lensInputs), lensInputs)
	}
	if !strings.Contains(lensInputs[0], "CALCULUS") {
		t.Fatalf("the regenerated lens should be math (new value), got input: %q", lensInputs[0])
	}
	// The unchanged default lens's summary must still be present (reused, not lost).
	if md, ok, _ := s.ReadProfile("default"); !ok || !strings.Contains(md, "stops at good enough") {
		t.Fatalf("unchanged default summary should be reused intact, got ok=%v md=%q", ok, md)
	}
	// A lens changed → unified regenerates once.
	if unifiedCalls != 1 {
		t.Fatalf("a changed lens must regenerate the unified portrait once, got %d", unifiedCalls)
	}
}

// Self-heal (#73-S5): if a per-lens profile file is deleted but its signature still
// matches, Summarize must REGENERATE it (skip only when the prior file exists), not
// leave the profile permanently missing.
func TestSummarizerRegeneratesWhenProfileFileMissing(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	calls := 0
	fake := func(_ context.Context, _, _, _ string) (string, error) { calls++; return "OUT", nil }
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: fake}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("first Summarize: %v", err)
	}
	// Delete the default lens's profile file, leaving its signature stamped. (No store
	// delete method exists; remove the plain <lens>.md file under ProfileDir directly.)
	if err := os.Remove(filepath.Join(s.ProfileDir(), "default.md")); err != nil {
		t.Fatalf("delete profile file: %v", err)
	}
	calls = 0
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("second Summarize: %v", err)
	}
	// Must regenerate the deleted default (its file is gone) even though its sig matches;
	// the unchanged math lens stays skipped. Unified regenerates because a lens changed.
	if md, ok, _ := s.ReadProfile("default"); !ok || md != "OUT" {
		t.Fatalf("deleted profile should self-heal, got ok=%v md=%q", ok, md)
	}
	if calls == 0 {
		t.Fatalf("a missing profile file must force a regen despite a matching signature")
	}
}

// End-to-end per-lens model routing (#75): each per-lens summary must run on THAT
// lens's ReviewModel (via ModelFor); a facet whose lens isn't in the active set (an
// orphan) and the unified cross-lens pass both fall back to the default DistillModel.
func TestSummarizerUsesPerLensReviewModel(t *testing.T) {
	s := newStore(t)
	// Facets for three lenses: math (per-lens override), default (rides the default), and
	// "orphan" (no matching active lens → default fallback).
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
		Config: store.Config{TriageModel: "default-triage", DistillModel: "default-distill"},
		Lenses: []*lens.Lens{
			{Name: "math", ReviewModel: "math-strong"}, // per-lens override
			{Name: "default"},                          // rides the default
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
	if got := modelByLensInput["default"]; got != "default-distill" {
		t.Fatalf("default (no override) should use the default distill model, got %q", got)
	}
	if got := modelByLensInput["orphan"]; got != "default-distill" {
		t.Fatalf("an orphan lens (not in the active set) should fall back to default, got %q", got)
	}
	if unifiedModel != "default-distill" {
		t.Fatalf("the unified cross-lens pass has no single lens → default distill model, got %q", unifiedModel)
	}
}

// End-to-end per-lens RUNNER routing (#75 slice 2): a lens declaring its own runner has
// its summary run through THAT runner's SummarizeFunc; a lens with no runner (and the
// unified pass) go through the default Run. Proves the RunFor seam actually dispatches.
func TestSummarizerUsesPerLensRunner(t *testing.T) {
	s := newStore(t)
	if err := s.WriteFacets([]store.Facet{
		{Lens: "cheap", Dimension: "d", Key: "k", Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
		{Lens: "default", Dimension: "d", Key: "k", Versions: []store.FacetVersion{{Value: "v", Confidence: 0.9}}},
	}); err != nil {
		t.Fatalf("seed facets: %v", err)
	}

	runnerByLens := map[string]string{} // lens → which runner ran its summary
	var unifiedRunner string
	tagRun := func(runnerName string) SummarizeFunc {
		return func(_ context.Context, _, prompt, input string) (string, error) {
			if prompt == "UNIFIED" {
				unifiedRunner = runnerName
				return "# portrait\n", nil
			}
			for _, l := range []string{"cheap", "default"} {
				if strings.Contains(input, "LENS: "+l+"\n") {
					runnerByLens[l] = runnerName
				}
			}
			return "SUMMARY", nil
		}
	}
	defaultRun := tagRun("default")
	opencodeRun := tagRun("opencode")

	sm := &Summarizer{
		Store:  s,
		Config: store.Config{Runner: "claude", DistillModel: "d"},
		Lenses: []*lens.Lens{
			{Name: "cheap", Runner: "opencode"}, // routes to the opencode SummarizeFunc
			{Name: "default"},                   // rides the default
		},
		LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: defaultRun,
		RunFor: func(ln *lens.Lens) SummarizeFunc {
			if ln != nil && ln.Runner == "opencode" {
				return opencodeRun
			}
			return nil // fall back to Run
		},
	}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got := runnerByLens["cheap"]; got != "opencode" {
		t.Fatalf("a lens declaring runner=opencode must summarize via the opencode runner, got %q", got)
	}
	if got := runnerByLens["default"]; got != "default" {
		t.Fatalf("a lens with no runner must use the default runner, got %q", got)
	}
	if unifiedRunner != "default" {
		t.Fatalf("the unified cross-lens pass has no single lens → default runner, got %q", unifiedRunner)
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
