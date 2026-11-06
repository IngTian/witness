package view

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// est is every trait test's local zone. Fixtures deliberately mix offsets (Z, -05:00, -08:00)
// so a string comparison of RFC3339 times, or a day bucketed in UTC, gives a different answer.
var est = time.FixedZone("EST", -5*3600)

func traitByKey(t *testing.T, m *Model, key string) Trait {
	t.Helper()
	for _, tr := range m.Traits {
		if tr.Key == key {
			return tr
		}
	}
	t.Fatalf("no active trait %q among %d", key, len(m.Traits))
	return Trait{}
}

func wantTrait(t *testing.T, got, want Trait) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("trait %s:\n got  %s\n want %s", want.Key, g, w)
	}
}

func wantNotebooks(t *testing.T, got, want []Notebook) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("notebooks:\n got  %s\n want %s", g, w)
	}
}

type windowCount struct {
	Label               string
	NewTraits, Rewrites int
}

func windowCounts(m *Model) []windowCount {
	out := []windowCount{}
	for _, w := range m.Activity.Windows {
		out = append(out, windowCount{w.Label, w.NewTraits, w.Rewrites})
	}
	return out
}

func TestTraitBasics(t *testing.T) {
	in := &store.ViewInput{
		Facets: []store.Facet{{
			Lens: "default", Dimension: "biases", Key: "lets_prestige_leak",
			LastSeen: "2026-03-10T12:00:00Z",
			Versions: []store.FacetVersion{
				{Value: "old", ValidFrom: "2026-03-05T12:00:00Z", ValidTo: "2026-03-08T12:00:00Z",
					BecauseOf: []string{"n1", "n2", "gone"}, Confidence: 0.6},
				{Value: "new", Claim: "Prestige slips into decisions.", ValidFrom: "2026-03-08T12:00:00Z",
					BecauseOf: []string{"n2", "n3", "n5"}, Confidence: 0.9, ConfidenceSeq: 1000},
			},
		}},
		Notes: []store.ViewNote{
			// n1 is the earliest INSTANT (Feb 28 23:00 EST) but sorts after n2 as a string.
			{ID: "n1", TS: "2026-03-01T04:00:00Z", Session: "s1", Lens: "default", Observation: "o1", Evidence: "e1", Poignancy: 5},
			{ID: "n2", TS: "2026-03-01T02:00:00-08:00", Session: "s1", Lens: "default", Observation: "o2", Evidence: "e2", Poignancy: 7},
			{ID: "n3", TS: "2026-03-04T10:00:00-05:00", Session: "s1", Lens: "default", Observation: "o3", Evidence: "e3", Poignancy: 3},
			{ID: "n4", TS: "2026-03-04T11:00:00-05:00", Session: "s1", Lens: "default", Observation: "o4", Evidence: "e4", Poignancy: 9}, // uncited
			// Sunday night in EST, Monday Mar 9 in UTC: its week is Mar 2 only when bucketed in loc.
			{ID: "n5", TS: "2026-03-08T22:00:00-05:00", Session: "s1", Lens: "default", Observation: "o5", Evidence: "e5", Poignancy: 1},
		},
		HighWater: 4000, // 3000 observations past the open version's clock: one half-life
	}
	m := Build(in, nil, time.Date(2026, 3, 12, 0, 0, 0, 0, est), est)

	got := traitByKey(t, m, "lets_prestige_leak")
	wantTrait(t, got, Trait{
		Lens: "default", Dimension: "biases", Key: "lets_prestige_leak",
		Name:    "Lets prestige leak",
		Summary: "Prestige slips into decisions.",
		Notes:   4,            // n1, n2, n3, n5 once each; "gone" does not exist
		Arrival: "2026-02-28", // n1, not the first version's 2026-03-05
		Today:   0.45,         // 0.9 halved, never the stored 0.9
		Points:  []Point{{At: "2026-03-10", Value: 0.9}},
		Spans:   []Span{{From: "2026-03-05", To: "2026-03-08", Value: 0.6}},
		Trend:   "up", TrendFrom: 0.6, Rewrites: 1, Changes: 1,
		Weeks: []Week{{Week: "2026-02-23", N: 2}, {Week: "2026-03-02", N: 2}},
		Evidence: []Evidence{
			{Date: "2026-03-01", Evidence: "e2", Note: "o2"},
			{Date: "2026-02-28", Evidence: "e1", Note: "o1"},
			{Date: "2026-03-04", Evidence: "e3", Note: "o3"},
		},
	})
	// The GUI and `witness facets` must never disagree about the same trait.
	if want := in.Facets[0].Current().DecayedConfidence(in.HighWater); got.Today != want {
		t.Errorf("Today = %v, want DecayedConfidence(HighWater) = %v", got.Today, want)
	}
	wantNotebooks(t, m.Notebooks, []Notebook{{Name: "default", Topics: []Topic{
		{Key: "biases", Label: "blind spots", Traits: []int{0}, AxisStart: "2026-02-01"},
	}}})

	for key, want := range map[string]string{
		"lets_prestige_leak": "Lets prestige leak",
		"élan_vital":         "Élan vital", // first RUNE, not first byte
		"已知_unknowns":        "已知 unknowns",
		"already_Capital_X":  "Already Capital X", // nothing but the first rune changes
		"":                   "",
	} {
		if got := traitName(key); got != want {
			t.Errorf("traitName(%q) = %q, want %q", key, got, want)
		}
	}
	for dim, want := range map[string]string{
		"biases": "blind spots", "workstyle": "how you work", "problem_solving": "problem solving",
	} {
		if got := topicLabel(dim); got != want {
			t.Errorf("topicLabel(%q) = %q, want %q", dim, got, want)
		}
	}
}

