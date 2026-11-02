package distill

import (
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// A caller with no clock information must not ERASE a clock that is already there.
//
// The emergent (long-arc) pass calls applyFacet with seq 0 on purpose: it has no honest
// observation seq to stamp, and its comment explains that a NEW arc facet therefore starts
// without a decay clock. That reasoning covers new facets only. When a verified arc lands
// on an EXISTING facet, every branch of applyFacet wrote `ConfidenceSeq: seq` — so the
// facet's real clock was overwritten with 0, which DecayedConfidence reads as "never
// decays". One emergent pass froze every established facet it reinforced: the same bug
// #185 fixed for compaction, reached from a different caller.
//
// The rule: 0 means "unknown", and unknown never replaces known.

const establishedClock = 500

func clockedFacet(closed bool) store.Facet {
	f := goodFacet()
	f.Versions[0].ConfidenceSeq = establishedClock
	if closed {
		f.Versions[0].ValidTo = "2026-02-01T00:00:00Z"
	}
	return f
}

func TestApplyFacetWithNoClockKeepsTheKnownOne(t *testing.T) {
	cases := []struct {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byKey := indexFacets([]store.Facet{tc.facet})
			if !(&Reviewer{}).applyFacet(byKey, "L", tc.assert, applyNow, 0) {
				t.Fatal("the assertion was rejected, so nothing below is exercised")
			}
			cur := byKey["L|d|k"].Current()
			if cur == nil {
				t.Fatal("no current version")
			}
			if cur.ConfidenceSeq != establishedClock {
				t.Errorf("ConfidenceSeq = %d, want %d: a caller with no clock erased the facet's real one",
					cur.ConfidenceSeq, establishedClock)
			}
			// The outcome: one half-life on, the facet must have decayed. With the clock at 0 it
			// reads back exactly as stored, forever.
			later := int64(establishedClock) + int64(store.FacetDecayHalfLifeObs)
			if got := cur.DecayedConfidence(later); got >= cur.Confidence {
				t.Errorf("the facet stopped decaying: %.2f, one half-life later still %.2f", cur.Confidence, got)
			}
		})
	}
}

// The other half of the rule, so the fix cannot over-reach: a REAL seq still restarts the
// clock on every branch (that is what "confirmed now" means), and a brand-new facet asserted
// with no clock still starts without one, exactly as the emergent comment documents.
func TestApplyFacetClockStillRestartsOnARealSeq(t *testing.T) {
	byKey := indexFacets([]store.Facet{clockedFacet(false)})
	(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.8, BecauseOf: []string{"o2"},
	}, applyNow, 900)
	if got := byKey["L|d|k"].Current().ConfidenceSeq; got != 900 {
		t.Errorf("a review at seq 900 must restart the clock there, got %d", got)
	}

	fresh := map[string]*store.Facet{}
	(&Reviewer{}).applyFacet(fresh, "L", reviewedFacet{
		Dimension: "d", Key: "new", Value: "an arc", Confidence: 0.6, BecauseOf: []string{"o3"},
	}, applyNow, 0)
	if got := fresh["L|d|new"].Current().ConfidenceSeq; got != 0 {
		t.Errorf("a new facet with no clock information must start without a clock, got %d", got)
	}
}
