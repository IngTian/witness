package view

import (
	"encoding/json"
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
	for _, w := range m.Activity.Windows {
		if w.Kind == "month" && w.Label == "Mar 2026" {
			if w.Conversations != 2 {
				t.Errorf("March.Conversations: got %d, want 2 (set union, not sum)", w.Conversations)
			}
		}
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
	var months, quarters, halves, years []Window
	for _, w := range m.Activity.Windows {
		switch w.Kind {
		case "month":
			months = append(months, w)
		case "quarter":
			quarters = append(quarters, w)
		case "half":
			halves = append(halves, w)
		case "year":
			years = append(years, w)
		}
	}

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
	for _, w := range months {
		if w.Label == "Nov 2026" {
			if !w.SoFar {
				t.Errorf("Nov.SoFar: got false, want true")
			}
			if w.SpanDays != 5 {
				t.Errorf("Nov.SpanDays: got %d, want 5 (Nov 1–5)", w.SpanDays)
			}
		}
	}

	// Feb has SpanDays 19 (Feb 10–28, 19 days)
	for _, w := range months {
		if w.Label == "Feb 2026" {
			if w.SpanDays != 19 {
				t.Errorf("Feb.SpanDays: got %d, want 19 (Feb 10–28)", w.SpanDays)
			}
		}
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

	// LongestRun: days 1, 2, 3 = 3 consecutive days
	for _, w := range m.Activity.Windows {
		if w.Kind == "month" && w.Label == "Mar 2026" {
			if w.LongestRun != 3 {
				t.Errorf("March.LongestRun: got %d, want 3 (days 1,2,3)", w.LongestRun)
			}
		}
	}

	// Busiest: day 5 with 3 turns (ties go to earliest, but day 5 wins outright)
	for _, w := range m.Activity.Windows {
		if w.Kind == "month" && w.Label == "Mar 2026" {
			if w.Busiest == nil {
				t.Errorf("March.Busiest: got nil, want day 5")
			} else if w.Busiest.Date != "2026-03-05" {
				t.Errorf("March.Busiest.Date: got %s, want 2026-03-05", w.Busiest.Date)
			} else if w.Busiest.Turns != 3 {
				t.Errorf("March.Busiest.Turns: got %d, want 3", w.Busiest.Turns)
			}
		}
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
	for _, w := range m2.Activity.Windows {
		if w.Kind == "month" && w.Label == "Mar 2026" {
			if w.Busiest == nil {
				t.Errorf("March.Busiest (tie): got nil")
			} else if w.Busiest.Date != "2026-03-01" {
				t.Errorf("March.Busiest.Date (tie): got %s, want 2026-03-01 (earliest)", w.Busiest.Date)
			}
		}
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

func TestActivityNotesOnTurnlessDays(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, loc)

	// P6: window notes are counted directly by parsed ts date, NOT sum over Days.
	// One conversation note on 2026-03-05 with NO turn on that day, and one turn on 03-06.
	in := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-03-06T10:00:00-05:00"}, // turn on 03-06
		},
		Notes: []store.ViewNote{
			{Session: "s1", TS: "2026-03-05T10:00:00-05:00", Observation: "note on turnless day"}, // note on 03-05
		},
	}

	m := Build(in, nil, now, loc)

	// Days should have exactly one entry (03-06), whose Notes == 0
	if len(m.Activity.Days) != 1 {
		t.Fatalf("Days: got %d, want 1 (only 03-06 has turns)", len(m.Activity.Days))
	}
	if m.Activity.Days[0].Date != "2026-03-06" {
		t.Errorf("Days[0].Date: got %s, want 2026-03-06", m.Activity.Days[0].Date)
	}
	if m.Activity.Days[0].Notes != 0 {
		t.Errorf("Days[0].Notes: got %d, want 0 (note is on 03-05, not 03-06)", m.Activity.Days[0].Notes)
	}

	// March window's Notes should be 1 (counted directly from note's ts, not sum of Day.Notes)
	for _, w := range m.Activity.Windows {
		if w.Kind == "month" && w.Label == "Mar 2026" {
			if w.Notes != 1 {
				t.Errorf("March.Notes: got %d, want 1 (note on 03-05, even though no turn that day)", w.Notes)
			}
		}
	}
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
	var months, quarters, halves, years []Window
	for _, w := range m.Activity.Windows {
		switch w.Kind {
		case "month":
			months = append(months, w)
		case "quarter":
			quarters = append(quarters, w)
		case "half":
			halves = append(halves, w)
		case "year":
			years = append(years, w)
		}
	}

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
	if !contains(s, `"days":[]`) {
		t.Errorf("JSON days: want [], got something else")
	}
	if !contains(s, `"notebooks":[]`) {
		t.Errorf("JSON notebooks: want [], got something else")
	}
	if !contains(s, `"traits":[]`) {
		t.Errorf("JSON traits: want [], got something else")
	}
	if !contains(s, `"portraits":[]`) {
		t.Errorf("JSON portraits: want [], got something else")
	}
	// Verify slice fields in activity are not null (busiest is a pointer and can be null per spec)
	if contains(s, `"cuts":null`) {
		t.Errorf("JSON cuts: want [], got null")
	}
	if contains(s, `"windows":null`) {
		t.Errorf("JSON windows: want [], got null")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && search(s, substr))
}

func search(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