func TestTraitPointsAndSpans(t *testing.T) {
	in := &store.ViewInput{
		Facets: []store.Facet{{
			Lens: "default", Dimension: "habits", Key: "ships_small", LastSeen: "2026-03-20T12:00:00Z",
			Versions: []store.FacetVersion{
				{Value: "a", ValidFrom: "2026-01-10T12:00:00Z", ValidTo: "2026-02-10T12:00:00Z", Confidence: 0.5},
				{Value: "b", ValidFrom: "2026-02-10T12:00:00Z", ValidTo: "2026-03-10T12:00:00Z", Confidence: 0.7},
				{Value: "c", ValidFrom: "2026-03-10T12:00:00Z", Confidence: 0.8, ConfidenceSeq: 50},
			},
		}},
		Log: []store.ConfidenceEvent{
			// The rewrite that closed b: reviewer.go stamps the old ValidTo, the new ValidFrom and
			// this row with one nowStr. Same instant as b.ValidTo, written in another offset. It
			// records c's value, so b keeps its span.
			{Lens: "default", Dimension: "habits", Key: "ships_small", At: "2026-03-10T07:00:00-05:00", From: 0.7, To: 0.72, Kind: store.EventRewrite},
			{Lens: "default", Dimension: "habits", Key: "ships_small", At: "2026-03-20T12:00:00Z", From: 0.72, To: 0.8, Kind: store.EventReinforce},
			// Another trait's row inside a's lifetime: rows match on lens+dim+key, so a keeps its span.
			{Lens: "default", Dimension: "habits", Key: "other", At: "2026-01-20T12:00:00Z", To: 0.3, Kind: store.EventNew},
			{Lens: "math", Dimension: "habits", Key: "ships_small", At: "2026-01-21T12:00:00Z", To: 0.3, Kind: store.EventNew},
		},
		HighWater: 50,
	}
	m := Build(in, nil, time.Date(2026, 3, 25, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "ships_small"), Trait{
		Lens: "default", Dimension: "habits", Key: "ships_small",
		Name: "Ships small", Arrival: "2026-01-10", Today: 0.8,
		Points: []Point{{At: "2026-03-10", Value: 0.72}, {At: "2026-03-20", Value: 0.8}}, // no LastSeen point
		Spans: []Span{
			{From: "2026-01-10", To: "2026-02-10", Value: 0.5},
			{From: "2026-02-10", To: "2026-03-10", Value: 0.7},
		},
		Trend: "up", TrendFrom: 0.5, Rewrites: 2, Changes: 3,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
}

func TestTraitSpanSkipsLoggedVersion(t *testing.T) {
	in := &store.ViewInput{
		Facets: []store.Facet{
			{
				Lens: "default", Dimension: "habits", Key: "asks_why", LastSeen: "2026-04-12T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-04-01T12:00:00Z", ValidTo: "2026-04-10T12:00:00Z", Confidence: 0.65},
					{ValidFrom: "2026-04-10T12:00:00Z", Confidence: 0.7},
				},
			},
			{
				// An open version no log row reached (rows only before its ValidFrom): today's stored
				// value still gets its point, at LastSeen.
				Lens: "default", Dimension: "habits", Key: "stale_log", LastSeen: "2026-04-15T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-04-01T12:00:00Z", ValidTo: "2026-04-10T12:00:00Z", Confidence: 0.6},
					{ValidFrom: "2026-04-10T12:00:00Z", Confidence: 0.75},
				},
			},
		},
		Log: []store.ConfidenceEvent{
			{Lens: "default", Dimension: "habits", Key: "asks_why", At: "2026-04-01T12:00:00Z", To: 0.6, Kind: store.EventNew},
			// Re-confirmed at the same value: a point, but not a change.
			{Lens: "default", Dimension: "habits", Key: "asks_why", At: "2026-04-03T12:00:00Z", From: 0.6, To: 0.6, Kind: store.EventReinforce},
			{Lens: "default", Dimension: "habits", Key: "asks_why", At: "2026-04-05T12:00:00Z", To: 0.65, Kind: store.EventReinforce},
			{Lens: "default", Dimension: "habits", Key: "asks_why", At: "2026-04-10T12:00:00Z", To: 0.7, Kind: store.EventRewrite},
			{Lens: "default", Dimension: "habits", Key: "stale_log", At: "2026-04-03T12:00:00Z", To: 0.6, Kind: store.EventReinforce},
		},
	}
	m := Build(in, nil, time.Date(2026, 4, 20, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "asks_why"), Trait{
		Lens: "default", Dimension: "habits", Key: "asks_why",
		Name: "Asks why", Arrival: "2026-04-01", Today: 0.7,
		Points: []Point{{At: "2026-04-01", Value: 0.6}, {At: "2026-04-03", Value: 0.6}, {At: "2026-04-05", Value: 0.65}, {At: "2026-04-10", Value: 0.7}},
		Spans:  []Span{}, // the logged version is drawn by its points, never also as a dashed guess
		Trend:  "up", TrendFrom: 0.6, Rewrites: 1, Changes: 2,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
	wantTrait(t, traitByKey(t, m, "stale_log"), Trait{
		Lens: "default", Dimension: "habits", Key: "stale_log",
		Name: "Stale log", Arrival: "2026-04-01", Today: 0.75,
		Points: []Point{{At: "2026-04-03", Value: 0.6}, {At: "2026-04-15", Value: 0.75}},
		Spans:  []Span{},
		Trend:  "up", TrendFrom: 0.6, Rewrites: 1, Changes: 1,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
}

func TestTraitOnlyTodaysValue(t *testing.T) {
	in := &store.ViewInput{Facets: []store.Facet{{
		Lens: "default", Dimension: "thinking", Key: "first_principles",
		LastSeen: "2026-05-03T02:00:00Z", // May 2 in EST
		Versions: []store.FacetVersion{{Claim: "Starts from the physics.", ValidFrom: "2026-05-01T12:00:00Z", Confidence: 0.7}},
	}}}
	m := Build(in, nil, time.Date(2026, 5, 5, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "first_principles"), Trait{
		Lens: "default", Dimension: "thinking", Key: "first_principles",
		Name: "First principles", Summary: "Starts from the physics.", Arrival: "2026-05-01", Today: 0.7,
		Points: []Point{{At: "2026-05-02", Value: 0.7}},
		Spans:  []Span{},
		Trend:  "only", TrendFrom: 0.7,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
}

func TestTraitLevel(t *testing.T) {
	for _, c := range []struct {
		first, last float64
		want        string
	}{
		{0.70, 0.73, "level"},
		{0.74, 0.70, "level"},
		// Exactly five points is NOT level, though 0.60-0.55 is 0.0499… in float64.
		{0.55, 0.60, "up"},
		{0.60, 0.55, "down"},
		{0.70, 0.75, "up"},
	} {
		in := &store.ViewInput{Facets: []store.Facet{{
			Lens: "default", Dimension: "habits", Key: "k", LastSeen: "2026-05-10T12:00:00Z",
			Versions: []store.FacetVersion{
				{ValidFrom: "2026-05-01T12:00:00Z", ValidTo: "2026-05-05T12:00:00Z", Confidence: c.first},
				{ValidFrom: "2026-05-05T12:00:00Z", Confidence: c.last},
			},
		}}}
		tr := traitByKey(t, Build(in, nil, time.Date(2026, 5, 12, 0, 0, 0, 0, est), est), "k")
		if tr.Trend != c.want || tr.TrendFrom != c.first {
			t.Errorf("%.2f → %.2f: Trend %q from %v, want %q from %v", c.first, c.last, tr.Trend, tr.TrendFrom, c.want, c.first)
		}
	}
}

func TestCompactionVersionIsNotARewrite(t *testing.T) {
	in := &store.ViewInput{Facets: []store.Facet{
		{
			Lens: "default", Dimension: "habits", Key: "reads_the_docs", LastSeen: "2026-04-01T12:00:00Z",
			Versions: []store.FacetVersion{
				{ValidFrom: "2026-03-01T12:00:00Z", ValidTo: "2026-04-01T12:00:00Z", Confidence: 0.6},
				{ValidFrom: "2026-04-01T12:00:00Z", Confidence: 0.8}, // written by the compaction below
			},
		},
		{
			// Absorbed by the survivor, at the same instant as its second version, in another offset.
			Lens: "default", Dimension: "habits", Key: "checks_sources",
			ArchivedAt: "2026-04-01T08:00:00-04:00", MergedInto: "habits/reads_the_docs",
			Versions: []store.FacetVersion{{ValidFrom: "2026-02-01T12:00:00Z", Confidence: 0.5}},
		},
		{
			// Same target ref in ANOTHER lens: compaction never crosses lenses, so not an absorber.
			Lens: "math", Dimension: "habits", Key: "elsewhere",
			ArchivedAt: "2026-03-15T12:00:00Z", MergedInto: "habits/reads_the_docs",
			Versions: []store.FacetVersion{{ValidFrom: "2026-02-01T12:00:00Z", Confidence: 0.5}},
		},
		{
			// A plain rewrite at the same instant: the absorber above merged into reads_the_docs,
			// not into this, so this version is review-written.
			Lens: "default", Dimension: "habits", Key: "plain", LastSeen: "2026-04-01T12:00:00Z",
			Versions: []store.FacetVersion{
				{ValidFrom: "2026-03-01T12:00:00Z", ValidTo: "2026-04-01T12:00:00Z", Confidence: 0.6},
				{ValidFrom: "2026-04-01T12:00:00Z", Confidence: 0.8},
			},
		},
		{
			// Grown by two compactions: both later versions are compaction-written, and it was
			// formed at the FIRST absorption (listed second here, so input order cannot fake it).
			Lens: "default", Dimension: "habits", Key: "formed_twice", LastSeen: "2026-04-03T12:00:00Z",
			Versions: []store.FacetVersion{
				{ValidFrom: "2026-03-01T12:00:00Z", ValidTo: "2026-03-20T12:00:00Z", Confidence: 0.5},
				{ValidFrom: "2026-03-20T12:00:00Z", ValidTo: "2026-04-03T12:00:00Z", Confidence: 0.6},
				{ValidFrom: "2026-04-03T12:00:00Z", Confidence: 0.7},
			},
		},
		{Lens: "default", Dimension: "habits", Key: "late", ArchivedAt: "2026-04-03T12:00:00Z", MergedInto: "habits/formed_twice",
			Versions: []store.FacetVersion{{ValidFrom: "2026-02-01T12:00:00Z", Confidence: 0.5}}},
		{Lens: "default", Dimension: "habits", Key: "early", ArchivedAt: "2026-03-20T12:00:00Z", MergedInto: "habits/formed_twice",
			Versions: []store.FacetVersion{{ValidFrom: "2026-02-01T12:00:00Z", Confidence: 0.5}}},
	}}
	m := Build(in, nil, time.Date(2026, 4, 5, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "reads_the_docs"), Trait{
		Lens: "default", Dimension: "habits", Key: "reads_the_docs",
		Name: "Reads the docs", Arrival: "2026-03-01", Today: 0.8,
		Points: []Point{{At: "2026-04-01", Value: 0.8}},
		Spans:  []Span{{From: "2026-03-01", To: "2026-04-01", Value: 0.6}},
		Trend:  "up", TrendFrom: 0.6, Rewrites: 0, Changes: 1,
		Formed: &Formed{Date: "2026-04-01", Merged: 1},
		Weeks:  []Week{}, Evidence: []Evidence{},
	})
	plain := traitByKey(t, m, "plain")
	if plain.Rewrites != 1 || plain.Formed != nil {
		t.Errorf("plain: Rewrites %d Formed %+v, want 1 and nil", plain.Rewrites, plain.Formed)
	}
	twice := traitByKey(t, m, "formed_twice")
	if want := (Formed{Date: "2026-03-20", Merged: 2}); twice.Rewrites != 0 || twice.Formed == nil || *twice.Formed != want {
		t.Errorf("formed_twice: Rewrites %d Formed %+v, want 0 and %+v", twice.Rewrites, twice.Formed, want)
	}
	if len(m.Traits) != 3 {
		t.Errorf("Traits: got %d, want 3 (every absorbed facet is archived)", len(m.Traits))
	}
}

func TestArchivedTraitsExcluded(t *testing.T) {
	in := &store.ViewInput{
		Turns: []store.ViewTurn{{Session: "s1", TS: "2026-03-02T10:00:00-05:00"}},
		Facets: []store.Facet{
			{Lens: "default", Dimension: "habits", Key: "kept", LastSeen: "2026-03-02T12:00:00Z",
				Versions: []store.FacetVersion{{ValidFrom: "2026-03-02T12:00:00Z", Confidence: 0.5}}},
			// Retired by compaction, with an arrival and a rewrite inside March.
			{Lens: "default", Dimension: "habits", Key: "retired", ArchivedAt: "2026-03-04T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-03-01T12:00:00Z", ValidTo: "2026-03-03T12:00:00Z", Confidence: 0.5},
					{ValidFrom: "2026-03-03T12:00:00Z", Confidence: 0.6},
				}},
			// Never archived but has no open version: not part of the live profile either.
			{Lens: "default", Dimension: "thinking", Key: "expired",
				Versions: []store.FacetVersion{{ValidFrom: "2026-03-01T12:00:00Z", ValidTo: "2026-03-03T12:00:00Z", Confidence: 0.5}}},
		},
	}
	m := Build(in, nil, time.Date(2026, 3, 5, 0, 0, 0, 0, est), est)

	if len(m.Traits) != 1 || m.Traits[0].Key != "kept" {
		t.Fatalf("Traits: got %+v, want only kept", m.Traits)
	}
	wantNotebooks(t, m.Notebooks, []Notebook{{Name: "default", Topics: []Topic{
		{Key: "habits", Label: "habits", Traits: []int{0}, AxisStart: "2026-03-01"},
	}}})
	if m.Activity.TotalTraits != 1 {
		t.Errorf("TotalTraits: got %d, want 1", m.Activity.TotalTraits)
	}
	if got, want := windowCounts(m)[0], (windowCount{"Mar 2026", 1, 0}); got != want {
		t.Errorf("March: got %+v, want %+v", got, want)
	}
}

func TestTraitWithNoSurvivingNotes(t *testing.T) {
	in := &store.ViewInput{
		Facets: []store.Facet{{
			Lens: "default", Dimension: "thinking", Key: "quiet_one", LastSeen: "2026-05-10T12:00:00Z",
			Versions: []store.FacetVersion{{ValidFrom: "2026-05-02T02:00:00Z", BecauseOf: []string{"gone1", "gone2"}, Confidence: 0.4}},
		}},
		Notes: []store.ViewNote{{ID: "other", TS: "2026-04-01T10:00:00Z", Session: "s1", Lens: "default", Poignancy: 9}},
	}
	m := Build(in, nil, time.Date(2026, 5, 12, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "quiet_one"), Trait{
		Lens: "default", Dimension: "thinking", Key: "quiet_one",
		Name: "Quiet one", Notes: 0,
		Arrival: "2026-05-01", // the first version's ValidFrom, in EST
		Today:   0.4,
		Points:  []Point{{At: "2026-05-10", Value: 0.4}},
		Spans:   []Span{},
		Trend:   "only", TrendFrom: 0.4,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
	wantNotebooks(t, m.Notebooks, []Notebook{{Name: "default", Topics: []Topic{
		{Key: "thinking", Label: "thinking", Traits: []int{0}, AxisStart: "2026-05-01"},
	}}})
}

func TestEvidenceOrderAndAllowList(t *testing.T) {
	note := func(id, ts string, p int) store.ViewNote {
		return store.ViewNote{ID: id, TS: ts, Session: "s1", Lens: "default", Dimension: "habits",
			Observation: "obs-" + id, Evidence: "ev-" + id, Poignancy: p}
	}
	in := &store.ViewInput{
		Facets: []store.Facet{{
			Lens: "default", Dimension: "habits", Key: "k", LastSeen: "2026-06-05T12:00:00Z",
			Versions: []store.FacetVersion{{ValidFrom: "2026-06-01T12:00:00Z", BecauseOf: []string{"a", "b", "c", "d", "e"}, Confidence: 0.5}},
		}},
		Notes: []store.ViewNote{
			note("a", "2026-06-01T10:00:00-05:00", 8),
			note("b", "2026-06-02T10:00:00-05:00", 9),
			note("c", "2026-06-03T10:00:00-05:00", 8),
			note("d", "2026-06-04T10:00:00-05:00", 2),
			note("e", "2026-06-01T09:00:00-08:00", 8), // 2h AFTER a, though its string sorts before a's
		},
	}
	m := Build(in, nil, time.Date(2026, 6, 6, 0, 0, 0, 0, est), est)

	tr := traitByKey(t, m, "k")
	want := []Evidence{
		{Date: "2026-06-02", Evidence: "ev-b", Note: "obs-b"},
		{Date: "2026-06-03", Evidence: "ev-c", Note: "obs-c"},
		{Date: "2026-06-01", Evidence: "ev-e", Note: "obs-e"},
	}
	if !reflect.DeepEqual(tr.Evidence, want) {
		t.Errorf("Evidence:\n got  %+v\n want %+v", tr.Evidence, want)
	}
	if want := []Week{{Week: "2026-06-01", N: 5}}; !reflect.DeepEqual(tr.Weeks, want) {
		t.Errorf("Weeks: got %+v, want %+v", tr.Weeks, want)
	}

	raw, err := json.Marshal(tr.Evidence[0])
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"date", "evidence", "note"}) {
		t.Errorf("Evidence JSON keys: got %v, want exactly [date evidence note]", keys)
	}
	// Only the three strongest notes' text leaves the archive.
	all, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "d"} {
		if s := string(all); strings.Contains(s, "obs-"+id) || strings.Contains(s, "ev-"+id) {
			t.Errorf("snapshot contains text of note %s, which is not in the top three", id)
		}
	}
}

func TestCorpusNotebook(t *testing.T) {
	in := &store.ViewInput{
		Turns: []store.ViewTurn{{Session: "s1", TS: "2026-03-02T10:00:00-05:00"}},
		Notes: []store.ViewNote{
			{ID: "b1", TS: "2026-03-03T10:00:00-05:00", Session: "file:/books/a.md", Lens: "books"},
			{ID: "b2", TS: "2026-03-05T10:00:00-05:00", Session: "file:/books/b.md", Lens: "books"},
			{ID: "d1", TS: "2026-03-02T10:00:00-05:00", Session: "s1", Lens: "default"},
			{ID: "m1", TS: "2026-03-04T10:00:00-05:00", Session: "file:/x.md", Lens: "mixed"},
			{ID: "m2", TS: "2026-03-06T10:00:00-05:00", Session: "s1", Lens: "mixed"},
		},
		Facets: []store.Facet{
			// Corpus: arrives and is rewritten in March, and must move neither number.
			{Lens: "books", Dimension: "themes", Key: "rivers_matter", LastSeen: "2026-03-05T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-03-03T12:00:00Z", ValidTo: "2026-03-05T12:00:00Z", BecauseOf: []string{"b1"}, Confidence: 0.5},
					{ValidFrom: "2026-03-05T12:00:00Z", BecauseOf: []string{"b2"}, Confidence: 0.6},
				}},
			{Lens: "default", Dimension: "habits", Key: "ships", LastSeen: "2026-03-02T12:00:00Z",
				Versions: []store.FacetVersion{{ValidFrom: "2026-03-02T12:00:00Z", BecauseOf: []string{"d1"}, Confidence: 0.5}}},
			// One conversation note is enough to make a notebook not a corpus.
			{Lens: "mixed", Dimension: "habits", Key: "both", LastSeen: "2026-03-06T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-03-04T12:00:00Z", ValidTo: "2026-03-06T12:00:00Z", BecauseOf: []string{"m1"}, Confidence: 0.5},
					{ValidFrom: "2026-03-06T12:00:00Z", BecauseOf: []string{"m2"}, Confidence: 0.6},
				}},
			// A lens with no notes at all is not a corpus (there is nothing to say it is one).
			{Lens: "nolog", Dimension: "habits", Key: "bare", LastSeen: "2026-03-07T12:00:00Z",
				Versions: []store.FacetVersion{{ValidFrom: "2026-03-07T12:00:00Z", Confidence: 0.5}}},
		},
	}
	m := Build(in, nil, time.Date(2026, 3, 31, 0, 0, 0, 0, est), est)

	topic := func(id int) []Topic {
		return []Topic{{Key: "habits", Label: "habits", Traits: []int{id}, AxisStart: "2026-03-01"}}
	}
	wantNotebooks(t, m.Notebooks, []Notebook{
		{Name: "books", Corpus: true, Topics: []Topic{{Key: "themes", Label: "themes", Traits: []int{0}, AxisStart: "2026-03-01"}}},
		{Name: "default", Topics: topic(1)},
		{Name: "mixed", Topics: topic(2)},
		{Name: "nolog", Topics: topic(3)},
	})
	if m.Activity.TotalTraits != 4 {
		t.Errorf("TotalTraits: got %d, want 4 (a corpus trait is still a trait)", m.Activity.TotalTraits)
	}
	if got, want := windowCounts(m)[0], (windowCount{"Mar 2026", 3, 1}); got != want {
		t.Errorf("March: got %+v, want %+v (default+mixed+nolog arrive; mixed is rewritten)", got, want)
	}
}

