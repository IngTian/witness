package distill

import (
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// An assertion never moves a facet's decay clock backwards (knownClock = max(seq, prior)).

const establishedClock = 500

func clockedFacet(closed bool) store.Facet {
	f := goodFacet()
	f.Versions[0].ConfidenceSeq = establishedClock
	if closed {
		f.Versions[0].ValidTo = "2026-02-01T00:00:00Z"
	}
	return f
}

// The other half, so the rule cannot over-reach: a later seq restarts the clock there (that is
// what "confirmed now" means).
func TestApplyFacetClockStillRestartsOnARealSeq(t *testing.T) {
	byKey := indexFacets([]store.Facet{clockedFacet(false)})
	(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.8, BecauseOf: []string{"o2"},
	}, applyNow, 900, store.ContiguousClock(900))
	if got := byKey["L|d|k"].Current().ConfidenceSeq; got != 900 {
		t.Errorf("a review at seq 900 must restart the clock there, got %d", got)
	}
}

// A real seq LOWER than the facet's clock must not rewind it. An early window of a long fold
// re-reads observations older than the facet's last confirmation; stamping that seq made
// DecayedConfidence charge the facet for every observation since, decaying it far below what
// the archive actually believed.
func TestApplyFacetClockNeverMovesBackwards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		facet  store.Facet
		assert reviewedFacet
	}{
		{"reinforced (same value)", clockedFacet(false),
			reviewedFacet{Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.8, BecauseOf: []string{"o2"}}},
		{"contradicted (change arc)", clockedFacet(false),
			reviewedFacet{Dimension: "d", Key: "k", Value: "prefers Rust now", Confidence: 0.7, Contradicts: true, BecauseOf: []string{"o2"}}},
		{"reopened (every version closed)", clockedFacet(true),
			reviewedFacet{Dimension: "d", Key: "k", Value: "prefers Go again", Confidence: 0.7, BecauseOf: []string{"o2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			byKey := indexFacets([]store.Facet{tc.facet})
			if !(&Reviewer{}).applyFacet(byKey, "L", tc.assert, applyNow, establishedClock-400, store.ContiguousClock(establishedClock-400)) {
				t.Fatal("the assertion was rejected, so nothing below is exercised")
			}
			if got := byKey["L|d|k"].Current().ConfidenceSeq; got != establishedClock {
				t.Errorf("ConfidenceSeq = %d, want %d: an older window rewound the decay clock", got, establishedClock)
			}
		})
	}
}
