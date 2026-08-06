package distill

import (
	"testing"

	"github.com/IngTian/witness/internal/store"
)

const applyNow = "2026-08-05T00:00:00Z"

func goodFacet() store.Facet {
	return store.Facet{
		Lens: "L", Dimension: "d", Key: "k", LastSeen: "2026-01-01T00:00:00Z",
		Versions: []store.FacetVersion{{
			Value: "prefers Go", ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z",
			Confidence: 0.9, BecauseOf: []string{"o1"},
		}},
	}
}

// A malformed assertion must never supersede a good facet version.
//
// applyFacet trusted the model's dimension/key/value. With contradicts_prior:true and an
// EMPTY value it took the change-arc branch: closed the good version (stamping ValidTo) and
// opened an empty one. The facet's CURRENT stance became "" — the real value demoted to
// history, the profile rendering a blank, and the review watermark already advanced past
// the observations that would have re-asserted it. Reproduced before the fix: 2 versions,
// current value "".
func TestApplyFacetRejectsAnEmptyValueInsteadOfSupersedingAGoodOne(t *testing.T) {
	byKey := indexFacets([]store.Facet{goodFacet()})
	rv := &Reviewer{}

	applied := rv.applyFacet(byKey, "L", reviewedFacet{
		Dimension: "d", Key: "k", Value: "", Confidence: 0.8, Contradicts: true,
	}, applyNow)
	if applied {
		t.Error("applyFacet reported success for an assertion with no value")
	}

	f := byKey["L|d|k"]
	if len(f.Versions) != 1 {
		t.Fatalf("an empty value opened a new version: %d versions", len(f.Versions))
	}
	if f.Versions[0].ValidTo != "" {
		t.Errorf("the good version was CLOSED (validTo=%q) by a valueless contradiction", f.Versions[0].ValidTo)
	}
	cur := f.Current()
	if cur == nil {
		t.Fatal("the facet lost its current version entirely")
	}
	if cur.Value != "prefers Go" {
		t.Errorf("current stance is now %q, want the real value to survive", cur.Value)
	}
}

// An empty dimension/key must not mint a junk facet. Its id would be "<lens>||", which no
// lens prompt can ever reinforce or supersede — so it lingers in L2 and in the profile
// input forever.
func TestApplyFacetRejectsAnUnidentifiableFacet(t *testing.T) {
	for _, tc := range []struct {
		name string
		rf   reviewedFacet
	}{
		{"wholly empty", reviewedFacet{}},
		{"no dimension", reviewedFacet{Key: "k", Value: "v"}},
		{"no key", reviewedFacet{Dimension: "d", Value: "v"}},
		{"whitespace only", reviewedFacet{Dimension: "  ", Key: "\t", Value: "\n"}},
		{"no value", reviewedFacet{Dimension: "d2", Key: "k2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			byKey := indexFacets([]store.Facet{goodFacet()})
			if rv := (&Reviewer{}); rv.applyFacet(byKey, "L", tc.rf, applyNow) {
				t.Error("applyFacet reported success for an unusable assertion")
			}
			if len(byKey) != 1 {
				for id := range byKey {
					t.Logf("  present: %q", id)
				}
				t.Fatalf("a junk facet was minted: %d facets, want only the pre-existing one", len(byKey))
			}
			if _, ok := byKey["L|d|k"]; !ok {
				t.Error("the pre-existing facet was replaced")
			}
		})
	}
}

// The guard must not block legitimate work: new facets, real contradictions, and
// reinforcement all still apply. Confidence 0 is deliberately allowed — it means "asserted
// but unsure", not "unasserted".
func TestApplyFacetStillAppliesWellFormedAssertions(t *testing.T) {
	t.Run("brand-new facet", func(t *testing.T) {
		byKey := map[string]*store.Facet{}
		if !(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
			Dimension: "d", Key: "k", Value: "v", Confidence: 0,
		}, applyNow) {
			t.Fatal("a well-formed new facet was rejected")
		}
		f := byKey["L|d|k"]
		if f == nil || f.Current() == nil || f.Current().Value != "v" {
			t.Fatalf("new facet not stored correctly: %+v", f)
		}
	})

	t.Run("real contradiction opens a change arc", func(t *testing.T) {
		byKey := indexFacets([]store.Facet{goodFacet()})
		if !(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
			Dimension: "d", Key: "k", Value: "prefers Rust", Confidence: 0.8, Contradicts: true,
		}, applyNow) {
			t.Fatal("a well-formed contradiction was rejected")
		}
		f := byKey["L|d|k"]
		if len(f.Versions) != 2 {
			t.Fatalf("want 2 versions (closed + new), got %d", len(f.Versions))
		}
		if f.Versions[0].ValidTo != applyNow {
			t.Errorf("the superseded version was not closed: %q", f.Versions[0].ValidTo)
		}
		if f.Current().Value != "prefers Rust" {
			t.Errorf("current = %q", f.Current().Value)
		}
	})

	t.Run("reaffirmation reinforces", func(t *testing.T) {
		byKey := indexFacets([]store.Facet{goodFacet()})
		if !(&Reviewer{}).applyFacet(byKey, "L", reviewedFacet{
			Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.95, BecauseOf: []string{"o2"},
		}, applyNow) {
			t.Fatal("a reaffirmation was rejected")
		}
		cur := byKey["L|d|k"].Current()
		if cur.Confidence != 0.95 {
			t.Errorf("confidence not raised: %v", cur.Confidence)
		}
		if len(cur.BecauseOf) != 2 {
			t.Errorf("provenance not merged: %v", cur.BecauseOf)
		}
	})
}