func TestTopicAndTraitOrder(t *testing.T) {
	one := func(lens, dim, key, from string) store.Facet {
		return store.Facet{Lens: lens, Dimension: dim, Key: key, LastSeen: from,
			Versions: []store.FacetVersion{{ValidFrom: from, Confidence: 0.5}}}
	}
	in := &store.ViewInput{Facets: []store.Facet{
		one("default", "workstyle", "b_side", "2026-03-05T12:00:00Z"),
		one("default", "ideas", "one", "2026-02-20T12:00:00Z"),
		one("default", "workstyle", "zed", "2026-02-25T12:00:00Z"),
		one("default", "workstyle", "a_side", "2026-03-05T12:00:00Z"),
		one("alpha", "x", "y", "2026-01-15T12:00:00Z"),
	}}
	m := Build(in, nil, time.Date(2026, 3, 10, 0, 0, 0, 0, est), est)

	// Notebooks by name; topics by LABEL ("how you work" < "ideas", though "ideas" < "workstyle");
	// traits by arrival, then name.
	wantNotebooks(t, m.Notebooks, []Notebook{
		{Name: "alpha", Topics: []Topic{{Key: "x", Label: "x", Traits: []int{0}, AxisStart: "2026-01-01"}}},
		{Name: "default", Topics: []Topic{
			{Key: "workstyle", Label: "how you work", Traits: []int{1, 2, 3}, AxisStart: "2026-02-01"}, // zed's month
			{Key: "ideas", Label: "ideas", Traits: []int{4}, AxisStart: "2026-02-01"},
		}},
	})
	var keys []string
	for _, tr := range m.Traits {
		keys = append(keys, tr.Key)
	}
	if want := []string{"y", "zed", "a_side", "b_side", "one"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("trait ids: got %v, want %v", keys, want)
	}
}

