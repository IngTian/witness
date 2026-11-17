package distill

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
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

// appendLensObs appends one observation per lens name, in ONE call so their seqs are dense.
func appendLensObs(t *testing.T, s *store.Store, prefix string, lenses ...string) {
	t.Helper()
	obs := make([]store.Observation, len(lenses))
	for i, l := range lenses {
		id := prefix + strconv.Itoa(i)
		obs[i] = store.Observation{ID: id, Lens: l, Observation: "seen " + id}
	}
	if err := s.AppendObservations(obs); err != nil {
		t.Fatal(err)
	}
}

// mathArchive is seqs 1-4501 with math at 1, 4, 7, ..., 4501: a third of the archive. A math
// facet at 0.90 stamped at seq 1 reads 0.64 on its own clock (1,500 math observations since),
// 0.45 on default's (3,000), 0.32 on the global one (4,500), and 0.90 on none.
func mathArchive() []string {
	return append([]string{"math"}, slices.Repeat([]string{"default", "default", "math"}, 1500)...)
}

// fakeClocks give that math facet the same readings as mathArchive: math 0.64, default 0.45,
// and a missing clock 0.90.
func fakeClocks() map[string]store.LensClock {
	return map[string]store.LensClock{"math": store.ContiguousClock(1501), "default": store.ContiguousClock(3001)}
}

// A lens's portrait is written from confidences decayed on that lens's own observations, not
// on the archive's, of which math holds a third.
func TestThePortraitDecaysEachLensOnItsOwnClock(t *testing.T) {
	s := newStore(t)
	appendLensObs(t, s, "a", mathArchive()...)
	f := clockedAt("traits", "satisfices", "stops at good enough", 1)
	f.Lens = "math"
	if err := s.WriteFacets([]store.Facet{f}); err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(lensInputs[0], "(0.64)") {
		t.Errorf("the math portrait must read 0.64, decayed on math's own observations:\n%s", lensInputs[0])
	}
	for _, wrong := range []string{"(0.45)", "(0.32)", "(0.90)"} {
		if strings.Contains(lensInputs[0], wrong) {
			t.Errorf("the math portrait reads %s (another lens's clock, the global one, or none):\n%s", wrong, lensInputs[0])
		}
	}
}

// Compaction shows the model, and logs as the From of its write, each facet's confidence on its
// own lens's clock.
func TestCompactionDecaysEachLensOnItsOwnClock(t *testing.T) {
	var facets []store.Facet
	for _, k := range []string{"a", "b", "c"} {
		f := clockedAt("thinking", k, "value "+k, 1)
		f.Lens = "math"
		facets = append(facets, f)
	}
	st := &fakeCompactStore{facets: facets, clocks: fakeClocks()}
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"a","value":"x","absorb":["b","c"],"confidence":0.5,"why":"same"}]`)
	c.Lenses = []*lens.Lens{{Name: "math", Review: "REVIEW"}}
	var stage1 string
	inner := c.Runner
	c.Runner = func(ctx context.Context, model, prompt, input string) (string, error) {
		if !strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			stage1 = input
		}
		return inner(ctx, model, prompt, input)
	}
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(context.Background(), plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stage1, "confidence: 0.64") || strings.Contains(stage1, "confidence: 0.45") || strings.Contains(stage1, "confidence: 0.90") {
		t.Errorf("stage 1 must show math's own-clock 0.64, not default's 0.45 or the stored 0.90:\n%s", stage1)
	}
	if len(st.events) != 1 || st.events[0].From != 0.64 {
		t.Errorf("the merge must log From 0.64, got %+v", st.events)
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
		clocks: map[string]store.LensClock{"default": store.ContiguousClock(confirmedAt + int64(store.FacetDecayHalfLifeObs))},
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
	// Stage 2 is shown no confidence at all: it has no use for one and copied it into values
	// (TestStageTwoSeesOnlyKeysAndValues). A stored 0.90 reaching it would be worse still.
	if strings.Contains(stage2, "0.45") || strings.Contains(stage2, "0.90") {
		t.Errorf("stage 2 (writing) must be shown no confidence, decayed or stored:\n%s", stage2)
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
	clock, err := s.LensClock("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFacets([]store.Facet{clockedAt("traits", "satisfices", "stops at good enough", clock[len(clock)-1])}); err != nil {
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
