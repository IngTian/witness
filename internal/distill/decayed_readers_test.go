package distill

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// Decay is derived on READ (#173), so it exists only where a reader derives it. Until this
// change exactly one reader did — the review — and every other surface showed the confidence
// stored at the facet's last confirmation, which is a record of the past, not the archive's
// current belief. These pin the two distill-side readers: what compaction shows the model, and
// what the portrait is written from.

// clockedAt is an active facet confirmed at observation seq `at` with stored confidence 0.90.
func clockedAt(dim, key, value string, at int64) store.Facet {
	return store.Facet{
		Lens: "default", Dimension: dim, Key: key, LastSeen: "2026-10-01T00:00:00Z",
		Versions: []store.FacetVersion{{
			Value: value, Confidence: 0.9, ConfidenceSeq: at,
			RecordedAt: "2026-10-01T00:00:00Z", ValidFrom: "2026-10-01T00:00:00Z",
			BecauseOf: []string{"obs-" + key},
		}},
	}
}

// One half-life after a 0.90 facet was last confirmed, it reads 0.45. Shown 0.90 instead,
// compaction's model treats a long-uncited facet as the strongest claim in its group, returns
// about that for the survivor, and Apply gives the survivor the freshest member's clock: a
// stale confidence comes out fresh.
func TestCompactionShowsTheModelDecayedConfidence(t *testing.T) {
	const confirmedAt = 100
	st := &fakeCompactStore{
		facets: []store.Facet{
			clockedAt("thinking", "a", "value a", confirmedAt),
			clockedAt("thinking", "b", "value b", confirmedAt),
			clockedAt("thinking", "c", "value c", confirmedAt),
		},
		highWater: confirmedAt + int64(store.FacetDecayHalfLifeObs),
	}
	var stage1, stage2 string
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"a","value":"x","absorb":["b","c"],"confidence":0.5,"why":"same"}]`)
	inner := c.Runner
	c.Runner = func(ctx context.Context, model, prompt, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			stage2 = input
		} else {
			stage1 = input
		}
		return inner(ctx, model, prompt, input)
	}
	if _, err := c.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stage1 == "" || stage2 == "" {
		t.Fatalf("both stages must run for this test to mean anything (stage1=%d chars, stage2=%d)", len(stage1), len(stage2))
	}
	if !strings.Contains(stage1, "confidence: 0.45") || strings.Contains(stage1, "confidence: 0.90") {
		t.Errorf("stage 1 (grouping) must show the decayed 0.45, not the stored 0.90:\n%s", stage1)
	}
	if !strings.Contains(stage2, "(confidence 0.45") || strings.Contains(stage2, "(confidence 0.90") {
		t.Errorf("stage 2 (writing) must show the decayed 0.45, not the stored 0.90:\n%s", stage2)
	}
}

// The portrait is written from the decayed values — and, the half that matters for cost, that
// does NOT make it regenerate as observations accrue. The change signature is taken over the
// stored rendering; decayed values drift a hundredth every few dozen observations, so hashing
// them would rebuild every portrait (a model call per lens) after almost every session.
func TestPortraitReadsDecayedConfidenceWithoutRegeneratingOnDrift(t *testing.T) {
	s := newStore(t)
	appendObs := func(n, from int) {
		t.Helper()
		obs := make([]store.Observation, n)
		for i := range obs {
			obs[i] = store.Observation{ID: fmt.Sprintf("o%d", from+i), Lens: "default", Observation: fmt.Sprintf("seen %d", from+i)}
		}
		if err := s.AppendObservations(obs); err != nil {
			t.Fatal(err)
		}
	}
	// One observation, then the facet is confirmed at it; then exactly one half-life more.
	appendObs(1, 0)
	hw0, err := s.ObservationHighWater()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFacets([]store.Facet{clockedAt("traits", "satisfices", "stops at good enough", hw0)}); err != nil {
		t.Fatal(err)
	}
	appendObs(int(store.FacetDecayHalfLifeObs), 1)

	var lensInputs []string
	run := func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "LENS" {
			lensInputs = append(lensInputs, input)
		}
		return "summary", nil
	}
	sm := &Summarizer{Store: s, Config: store.Config{}, LensPrompt: "LENS", UnifiedPrompt: "UNIFIED", Run: run}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lensInputs) != 1 {
		t.Fatalf("want one per-lens summary call, got %d", len(lensInputs))
	}
	if !strings.Contains(lensInputs[0], "(0.45)") || strings.Contains(lensInputs[0], "(0.90)") {
		t.Errorf("the portrait must be written from the decayed 0.45, not the stored 0.90:\n%s", lensInputs[0])
	}

	// More observations, no facet change: the decayed value moves (0.45 -> 0.44), the facet does
	// not, so the portrait must be reused rather than paid for again.
	appendObs(100, 1+int(store.FacetDecayHalfLifeObs))
	lensInputs = nil
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lensInputs) != 0 {
		t.Errorf("decay drift alone regenerated the portrait (%d model calls); the signature must "+
			"ignore it, or every session pays a call per lens", len(lensInputs))
	}
}