func TestActivityNewTraitsAndRewrites(t *testing.T) {
	note := func(id, ts string) store.ViewNote {
		return store.ViewNote{ID: id, TS: ts, Session: "s1", Lens: "default"}
	}
	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-02-10T10:00:00-05:00"},
			{Session: "s1", TS: "2026-04-15T10:00:00-05:00"},
		},
		Notes: []store.ViewNote{
			note("n1", "2026-02-12T10:00:00-05:00"),
			note("n3", "2026-03-04T10:00:00-05:00"),
			note("n4", "2026-04-02T10:00:00-05:00"),
		},
		Facets: []store.Facet{
			// t1 arrives in February (its note) and is rewritten twice in March — the second on
			// Mar 31 in EST, though Apr 1 in UTC.
			{Lens: "default", Dimension: "habits", Key: "t1", LastSeen: "2026-04-01T03:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-02-20T12:00:00Z", ValidTo: "2026-03-10T12:00:00Z", BecauseOf: []string{"n1"}, Confidence: 0.5},
					{ValidFrom: "2026-03-10T12:00:00Z", ValidTo: "2026-04-01T03:00:00Z", Confidence: 0.6},
					{ValidFrom: "2026-04-01T03:00:00Z", Confidence: 0.7},
				}},
			// t2 cites nothing: arrives on its ValidFrom, Feb 28 in EST.
			{Lens: "default", Dimension: "habits", Key: "t2", LastSeen: "2026-03-01T03:00:00Z",
				Versions: []store.FacetVersion{{ValidFrom: "2026-03-01T03:00:00Z", Confidence: 0.5}}},
			// t3 arrives in March; its April version is compaction-written, not a rewrite.
			{Lens: "default", Dimension: "habits", Key: "t3", LastSeen: "2026-04-10T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-03-05T12:00:00Z", ValidTo: "2026-04-10T12:00:00Z", BecauseOf: []string{"n3"}, Confidence: 0.5},
					{ValidFrom: "2026-04-10T12:00:00Z", Confidence: 0.6},
				}},
			{Lens: "default", Dimension: "habits", Key: "absorbed", ArchivedAt: "2026-04-10T12:00:00Z", MergedInto: "habits/t3",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-02-15T12:00:00Z", ValidTo: "2026-02-25T12:00:00Z", Confidence: 0.5},
					{ValidFrom: "2026-02-25T12:00:00Z", Confidence: 0.5},
				}},
			// t4 arrives in April and is rewritten in April.
			{Lens: "default", Dimension: "habits", Key: "t4", LastSeen: "2026-04-12T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-04-02T12:00:00Z", ValidTo: "2026-04-12T12:00:00Z", BecauseOf: []string{"n4"}, Confidence: 0.5},
					{ValidFrom: "2026-04-12T12:00:00Z", Confidence: 0.6},
				}},
		},
	}
	now := time.Date(2026, 4, 20, 0, 0, 0, 0, est)
	m := Build(in, nil, now, est)

	want := []windowCount{
		{"Feb 2026", 2, 0}, {"Mar 2026", 1, 2}, {"Apr 2026", 1, 1},
		{"Q1 2026", 3, 2}, {"Q2 2026", 1, 1},
		{"H1 2026", 4, 3},
		{"2026", 4, 3},
	}
	if got := windowCounts(m); !reflect.DeepEqual(got, want) {
		t.Errorf("windows:\n got  %+v\n want %+v", got, want)
	}
	if m.Activity.TotalTraits != 4 {
		t.Errorf("TotalTraits: got %d, want 4", m.Activity.TotalTraits)
	}

	// No conversation turns or notes at all (notes alone would set First): only now's windows
	// exist, and they still count traits. Arrivals fall back to ValidFrom, in the same months.
	in.Turns, in.Notes = nil, nil
	want = []windowCount{{"Apr 2026", 1, 1}, {"Q2 2026", 1, 1}, {"H1 2026", 4, 3}, {"2026", 4, 3}}
	if got := windowCounts(Build(in, nil, now, est)); !reflect.DeepEqual(got, want) {
		t.Errorf("windows without turns:\n got  %+v\n want %+v", got, want)
	}
}

