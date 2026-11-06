package view

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

func TestActivityDaysAndConversations(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 3, 0, 0, 0, 0, loc)

	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-03-01T10:00:00-05:00"},
			{Session: "s1", TS: "2026-03-01T11:00:00-05:00"},
			{Session: "s1", TS: "2026-03-01T12:00:00-05:00"},
			{Session: "s1", TS: "2026-03-02T10:00:00-05:00"},
			{Session: "s2", TS: "2026-03-02T11:00:00-05:00"},
			{Session: "s2", TS: "2026-03-02T12:00:00-05:00"},
			{Session: "file:doc", TS: "2026-03-02T13:00:00-05:00"}, // excluded: file: session
		},
	}

	m := Build(in, nil, now, loc)

	// Expect 2 days
	if len(m.Activity.Days) != 2 {
		t.Errorf("Days: got %d, want 2", len(m.Activity.Days))
	}
	// Day 1: 2026-03-01, 1 conversation (s1), 3 turns, 0 notes
	if len(m.Activity.Days) > 0 {
		d := m.Activity.Days[0]
		if d.Date != "2026-03-01" {
			t.Errorf("Days[0].Date: got %s, want 2026-03-01", d.Date)
		}
		if d.Conversations != 1 {
			t.Errorf("Days[0].Conversations: got %d, want 1", d.Conversations)
		}
		if d.Turns != 3 {
			t.Errorf("Days[0].Turns: got %d, want 3", d.Turns)
		}
		if d.Notes != 0 {
			t.Errorf("Days[0].Notes: got %d, want 0", d.Notes)
		}
	}
	// Day 2: 2026-03-02, 2 conversations (s1, s2), 3 turns, 0 notes
	if len(m.Activity.Days) > 1 {
		d := m.Activity.Days[1]
		if d.Date != "2026-03-02" {
			t.Errorf("Days[1].Date: got %s, want 2026-03-02", d.Date)
		}
		if d.Conversations != 2 {
			t.Errorf("Days[1].Conversations: got %d, want 2 (set union, not 3)", d.Conversations)
		}
		if d.Turns != 3 {
			t.Errorf("Days[1].Turns: got %d, want 3", d.Turns)
		}
		if d.Notes != 0 {
			t.Errorf("Days[1].Notes: got %d, want 0", d.Notes)
		}
	}

	// March window: Conversations should be 2 (set union of s1, s2)
	if w := windowByLabel(t, m, "Mar 2026"); w.Conversations != 2 {
		t.Errorf("March.Conversations: got %d, want 2 (set union, not sum)", w.Conversations)
	}
}

func TestActivityLocalMidnight(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 3, 0, 0, 0, 0, loc)

	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			// 2026-03-02 03:30 UTC = 2026-03-01 22:30 EST
			{Session: "s1", TS: "2026-03-02T03:30:00Z"},
		},
	}

	m := Build(in, nil, now, loc)

	if len(m.Activity.Days) != 1 {
		t.Fatalf("Days: got %d, want 1", len(m.Activity.Days))
	}
	if m.Activity.Days[0].Date != "2026-03-01" {
		t.Errorf("Days[0].Date: got %s, want 2026-03-01 (local midnight)", m.Activity.Days[0].Date)
	}
}

func TestActivityIgnoresBadTimestamps(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 5, 0, 0, 0, 0, loc)

	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: ""},                          // empty
			{Session: "s2", TS: "garbage"},                   // unparseable
			{Session: "s3", TS: "2026-03-03T10:00:00-05:00"}, // good
		},
	}

	m := Build(in, nil, now, loc)

	if len(m.Activity.Days) != 1 {
		t.Errorf("Days: got %d, want 1 (bad timestamps skipped)", len(m.Activity.Days))
	}
	if len(m.Activity.Days) > 0 && m.Activity.Days[0].Date != "2026-03-03" {
		t.Errorf("Days[0].Date: got %s, want 2026-03-03", m.Activity.Days[0].Date)
	}

	// First should be the first good day
	if m.Activity.First != "2026-03-03" {
		t.Errorf("First: got %s, want 2026-03-03 (no window starts before first good day)", m.Activity.First)
	}
}

