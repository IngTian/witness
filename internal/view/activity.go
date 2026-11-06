package view

import (
	"sort"
	"strconv"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// buildActivity computes all activity metrics from ViewInput: days, cuts, windows and facts.
// Only conversation sources count (Claude Code, OpenCode, Codex); file: sessions are excluded.
// tally carries the trait-derived numbers (buildTraits) — NewTraits/Rewrites per window and
// TotalTraits (counting every notebook's active traits, not just conversation ones).
func buildActivity(in *store.ViewInput, now time.Time, loc *time.Location, tally traitTally) Activity {
	// Parse and bucket turns by local date
	dayMap := make(map[string]*dayBuilder)
	for _, t := range in.Turns {
		if platform.IsCorpusSession(t.Session) {
			continue
		}
		ts, ok := parseTS(t.TS)
		if !ok {
			continue
		}
		local := ts.In(loc)
		date := local.Format("2006-01-02")

		if dayMap[date] == nil {
			dayMap[date] = &dayBuilder{
				date:     date,
				sessions: make(map[string]struct{}),
			}
		}
		dayMap[date].sessions[t.Session] = struct{}{}
		dayMap[date].turns++
	}

	// Parse and bucket notes by local date
	for _, n := range in.Notes {
		if platform.IsCorpusSession(n.Session) {
			continue
		}
		ts, ok := parseTS(n.TS)
		if !ok {
			continue
		}
		local := ts.In(loc)
		date := local.Format("2006-01-02")

		if dayMap[date] == nil {
			dayMap[date] = &dayBuilder{
				date:     date,
				sessions: make(map[string]struct{}),
			}
		}
		dayMap[date].notes++
	}

	// Sort days
	var dates []string
	for date := range dayMap {
		// Only include days with ≥1 conversation turn
		if dayMap[date].turns > 0 {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)

	// Build Days slice (never nil, always [] when empty)
	days := []Day{}
	for _, date := range dates {
		db := dayMap[date]
		days = append(days, Day{
			Date:          date,
			Conversations: len(db.sessions),
			Turns:         db.turns,
			Notes:         db.notes,
		})
	}

	// First is the earliest day with a conversation turn OR note — every dayMap key, since both
	// loops above already dropped file: sessions and bad ts. Turns alone are not enough:
	// `witness cleanup` reclaims transcripts (raw) but keeps notes, so an archive can hold notes
	// whose turns are gone, and a turns-only First would leave them before every window.
	first := ""
	for date := range dayMap {
		if first == "" || date < first {
			first = date
		}
	}
	last := now.In(loc).Format("2006-01-02")

	// Compute Cuts (20/40/60/80th percentiles, nearest-rank)
	cuts := computeCuts(days)

	// Windows run from First's to Last's. An archive with nothing dated yet gets one window per
	// kind, all containing now: the same path started at Last, so there is no second window
	// builder to forget an input (the old empty-archive one was handed no notes).
	// Clamp first to at most last: if all records are after now, first > last, and we must
	// generate at least one window per kind (all containing last).
	start := first
	if start == "" || start > last {
		start = last
	}
	windows := generateWindows(start, last, days, dayMap, in.Notes, loc, tally)

	return Activity{
		First:       first,
		Last:        last,
		Days:        days,
		Cuts:        cuts,
		Windows:     windows,
		TotalTraits: tally.total,
	}
}

type dayBuilder struct {
	date     string
	sessions map[string]struct{}
	turns    int
	notes    int
}

// computeCuts returns the 20/40/60/80th percentiles of Turns over Days, deduplicated.
func computeCuts(days []Day) []int {
	if len(days) == 0 {
		return []int{}
	}

	// Extract turn counts
	var turns []int
	for _, d := range days {
		turns = append(turns, d.Turns)
	}
	sort.Ints(turns)

	// Nearest-rank percentiles
	percentiles := []float64{0.2, 0.4, 0.6, 0.8}
	var cuts []int
	seen := make(map[int]struct{})

	for _, p := range percentiles {
		// Nearest-rank: position = ceil(p * n)
		pos := int(p*float64(len(turns)) + 0.999999) // ceiling
		if pos < 1 {
			pos = 1
		}
		if pos > len(turns) {
			pos = len(turns)
		}
		val := turns[pos-1] // 0-indexed

		if _, ok := seen[val]; !ok {
			cuts = append(cuts, val)
			seen[val] = struct{}{}
		}
	}

	sort.Ints(cuts)
	return cuts
}

// generateWindows builds all windows from First's window to Last's window.
func generateWindows(first, last string, days []Day, dayMap map[string]*dayBuilder, notes []store.ViewNote, loc *time.Location, tally traitTally) []Window {
	firstDate := parseDate(first)
	lastDate := parseDate(last)

	var windows []Window

	// Months
	for ym := monthOf(firstDate); !ym.After(monthOf(lastDate)); ym = ym.AddDate(0, 1, 0) {
		windows = append(windows, buildWindow("month", ym, first, last, days, dayMap, notes, loc, tally))
	}

	// Quarters
	for yq := quarterOf(firstDate); !yq.After(quarterOf(lastDate)); yq = yq.AddDate(0, 3, 0) {
		windows = append(windows, buildWindow("quarter", yq, first, last, days, dayMap, notes, loc, tally))
	}

	// Halves
	for yh := halfOf(firstDate); !yh.After(halfOf(lastDate)); yh = yh.AddDate(0, 6, 0) {
		windows = append(windows, buildWindow("half", yh, first, last, days, dayMap, notes, loc, tally))
	}

	// Years
	for y := yearOf(firstDate); !y.After(yearOf(lastDate)); y = y.AddDate(1, 0, 0) {
		windows = append(windows, buildWindow("year", y, first, last, days, dayMap, notes, loc, tally))
	}

	return windows
}

// buildWindow constructs a Window for the given kind and start date. first is Activity's
// First (or Last for an empty archive), not the first turn day: SpanDays clips to it.
func buildWindow(kind string, start time.Time, first, last string, days []Day, dayMap map[string]*dayBuilder, notes []store.ViewNote, loc *time.Location, tally traitTally) Window {
	var end time.Time
	var label string

	switch kind {
	case "month":
		end = start.AddDate(0, 1, 0).Add(-time.Second) // last moment of month
		label = start.Format("Jan 2006")
	case "quarter":
		end = start.AddDate(0, 3, 0).Add(-time.Second)
		q := (start.Month()-1)/3 + 1
		label = "Q" + strconv.Itoa(int(q)) + start.Format(" 2006")
	case "half":
		end = start.AddDate(0, 6, 0).Add(-time.Second)
		h := (start.Month()-1)/6 + 1
		label = "H" + strconv.Itoa(int(h)) + start.Format(" 2006")
	case "year":
		end = start.AddDate(1, 0, 0).Add(-time.Second)
		label = start.Format("2006")
	}

	startStr := start.Format("2006-01-02")
	endStr := end.Format("2006-01-02")

	// Compute metrics
	sessions := make(map[string]struct{})
	turns := 0
	activeDays := 0
	var busiest *Day

	for _, d := range days {
		if d.Date >= startStr && d.Date <= endStr {
			activeDays++
			if dayMap[d.Date] != nil {
				for s := range dayMap[d.Date].sessions {
					sessions[s] = struct{}{}
				}
			}
			turns += d.Turns

			if busiest == nil || d.Turns > busiest.Turns || (d.Turns == busiest.Turns && d.Date < busiest.Date) {
				busiest = &Day{Date: d.Date, Conversations: d.Conversations, Turns: d.Turns, Notes: d.Notes}
			}
		}
	}

	// Count notes directly by parsed ts date (not sum over Days)
	notesCount := 0
	for _, n := range notes {
		if platform.IsCorpusSession(n.Session) {
			continue
		}
		ts, ok := parseTS(n.TS)
		if !ok {
			continue
		}
		date := ts.In(loc).Format("2006-01-02")
		if date >= startStr && date <= endStr {
			notesCount++
		}
	}

	// Compute SpanDays and SoFar
	spanDays := computeSpanDays(startStr, endStr, first, last)
	soFar := last >= startStr && last <= endStr

	// Compute LongestRun
	longestRun := computeLongestRun(startStr, endStr, days)

	return Window{
		Kind:          kind,
		Start:         startStr,
		End:           endStr,
		Label:         label,
		Conversations: len(sessions),
		Turns:         turns,
		Notes:         notesCount,
		NewTraits:     countDates(tally.arrivals, startStr, endStr),
		Rewrites:      countDates(tally.rewrites, startStr, endStr),
		ActiveDays:    activeDays,
		SpanDays:      spanDays,
		SoFar:         soFar,
		LongestRun:    longestRun,
		Busiest:       busiest,
	}
}

// computeSpanDays returns the window clipped to [First, Last], in days; 0 if disjoint. An
// empty archive passes first == last, which clips every window to now's one day.
func computeSpanDays(windowStart, windowEnd, first, last string) int {
	// Clip [windowStart, windowEnd] to [first, last]
	clipStart := windowStart
	if first > clipStart {
		clipStart = first
	}
	clipEnd := windowEnd
	if last < clipEnd {
		clipEnd = last
	}

	if clipStart > clipEnd {
		return 0 // disjoint
	}

	startDate := parseDate(clipStart)
	endDate := parseDate(clipEnd)
	return int(endDate.Sub(startDate).Hours()/24) + 1
}

// computeLongestRun returns the longest run of consecutive conversation days within [start, end].
func computeLongestRun(start, end string, days []Day) int {
	longest := 0
	current := 0
	var prevDate time.Time

	for _, d := range days {
		if d.Date < start || d.Date > end {
			continue
		}

		date := parseDate(d.Date)
		if current == 0 || date.Sub(prevDate).Hours() == 24 {
			current++
		} else {
			if current > longest {
				longest = current
			}
			current = 1
		}
		prevDate = date
	}

	if current > longest {
		longest = current
	}
	return longest
}

// Helpers
func parseDate(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func monthOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

func quarterOf(t time.Time) time.Time {
	q := ((t.Month() - 1) / 3) * 3
	return time.Date(t.Year(), time.Month(q+1), 1, 0, 0, 0, 0, t.Location())
}

func halfOf(t time.Time) time.Time {
	h := ((t.Month() - 1) / 6) * 6
	return time.Date(t.Year(), time.Month(h+1), 1, 0, 0, 0, 0, t.Location())
}

func yearOf(t time.Time) time.Time {
	return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
}

// countDates counts YYYY-MM-DD dates inside [start, end]. Plain string comparison is safe
// here: both sides are local calendar dates in one fixed-width layout, not RFC3339 instants.
func countDates(dates []string, start, end string) int {
	n := 0
	for _, d := range dates {
		if d >= start && d <= end {
			n++
		}
	}
	return n
}