func TestTraitBadTimestamps(t *testing.T) {
	in := &store.ViewInput{
		Facets: []store.Facet{
			{Lens: "default", Dimension: "habits", Key: "garbled", LastSeen: "nope",
				Versions: []store.FacetVersion{{ValidFrom: "garbage", BecauseOf: []string{"n1"}, Confidence: 0.5}}},
			{Lens: "default", Dimension: "habits", Key: "gone", ArchivedAt: "bad", MergedInto: "habits/garbled",
				Versions: []store.FacetVersion{{ValidFrom: "2026-03-01T12:00:00Z", Confidence: 0.5}}},
		},
		Notes: []store.ViewNote{{ID: "n1", TS: "", Session: "s1", Lens: "default", Observation: "o", Evidence: "e", Poignancy: 5}},
		Log:   []store.ConfidenceEvent{{Lens: "default", Dimension: "habits", Key: "garbled", At: "", To: 0.9}},
	}
	m := Build(in, nil, time.Date(2026, 3, 5, 0, 0, 0, 0, est), est)

	// Nothing dated survives, so nothing is placed: no "year 1" arrival, point or week.
	wantTrait(t, traitByKey(t, m, "garbled"), Trait{
		Lens: "default", Dimension: "habits", Key: "garbled", Name: "Garbled",
		Notes: 1, Today: 0.5, Points: []Point{}, Spans: []Span{}, Trend: "only",
		Formed: &Formed{Merged: 1},
		Weeks:  []Week{}, Evidence: []Evidence{},
	})
	wantNotebooks(t, m.Notebooks, []Notebook{{Name: "default", Topics: []Topic{
		{Key: "habits", Label: "habits", Traits: []int{0}},
	}}})
}