func TestActivityWindows(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, loc)

	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-02-10T10:00:00-05:00"}, // first conversation
			{Session: "s2", TS: "2026-11-01T10:00:00-05:00"},
		},
	}

	m := Build(in, nil, now, loc)

	// Expect 10 months (Feb–Nov), 4 quarters (Q1–Q4), 2 halves (H1, H2), 1 year (2026)
	k := windowsByKind(m)
	months, quarters, halves, years := k["month"], k["quarter"], k["half"], k["year"]

	if len(months) != 10 {
		t.Errorf("months: got %d, want 10 (Feb–Nov)", len(months))
	}
	if len(quarters) != 4 {
		t.Errorf("quarters: got %d, want 4 (Q1–Q4)", len(quarters))
	}
	if len(halves) != 2 {
		t.Errorf("halves: got %d, want 2 (H1, H2)", len(halves))
	}
	if len(years) != 1 {
		t.Errorf("years: got %d, want 1 (2026)", len(years))
	}

	// Check labels
	if len(months) > 0 && months[0].Label != "Feb 2026" {
		t.Errorf("months[0].Label: got %s, want 'Feb 2026'", months[0].Label)
	}
	if len(quarters) > 0 && quarters[0].Label != "Q1 2026" {
		t.Errorf("quarters[0].Label: got %s, want 'Q1 2026'", quarters[0].Label)
	}
	if len(halves) > 0 && halves[0].Label != "H1 2026" {
		t.Errorf("halves[0].Label: got %s, want 'H1 2026'", halves[0].Label)
	}
	if len(years) > 0 && years[0].Label != "2026" {
		t.Errorf("years[0].Label: got %s, want '2026'", years[0].Label)
	}

	// Nov has SoFar true and SpanDays 5 (Nov 1–5)
	if w := windowByLabel(t, m, "Nov 2026"); !w.SoFar || w.SpanDays != 5 {
		t.Errorf("Nov: SoFar %v SpanDays %d, want true and 5 (Nov 1–5)", w.SoFar, w.SpanDays)
	}

	// Feb has SpanDays 19 (Feb 10–28, 19 days)
	if w := windowByLabel(t, m, "Feb 2026"); w.SpanDays != 19 {
		t.Errorf("Feb.SpanDays: got %d, want 19 (Feb 10–28)", w.SpanDays)
	}
}

func TestActivityLongestRunAndBusiest(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, loc)

	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-03-01T10:00:00-05:00"}, // day 1
			{Session: "s1", TS: "2026-03-02T10:00:00-05:00"}, // day 2
			{Session: "s1", TS: "2026-03-02T11:00:00-05:00"}, // day 2 (2 turns)
			{Session: "s1", TS: "2026-03-03T10:00:00-05:00"}, // day 3
			// gap on day 4
			{Session: "s1", TS: "2026-03-05T10:00:00-05:00"}, // day 5
			{Session: "s1", TS: "2026-03-05T11:00:00-05:00"}, // day 5 (2 turns)
			{Session: "s1", TS: "2026-03-05T12:00:00-05:00"}, // day 5 (3 turns total)
			{Session: "s1", TS: "2026-03-06T10:00:00-05:00"}, // day 6
		},
	}

	m := Build(in, nil, now, loc)

	mar := windowByLabel(t, m, "Mar 2026")
	// LongestRun: days 1, 2, 3 = 3 consecutive days
	if mar.LongestRun != 3 {
		t.Errorf("March.LongestRun: got %d, want 3 (days 1,2,3)", mar.LongestRun)
	}

	// Busiest: day 5 with 3 turns (ties go to earliest, but day 5 wins outright)
	if mar.Busiest == nil {
		t.Errorf("March.Busiest: got nil, want day 5")
	} else if mar.Busiest.Date != "2026-03-05" {
		t.Errorf("March.Busiest.Date: got %s, want 2026-03-05", mar.Busiest.Date)
	} else if mar.Busiest.Turns != 3 {
		t.Errorf("March.Busiest.Turns: got %d, want 3", mar.Busiest.Turns)
	}

	// Test tie-breaking: if two days have same turns, earliest wins
	in2 := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-03-01T10:00:00-05:00"}, // day 1: 2 turns
			{Session: "s1", TS: "2026-03-01T11:00:00-05:00"},
			{Session: "s1", TS: "2026-03-05T10:00:00-05:00"}, // day 5: 2 turns
			{Session: "s1", TS: "2026-03-05T11:00:00-05:00"},
		},
	}

	m2 := Build(in2, nil, now, loc)
	if w := windowByLabel(t, m2, "Mar 2026"); w.Busiest == nil {
		t.Errorf("March.Busiest (tie): got nil")
	} else if w.Busiest.Date != "2026-03-01" {
		t.Errorf("March.Busiest.Date (tie): got %s, want 2026-03-01 (earliest)", w.Busiest.Date)
	}
}

