package store

import (
	"reflect"
	"testing"
)

// When both archives have the same key with diverged histories and the incoming copy wins, our
// facet is renamed to <key>__mN. The confidence log is keyed by name, so a rename that left the
// rows behind would hand our history to the incoming facet now holding the key. Our log must
// follow our facet to its new key.
func TestMergeRenameCarriesConfidenceLog(t *testing.T) {
	t.Run("incoming newer", func(t *testing.T) {
		base := newArch(t)
		base.obs("ob1", "s1", "default", "2026-02-01T00:00:00Z")
		baseFacet := Facet{Lens: "default", Dimension: "thinking", Key: "k", LastSeen: "2026-02-01T00:00:00Z",
			Versions: []FacetVersion{{Value: "February view", Confidence: 0.9, ConfidenceSeq: 1,
				ValidFrom: "2026-02-01T00:00:00Z", RecordedAt: "2026-02-01T00:00:00Z", BecauseOf: []string{"ob1"}}}}
		// Seed an unrelated key that must not move
		otherFacet := mf("thinking", "other", "unrelated", 0.8, 1, "ob1")
		otherFacet.LastSeen = "2026-02-01T00:00:00Z"
		// Seed a facet with the same key "k" but in a DIFFERENT dimension (to catch missing dimension in WHERE)
		behaviorFacet := mf("behavior", "k", "different dimension", 0.7, 1, "ob1")
		behaviorFacet.LastSeen = "2026-02-01T00:00:00Z"
		// Write all facets and events in one call (WriteFacetsLogged replaces the whole table)
		baseEvents := []ConfidenceEvent{
			{Lens: "default", Dimension: "thinking", Key: "k",
				At: "2026-02-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.9, Kind: EventNew},
			{Lens: "default", Dimension: "thinking", Key: "other",
				At: "2026-02-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.8, Kind: EventNew},
			{Lens: "default", Dimension: "behavior", Key: "k",
				At: "2026-02-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.7, Kind: EventNew},
		}
		if err := base.st.WriteFacetsLogged([]Facet{baseFacet, otherFacet, behaviorFacet}, baseEvents); err != nil {
			t.Fatal(err)
		}

		inc := newArch(t)
		inc.obs("oi1", "s9", "default", "2026-08-01T00:00:00Z")
		incFacet := Facet{Lens: "default", Dimension: "thinking", Key: "k", LastSeen: "2026-08-01T00:00:00Z",
			Versions: []FacetVersion{{Value: "August view", Confidence: 0.7, ConfidenceSeq: 1,
				ValidFrom: "2026-08-01T00:00:00Z", RecordedAt: "2026-08-01T00:00:00Z", BecauseOf: []string{"oi1"}}}}
		inc.facets(incFacet)

		rep := base.merge(inc.export(), true)
		if len(rep.Facets.SameKey) != 1 || !rep.Facets.SameKey[0].IncomingWon {
			t.Fatalf("want one same-key collision won by incoming, got %+v", rep.Facets.SameKey)
		}

		events, err := base.st.ReadConfidenceLog()
		if err != nil {
			t.Fatalf("ReadConfidenceLog: %v", err)
		}

		// Our thinking/k row must be under "k__m1"
		var kEvents, otherEvs, behaviorEvs []ConfidenceEvent
		for _, ev := range events {
			if ev.Dimension == "thinking" && ev.Key == "k__m1" {
				kEvents = append(kEvents, ev)
			} else if ev.Dimension == "thinking" && ev.Key == "other" {
				otherEvs = append(otherEvs, ev)
			} else if ev.Dimension == "behavior" && ev.Key == "k" {
				behaviorEvs = append(behaviorEvs, ev)
			} else if ev.Dimension == "thinking" && ev.Key == "k" {
				t.Errorf("found thinking/k log row still under 'k' after rename: %+v", ev)
			}
		}
		if len(kEvents) != 1 {
			t.Errorf("want 1 event under thinking/k__m1, got %d: %+v", len(kEvents), kEvents)
		} else {
			want := ConfidenceEvent{Lens: "default", Dimension: "thinking", Key: "k__m1",
				At: "2026-02-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.9, Kind: EventNew}
			if !reflect.DeepEqual(kEvents[0], want) {
				t.Errorf("k__m1 event mismatch:\n got %+v\nwant %+v", kEvents[0], want)
			}
		}
		// The unrelated rows must be unchanged
		if len(otherEvs) != 1 {
			t.Errorf("want 1 event under thinking/other, got %d: %+v", len(otherEvs), otherEvs)
		}
		if len(behaviorEvs) != 1 {
			t.Errorf("want 1 event under behavior/k (unchanged), got %d: %+v", len(behaviorEvs), behaviorEvs)
		}
	})

	t.Run("ours newer", func(t *testing.T) {
		base := newArch(t)
		base.obs("ob1", "s1", "default", "2026-08-01T00:00:00Z")
		baseFacet := Facet{Lens: "default", Dimension: "thinking", Key: "k", LastSeen: "2026-08-01T00:00:00Z",
			Versions: []FacetVersion{{Value: "August view", Confidence: 0.9, ConfidenceSeq: 1,
				ValidFrom: "2026-08-01T00:00:00Z", RecordedAt: "2026-08-01T00:00:00Z", BecauseOf: []string{"ob1"}}}}
		baseEvents := []ConfidenceEvent{
			{Lens: "default", Dimension: "thinking", Key: "k",
				At: "2026-08-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.9, Kind: EventNew},
		}
		if err := base.st.WriteFacetsLogged([]Facet{baseFacet}, baseEvents); err != nil {
			t.Fatal(err)
		}

		inc := newArch(t)
		inc.obs("oi1", "s9", "default", "2026-02-01T00:00:00Z")
		incFacet := Facet{Lens: "default", Dimension: "thinking", Key: "k", LastSeen: "2026-02-01T00:00:00Z",
			Versions: []FacetVersion{{Value: "February view", Confidence: 0.7, ConfidenceSeq: 1,
				ValidFrom: "2026-02-01T00:00:00Z", RecordedAt: "2026-02-01T00:00:00Z", BecauseOf: []string{"oi1"}}}}
		inc.facets(incFacet)

		rep := base.merge(inc.export(), true)
		if len(rep.Facets.SameKey) != 1 || rep.Facets.SameKey[0].IncomingWon {
			t.Fatalf("want one collision won by ours, got %+v", rep.Facets.SameKey)
		}

		events, err := base.st.ReadConfidenceLog()
		if err != nil {
			t.Fatalf("ReadConfidenceLog: %v", err)
		}

		// Our rows stay under "k"
		var kEvents []ConfidenceEvent
		for _, ev := range events {
			if ev.Key == "k" {
				kEvents = append(kEvents, ev)
			} else if ev.Key == "k__m1" {
				t.Errorf("found log row under 'k__m1' when ours won: %+v", ev)
			}
		}
		if len(kEvents) != 1 {
			t.Errorf("want 1 event under k, got %d: %+v", len(kEvents), kEvents)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		base := newArch(t)
		base.obs("ob1", "s1", "default", "2026-02-01T00:00:00Z")
		baseFacet := Facet{Lens: "default", Dimension: "thinking", Key: "k", LastSeen: "2026-02-01T00:00:00Z",
			Versions: []FacetVersion{{Value: "February view", Confidence: 0.9, ConfidenceSeq: 1,
				ValidFrom: "2026-02-01T00:00:00Z", RecordedAt: "2026-02-01T00:00:00Z", BecauseOf: []string{"ob1"}}}}
		baseEvents := []ConfidenceEvent{
			{Lens: "default", Dimension: "thinking", Key: "k",
				At: "2026-02-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.9, Kind: EventNew},
		}
		if err := base.st.WriteFacetsLogged([]Facet{baseFacet}, baseEvents); err != nil {
			t.Fatal(err)
		}

		inc := newArch(t)
		inc.obs("oi1", "s9", "default", "2026-08-01T00:00:00Z")
		incFacet := Facet{Lens: "default", Dimension: "thinking", Key: "k", LastSeen: "2026-08-01T00:00:00Z",
			Versions: []FacetVersion{{Value: "August view", Confidence: 0.7, ConfidenceSeq: 1,
				ValidFrom: "2026-08-01T00:00:00Z", RecordedAt: "2026-08-01T00:00:00Z", BecauseOf: []string{"oi1"}}}}
		inc.facets(incFacet)

		// Dry run
		base.merge(inc.export(), false)

		events, err := base.st.ReadConfidenceLog()
		if err != nil {
			t.Fatalf("ReadConfidenceLog: %v", err)
		}

		// Log must be unchanged — still under "k"
		want := []ConfidenceEvent{
			{Lens: "default", Dimension: "thinking", Key: "k",
				At: "2026-02-01T12:00:00Z", ClockSeq: 1, From: 0.0, To: 0.9, Kind: EventNew},
		}
		if !reflect.DeepEqual(events, want) {
			t.Errorf("dry run changed log:\n got %+v\nwant %+v", events, want)
		}
	})
}