func TestConfirmedValuesInTimeOrder(t *testing.T) {
	const T = "2026-05-10T12:00:00Z"
	in := &store.ViewInput{
		Facets: []store.Facet{
			{
				// (a) A multi-window review Run stamps every window with one nowStr: rewritten in two
				// windows, the facet gets a zero-length [T,T) version whose value is a log point.
				Lens: "default", Dimension: "habits", Key: "same_instant", LastSeen: T,
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-05-01T12:00:00Z", ValidTo: T, Confidence: 0.5},
					{ValidFrom: T, ValidTo: T, Confidence: 0.6},
					{ValidFrom: T, Confidence: 0.62},
				},
			},
			{
				// (b) An unlogged closed version (merge-appended) between a logged one and an
				// unlogged open one: its span belongs BETWEEN the two points.
				Lens: "default", Dimension: "habits", Key: "merge_between", LastSeen: "2026-05-25T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-05-01T12:00:00Z", ValidTo: "2026-05-10T12:00:00Z", Confidence: 0.5},
					{ValidFrom: "2026-05-10T12:00:00Z", ValidTo: "2026-05-20T12:00:00Z", Confidence: 0.8},
					{ValidFrom: "2026-05-20T12:00:00Z", Confidence: 0.82},
				},
			},
			{
				// A span and a point at the same instant (a stale LastSeen): the span goes first.
				Lens: "default", Dimension: "habits", Key: "tie", LastSeen: "2026-05-01T12:00:00Z",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-05-01T12:00:00Z", ValidTo: "2026-05-10T12:00:00Z", Confidence: 0.5},
					{ValidFrom: "2026-05-10T12:00:00Z", Confidence: 0.6},
				},
			},
		},
		Log: []store.ConfidenceEvent{
			{Lens: "default", Dimension: "habits", Key: "same_instant", At: "2026-05-01T12:00:00Z", To: 0.5, Kind: store.EventNew},
			{Lens: "default", Dimension: "habits", Key: "same_instant", At: T, From: 0.5, To: 0.6, Kind: store.EventRewrite},
			{Lens: "default", Dimension: "habits", Key: "same_instant", At: T, From: 0.6, To: 0.62, Kind: store.EventRewrite},
			{Lens: "default", Dimension: "habits", Key: "merge_between", At: "2026-05-01T12:00:00Z", To: 0.5, Kind: store.EventNew},
		},
	}
	m := Build(in, nil, time.Date(2026, 5, 30, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "same_instant"), Trait{
		Lens: "default", Dimension: "habits", Key: "same_instant",
		Name: "Same instant", Arrival: "2026-05-01", Today: 0.62,
		Points: []Point{{At: "2026-05-01", Value: 0.5}, {At: "2026-05-10", Value: 0.6}, {At: "2026-05-10", Value: 0.62}},
		Spans:  []Span{}, // not the [T,T) version
		Trend:  "up", TrendFrom: 0.5, Rewrites: 2, Changes: 2,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
	wantTrait(t, traitByKey(t, m, "merge_between"), Trait{
		Lens: "default", Dimension: "habits", Key: "merge_between",
		Name: "Merge between", Arrival: "2026-05-01", Today: 0.82,
		Points: []Point{{At: "2026-05-01", Value: 0.5}, {At: "2026-05-25", Value: 0.82}},
		Spans:  []Span{{From: "2026-05-10", To: "2026-05-20", Value: 0.8}},
		Trend:  "up", TrendFrom: 0.5, Rewrites: 2, Changes: 2, // 0.5 → 0.8 → 0.82, not 0.8 → 0.5 → 0.82
		Weeks: []Week{}, Evidence: []Evidence{},
	})
	if tie := traitByKey(t, m, "tie"); tie.Trend != "up" || tie.TrendFrom != 0.5 {
		t.Errorf("tie: Trend %q from %v, want up from 0.5 (span before point at one instant)", tie.Trend, tie.TrendFrom)
	}
}

func TestMergeAdoptedConfidenceIsLastPoint(t *testing.T) {
	one := func(key string, stored float64) store.Facet {
		return store.Facet{Lens: "default", Dimension: "habits", Key: key, LastSeen: "2026-06-08T12:00:00Z",
			Versions: []store.FacetVersion{{ValidFrom: "2026-06-01T12:00:00Z", Confidence: stored}}}
	}
	row := func(key string) store.ConfidenceEvent {
		return store.ConfidenceEvent{Lens: "default", Dimension: "habits", Key: key, At: "2026-06-01T12:00:00Z", To: 0.7, Kind: store.EventNew}
	}
	in := &store.ViewInput{
		// `witness merge` adopted the other side's newer 0.85 without logging it.
		Facets: []store.Facet{one("adopted", 0.85), one("agreed", 0.7)},
		Log:    []store.ConfidenceEvent{row("adopted"), row("agreed")},
	}
	m := Build(in, nil, time.Date(2026, 6, 10, 0, 0, 0, 0, est), est)

	wantTrait(t, traitByKey(t, m, "adopted"), Trait{
		Lens: "default", Dimension: "habits", Key: "adopted",
		Name: "Adopted", Arrival: "2026-06-01", Today: 0.85,
		Points: []Point{{At: "2026-06-01", Value: 0.7}, {At: "2026-06-08", Value: 0.85}},
		Spans:  []Span{},
		Trend:  "up", TrendFrom: 0.7, Changes: 1,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
	if want := []Point{{At: "2026-06-01", Value: 0.7}}; !reflect.DeepEqual(traitByKey(t, m, "agreed").Points, want) {
		t.Errorf("agreed: Points %+v, want %+v (the row already says the stored value)", traitByKey(t, m, "agreed").Points, want)
	}

	// The open version owns only rows at or after its ValidFrom: an EARLIER version's row that
	// happens to say the same 0.7 does not confirm this unlogged (merge-appended) version.
	carried := store.Facet{Lens: "default", Dimension: "habits", Key: "carried", LastSeen: "2026-06-08T12:00:00Z",
		Versions: []store.FacetVersion{
			{ValidFrom: "2026-06-01T12:00:00Z", ValidTo: "2026-06-05T12:00:00Z", Confidence: 0.7},
			{ValidFrom: "2026-06-05T12:00:00Z", Confidence: 0.7},
		}}
	m = Build(&store.ViewInput{Facets: []store.Facet{carried}, Log: []store.ConfidenceEvent{row("carried")}},
		nil, time.Date(2026, 6, 10, 0, 0, 0, 0, est), est)
	if want := []Point{{At: "2026-06-01", Value: 0.7}, {At: "2026-06-08", Value: 0.7}}; !reflect.DeepEqual(traitByKey(t, m, "carried").Points, want) {
		t.Errorf("carried: Points %+v, want %+v", traitByKey(t, m, "carried").Points, want)
	}
}

func TestTraitPointsSortedByTime(t *testing.T) {
	// Log id order is not time order once a long review Run interleaves with a compaction Apply.
	ev := func(at string, to float64) store.ConfidenceEvent {
		return store.ConfidenceEvent{Lens: "default", Dimension: "habits", Key: "shuffled", At: at, To: to, Kind: store.EventReinforce}
	}
	in := &store.ViewInput{
		Facets: []store.Facet{{Lens: "default", Dimension: "habits", Key: "shuffled", LastSeen: "2026-07-20T12:00:00Z",
			Versions: []store.FacetVersion{{ValidFrom: "2026-07-01T12:00:00Z", Confidence: 0.7}}}},
		Log: []store.ConfidenceEvent{ev("2026-07-20T12:00:00Z", 0.7), ev("2026-07-01T12:00:00Z", 0.5), ev("2026-07-10T12:00:00Z", 0.6)},
	}
	m := Build(in, nil, time.Date(2026, 7, 25, 0, 0, 0, 0, est), est)

	// The latest row by TIME already says the stored 0.7, so no LastSeen point is added.
	wantTrait(t, traitByKey(t, m, "shuffled"), Trait{
		Lens: "default", Dimension: "habits", Key: "shuffled",
		Name: "Shuffled", Arrival: "2026-07-01", Today: 0.7,
		Points: []Point{{At: "2026-07-01", Value: 0.5}, {At: "2026-07-10", Value: 0.6}, {At: "2026-07-20", Value: 0.7}},
		Spans:  []Span{},
		Trend:  "up", TrendFrom: 0.5, Changes: 2,
		Weeks: []Week{}, Evidence: []Evidence{},
	})
}
