package distill

import (
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// Issue #173: confidence could only ever ratchet UP, because the reinforcement branch did
// max(current, returned) and threw away every downgrade the model proposed. The decay that
// types.go and the invalidation rule both document was unreachable.
//
// These pin the fix and, just as importantly, the reason max() existed — one window must not be
// able to rewrite a facet built on many.

func confFacet(conf float64) store.Facet {
	return store.Facet{
		Lens: "L", Dimension: "d", Key: "k", LastSeen: applyNow,
		Versions: []store.FacetVersion{{
			Value: "prefers Go", ValidFrom: applyNow, RecordedAt: applyNow,
			Confidence: conf, BecauseOf: []string{"o1"},
		}},
	}
}

// The bug itself. Measured on real facets: the model returned 0.80 against a stored 0.90, and
// max() kept 0.90 — so counter-evidence short of a sustained reversal had NO effect at all.
func TestAProposedDowngradeIsApplied(t *testing.T) {
	byKey := indexFacets([]store.Facet{confFacet(0.9)})
	if !(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.8, BecauseOf: []string{"o2"},
	}, applyNow) {
		t.Fatal("the assertion was rejected")
	}
	cur := byKey["L|d|k"].Current()
	if cur.Confidence != 0.8 {
		t.Errorf("a downgrade of 0.90 → 0.80 must be applied, got %v", cur.Confidence)
	}
	// Provenance still merges: the facet is still true, just less certain.
	if len(cur.BecauseOf) != 2 {
		t.Errorf("provenance should still merge on a downgrade, got %v", cur.BecauseOf)
	}
	// The value is untouched — a downgrade is not a change arc.
	if cur.Value != "prefers Go" || len(byKey["L|d|k"].Versions) != 1 {
		t.Error("a downgrade must not open a new version or alter the value")
	}
}

// Why max() was there, and the thing the fix must not break: one window with weak evidence
// cannot flatten a facet built on many.
func TestNoSingleFoldCanCollapseAWellSupportedFacet(t *testing.T) {
	byKey := indexFacets([]store.Facet{confFacet(0.95)})
	(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.0,
	}, applyNow)
	got := byKey["L|d|k"].Current().Confidence
	if got != 0.85 {
		t.Errorf("a 0.95 facet must fall by at most one step, got %v", got)
	}
}

// Symmetric: a single fold cannot catapult a weak facet to ceiling either. Confidence means
// "accumulated across folds", not "the highest any one window ever felt".
func TestNoSingleFoldCanCatapultAWeakFacet(t *testing.T) {
	byKey := indexFacets([]store.Facet{confFacet(0.2)})
	(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 1.0,
	}, applyNow)
	got := byKey["L|d|k"].Current().Confidence
	if got != 0.3 {
		t.Errorf("a 0.2 facet must rise by at most one step, got %v", got)
	}
}

// Repeated evidence is what moves a facet far — the cross-session reinforcement signal (#16),
// now actually requiring repetition in both directions.
func TestConfidenceConvergesOverFolds(t *testing.T) {
	byKey := indexFacets([]store.Facet{confFacet(0.9)})
	r := &Reviewer{}
	for i := 0; i < 5; i++ {
		r.applyFacet(byKey, "L", reviewedFacet{
			Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.4,
		}, applyNow)
	}
	if got := byKey["L|d|k"].Current().Confidence; got != 0.4 {
		t.Errorf("five downgrading folds should reach the target, got %v", got)
	}
	// And it lands exactly, never overshoots past the model's judgment.
	r.applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.4,
	}, applyNow)
	if got := byKey["L|d|k"].Current().Confidence; got != 0.4 {
		t.Errorf("a further fold at the same confidence must not move it, got %v", got)
	}
}

// A facet the model does NOT mention is left alone. "Decays on absence" is documented but
// deliberately still unimplemented — a fold asserts a couple of dozen facets out of thousands,
// so a per-fold decay of everything unmentioned would rot the profile in a few passes. Pinned so
// the current behaviour is a stated decision rather than an accident.
func TestAnUnmentionedFacetIsUntouched(t *testing.T) {
	// Two EXISTING facets, and the assertion lands on one of them — so the reinforcement path
	// actually runs while the other sits unmentioned. An earlier version of this test asserted
	// a facet that did not exist, which took the new-facet branch and never exercised the code
	// it was meant to pin (a mutation that decayed every other facet survived it).
	other := confFacet(0.5)
	other.Key = "other"
	byKey := indexFacets([]store.Facet{confFacet(0.9), other})

	(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.8,
	}, applyNow)

	if got := byKey["L|d|k"].Current().Confidence; got != 0.8 {
		t.Errorf("the asserted facet should have moved, got %v", got)
	}
	if got := byKey["L|d|other"].Current().Confidence; got != 0.5 {
		t.Errorf("an unmentioned facet must not move, got %v", got)
	}
}

func TestStepToward(t *testing.T) {
	cases := []struct{ from, to, want float64 }{
		{0.9, 0.8, 0.8},   // within a step: lands exactly
		{0.9, 0.0, 0.8},   // far below: one step down
		{0.2, 1.0, 0.3},   // far above: one step up
		{0.5, 0.5, 0.5},   // no movement
		{0.9, 0.95, 0.95}, // the existing reinforcement case
	}
	// stepToward is raw arithmetic; clampConf does the rounding, so compare through it — that
	// is the composition every write path actually uses.
	for _, c := range cases {
		if got := clampConf(stepToward(c.from, c.to, maxConfidenceStep)); got != c.want {
			t.Errorf("clampConf(stepToward(%v, %v)) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
	// And the rounding itself, which is what keeps float noise out of the database.
	if got := clampConf(0.30000000000000004); got != 0.3 {
		t.Errorf("clampConf should round step noise away, got %v", got)
	}
}