func TestActivityCuts(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 11, 0, 0, 0, 0, loc)

	// 10 days with turns [1..10]
	in := &store.ViewInput{
		Turns: []store.ViewTurn{},
	}
	for day := 1; day <= 10; day++ {
		for turn := 1; turn <= day; turn++ {
			ts := time.Date(2026, 3, day, 10, turn, 0, 0, loc).Format(time.RFC3339)
			in.Turns = append(in.Turns, store.ViewTurn{Session: "s1", TS: ts})
		}
	}

	m := Build(in, nil, now, loc)

	// 20/40/60/80th percentiles of [1,2,3,4,5,6,7,8,9,10]
	// Nearest-rank: positions 2, 4, 6, 8 (1-indexed) → values 2, 4, 6, 8
	want := []int{2, 4, 6, 8}
	if len(m.Activity.Cuts) != len(want) {
		t.Errorf("Cuts: got %v, want %v", m.Activity.Cuts, want)
	} else {
		for i, c := range m.Activity.Cuts {
			if c != want[i] {
				t.Errorf("Cuts[%d]: got %d, want %d", i, c, want[i])
			}
		}
	}

	// A single day gives one cut
	in2 := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-03-01T10:00:00-05:00"},
			{Session: "s1", TS: "2026-03-01T11:00:00-05:00"},
		},
	}
	m2 := Build(in2, nil, now, loc)
	if len(m2.Activity.Cuts) != 1 {
		t.Errorf("Cuts (single day): got %v, want 1 cut", m2.Activity.Cuts)
	}

	// Every day gets a shade: shade = 1 + count(cut < turns)
	// Day with 1 turn: shade 1 (0 cuts < 1)
	// Day with 3 turns: shade 2 (cuts 2 < 3: no; so 1 cut < 3)
	// This is implicit in the cuts being correct; just verify no crash
}

func TestActivityCutsNearestRankCeil(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, loc)

	// With n=9 days, nearest-rank ceil vs floor gives different results:
	// - ceil(0.2·9)=2, ceil(3.6)=4, ceil(5.4)=6, ceil(7.2)=8 → turns at positions 2,4,6,8 (1-indexed) = [2,4,6,8]
	// - floor would give positions 1,3,5,7 → [1,3,5,7]
	in := &store.ViewInput{
		Turns: []store.ViewTurn{},
	}
	for day := 1; day <= 9; day++ {
		for turn := 1; turn <= day; turn++ {
			ts := time.Date(2026, 3, day, 10, turn, 0, 0, loc).Format(time.RFC3339)
			in.Turns = append(in.Turns, store.ViewTurn{Session: "s1", TS: ts})
		}
	}

	m := Build(in, nil, now, loc)

	// Nearest-rank with ceiling: [2,4,6,8]
	want := []int{2, 4, 6, 8}
	if len(m.Activity.Cuts) != len(want) {
		t.Errorf("Cuts: got %v, want %v (n=9, nearest-rank ceil)", m.Activity.Cuts, want)
	} else {
		for i, c := range m.Activity.Cuts {
			if c != want[i] {
				t.Errorf("Cuts[%d]: got %d, want %d (nearest-rank ceil, not floor)", i, c, want[i])
			}
		}
	}
}

func TestBuildEmptyArchive(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, loc)

	in := &store.ViewInput{
		Facets: []store.Facet{},
		Notes:  []store.ViewNote{},
		Turns:  []store.ViewTurn{},
	}

	m := Build(in, nil, now, loc)

	// First should be empty
	if m.Activity.First != "" {
		t.Errorf("First: got %q, want empty string", m.Activity.First)
	}

	// Days should be empty slice
	if m.Activity.Days == nil {
		t.Errorf("Days: got nil, want []")
	}
	if len(m.Activity.Days) != 0 {
		t.Errorf("Days: got length %d, want 0", len(m.Activity.Days))
	}

	// Exactly one window per kind, all containing now
	k := windowsByKind(m)
	months, quarters, halves, years := k["month"], k["quarter"], k["half"], k["year"]

	if len(months) != 1 {
		t.Errorf("months: got %d, want 1 (Nov)", len(months))
	}
	if len(quarters) != 1 {
		t.Errorf("quarters: got %d, want 1 (Q4)", len(quarters))
	}
	if len(halves) != 1 {
		t.Errorf("halves: got %d, want 1 (H2)", len(halves))
	}
	if len(years) != 1 {
		t.Errorf("years: got %d, want 1 (2026)", len(years))
	}

	// JSON marshal: no null for any slice field
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"days":[]`) {
		t.Errorf("JSON days: want [], got something else")
	}
	if !strings.Contains(s, `"notebooks":[]`) {
		t.Errorf("JSON notebooks: want [], got something else")
	}
	if !strings.Contains(s, `"traits":[]`) {
		t.Errorf("JSON traits: want [], got something else")
	}
	if !strings.Contains(s, `"portraits":[]`) {
		t.Errorf("JSON portraits: want [], got something else")
	}
	// Verify slice fields in activity are not null (busiest is a pointer and can be null per spec)
	if strings.Contains(s, `"cuts":null`) {
		t.Errorf("JSON cuts: want [], got null")
	}
	if strings.Contains(s, `"windows":null`) {
		t.Errorf("JSON windows: want [], got null")
	}
}

// windowsByKind groups the windows by Kind, in build order.
func windowsByKind(m *Model) map[string][]Window {
	k := map[string][]Window{}
	for _, w := range m.Activity.Windows {
		k[w.Kind] = append(k[w.Kind], w)
	}
	return k
}

func windowByLabel(t *testing.T, m *Model, label string) Window {
	t.Helper()
	for _, w := range m.Activity.Windows {
		if w.Label == label {
			return w
		}
	}
	t.Fatalf("no window %q", label)
	return Window{}
}

// P9: `witness cleanup` reclaims transcripts (raw) but keeps notes, so notes can outlive their
// turns. First must reach back to them, or they fall before every window.
func TestActivityFirstCountsNotes(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 5, 0, 0, 0, 0, loc)
	in := &store.ViewInput{
		Turns: []store.ViewTurn{{Session: "s1", TS: "2026-03-01T10:00:00-05:00"}},
		Notes: []store.ViewNote{
			{ID: "n1", Session: "s0", TS: "2026-01-10T10:00:00-05:00"},           // its turn was reclaimed
			{ID: "f1", Session: "file:/doc.md", TS: "2025-12-01T10:00:00-05:00"}, // ingested: never First
			{ID: "bad", Session: "s0", TS: "garbage"},
		},
	}
	m := Build(in, nil, now, loc)

	if m.Activity.First != "2026-01-10" {
		t.Errorf("First: got %q, want 2026-01-10 (the earliest conversation note)", m.Activity.First)
	}
	if want := []Day{{Date: "2026-03-01", Conversations: 1, Turns: 1}}; !reflect.DeepEqual(m.Activity.Days, want) {
		t.Errorf("Days: got %+v, want %+v (only days with turns)", m.Activity.Days, want)
	}
	jan := windowByLabel(t, m, "Jan 2026")
	if jan.Notes != 1 || jan.SpanDays != 22 || jan.Turns != 0 || jan.ActiveDays != 0 {
		t.Errorf("Jan: Notes %d SpanDays %d Turns %d ActiveDays %d, want 1, 22 (Jan 10–31), 0, 0",
			jan.Notes, jan.SpanDays, jan.Turns, jan.ActiveDays)
	}
	if len(m.Activity.Windows) == 0 || m.Activity.Windows[0].Label != "Jan 2026" {
		t.Errorf("first window: want Jan 2026 (December's file: note must not open a window)")
	}
}

func TestActivityNotesWithoutTurns(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 5, 0, 0, 0, 0, loc)
	in := &store.ViewInput{Notes: []store.ViewNote{{ID: "n1", Session: "s0", TS: "2026-03-02T10:00:00-05:00"}}}
	m := Build(in, nil, now, loc)

	if m.Activity.First != "2026-03-02" {
		t.Errorf("First: got %q, want 2026-03-02", m.Activity.First)
	}
	if len(m.Activity.Days) != 0 {
		t.Errorf("Days: got %+v, want none (no turns)", m.Activity.Days)
	}
	if mar := windowByLabel(t, m, "Mar 2026"); mar.Notes != 1 || mar.SpanDays != 4 {
		t.Errorf("Mar: Notes %d SpanDays %d, want 1 and 4 (Mar 2–5)", mar.Notes, mar.SpanDays)
	}
}

func TestActivityClockSkewAllRecordsAfterNow(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 5, 0, 0, 0, 0, loc)

	// All records are after now (April turns, May note)
	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-04-10T10:00:00-05:00"},
			{Session: "s1", TS: "2026-04-15T10:00:00-05:00"},
		},
		Notes: []store.ViewNote{
			{ID: "n1", Session: "s1", TS: "2026-05-01T10:00:00-05:00"},
		},
	}

	m := Build(in, nil, now, loc)

	// First stays the earliest record (April); only the windows' start is clamped to Last
	if m.Activity.First != "2026-04-10" {
		t.Errorf("First: got %q, want 2026-04-10 (earliest record)", m.Activity.First)
	}
	if m.Activity.Last != "2026-03-05" {
		t.Errorf("Last: got %q, want 2026-03-05 (now)", m.Activity.Last)
	}

	// Every kind must have at least one window containing last (2026-03-05)
	k := windowsByKind(m)
	months, quarters, halves, years := k["month"], k["quarter"], k["half"], k["year"]

	if len(months) == 0 {
		t.Errorf("months: got 0, want at least 1 (containing last)")
	}
	if len(quarters) == 0 {
		t.Errorf("quarters: got 0, want at least 1 (containing last)")
	}
	if len(halves) == 0 {
		t.Errorf("halves: got 0, want at least 1 (containing last)")
	}
	if len(years) == 0 {
		t.Errorf("years: got 0, want at least 1 (containing last)")
	}

	// March window (containing last) must exist and be first for each kind
	if len(months) > 0 && months[0].Label != "Mar 2026" {
		t.Errorf("first month: got %s, want Mar 2026 (containing last)", months[0].Label)
	}
	if len(quarters) > 0 && quarters[0].Label != "Q1 2026" {
		t.Errorf("first quarter: got %s, want Q1 2026 (containing last)", quarters[0].Label)
	}
	if len(halves) > 0 && halves[0].Label != "H1 2026" {
		t.Errorf("first half: got %s, want H1 2026 (containing last)", halves[0].Label)
	}
	if len(years) > 0 && years[0].Label != "2026" {
		t.Errorf("first year: got %s, want 2026 (containing last)", years[0].Label)
	}
}
