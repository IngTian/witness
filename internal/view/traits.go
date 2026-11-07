package view

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Every time in this file is compared PARSED, never as an RFC3339 string: the log stamps UTC,
// notes keep their writer's offset, and "…T07:00:00-05:00" sorts before "…T12:00:00Z" though
// they are the same instant. A time that does not parse is skipped, never fatal.
func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// traitTally is what the activity windows count from the traits. Arrivals and rewrites are
// local dates of conversation notebooks only: a corpus notebook's traits describe the files
// ingested into it, not the person, so they must not inflate "new traits this month".
type traitTally struct {
	total              int      // active traits, every notebook
	arrivals, rewrites []string // YYYY-MM-DD in loc
}

// facetID is a facet's real identity (store rows are replaced on every write, ids are not).
type facetID struct{ lens, dim, key string }

// stamped is one confirmed value at an instant: a log row, or today's stored value.
type stamped struct {
	at time.Time
	v  float64
}

// buildTraits derives every ACTIVE trait and groups them into notebooks and topics. Trait ids
// are indexes into the returned slice, which is ordered exactly as the notebooks list them
// (lens, topic label, arrival, name), so ids read in display order.
func buildTraits(in *store.ViewInput, loc *time.Location) ([]Notebook, []Trait, traitTally) {
	notes := make(map[string]*store.ViewNote, len(in.Notes))
	lensNotes, lensConv := map[string]int{}, map[string]int{}
	for i := range in.Notes {
		n := &in.Notes[i]
		notes[n.ID] = n
		lensNotes[n.Lens]++
		if !platform.IsCorpusSession(n.Session) {
			lensConv[n.Lens]++
		}
	}
	// A lens with no notes at all is NOT a corpus: nothing says its traits came from files.
	corpus := func(lens string) bool { return lensNotes[lens] > 0 && lensConv[lens] == 0 }

	logs := map[facetID][]stamped{}
	for _, e := range in.Log {
		if at, ok := parseTS(e.At); ok {
			id := facetID{e.Lens, e.Dimension, e.Key}
			logs[id] = append(logs[id], stamped{at, e.To})
		}
	}

	// Direct absorbers only (MergedInto names the trait itself, same lens): compaction never
	// groups across lenses, so a same-named ref in another lens is a different trait.
	absorbers := map[facetID][]string{}
	for _, f := range in.Facets {
		if dim, key, ok := store.ParseFacetRef(f.MergedInto); ok {
			id := facetID{f.Lens, dim, key}
			absorbers[id] = append(absorbers[id], f.ArchivedAt)
		}
	}

	traits := []Trait{}
	tally := traitTally{}
	for i := range in.Facets {
		f := &in.Facets[i]
		cur := f.Current()
		if !f.Active() || cur == nil {
			continue // archived, or every version closed: not part of the live profile
		}
		id := facetID{f.Lens, f.Dimension, f.Key}
		t, rewrites := buildTrait(f, cur, notes, logs[id], absorbers[id], in.HighWater, loc)
		traits = append(traits, t)
		if !corpus(f.Lens) {
			if t.Arrival != "" {
				tally.arrivals = append(tally.arrivals, t.Arrival)
			}
			tally.rewrites = append(tally.rewrites, rewrites...)
		}
	}
	tally.total = len(traits)

	sort.SliceStable(traits, func(i, j int) bool {
		a, b := &traits[i], &traits[j]
		for _, c := range [][2]string{
			{a.Lens, b.Lens}, {topicLabel(a.Dimension), topicLabel(b.Dimension)},
			{a.Dimension, b.Dimension}, {a.Arrival, b.Arrival}, {a.Name, b.Name}, {a.Key, b.Key},
		} {
			if c[0] != c[1] {
				return c[0] < c[1]
			}
		}
		return false
	})

	// Sorted traits are contiguous per lens and per dimension, so grouping is one pass.
	notebooks := []Notebook{}
	for id, t := range traits {
		if len(notebooks) == 0 || notebooks[len(notebooks)-1].Name != t.Lens {
			notebooks = append(notebooks, Notebook{Name: t.Lens, Corpus: corpus(t.Lens), Topics: []Topic{}})
		}
		nb := &notebooks[len(notebooks)-1]
		if len(nb.Topics) == 0 || nb.Topics[len(nb.Topics)-1].Key != t.Dimension {
			nb.Topics = append(nb.Topics, Topic{Key: t.Dimension, Label: topicLabel(t.Dimension), Traits: []int{}})
		}
		tp := &nb.Topics[len(nb.Topics)-1]
		tp.Traits = append(tp.Traits, id)
		// Arrivals ascend within a topic and "" sorts first, so the first dated one is earliest.
		if tp.AxisStart == "" && t.Arrival != "" {
			tp.AxisStart = t.Arrival[:len("2006-01")] + "-01"
		}
	}
	return notebooks, traits, tally
}

// buildTrait computes one active trait, and the local dates of its review-written rewrites
// (for the activity windows).
func buildTrait(f *store.Facet, cur *store.FacetVersion, notes map[string]*store.ViewNote,
	rows []stamped, absorbers []string, highWater int64, loc *time.Location) (Trait, []string) {
	day := func(t time.Time) string { return t.In(loc).Format(time.DateOnly) }
	t := Trait{
		Lens: f.Lens, Dimension: f.Dimension, Key: f.Key,
		Name: traitName(f.Key),
		// Trimmed because a whitespace-only claim is "no claim" everywhere else (Gist).
		Summary: strings.TrimSpace(cur.Claim),
		// Decayed at the note high-water, exactly what `witness facets` prints: the GUI and the
		// CLI must never disagree about the same trait.
		Today:  cur.DecayedConfidence(highWater),
		Points: []Point{}, Spans: []Span{}, Weeks: []Week{}, Evidence: []Evidence{},
	}

	// Cited notes over EVERY version (a rewrite does not un-cite what earlier wording rested
	// on), once each, and only those that still exist — a deleted note is not evidence. Only
	// those with a readable ts are placed (Arrival, Weeks, Evidence); every one is counted.
	type dated struct {
		n  *store.ViewNote
		at time.Time
	}
	var cited []dated
	seen := map[string]bool{}
	for _, v := range f.Versions {
		for _, id := range v.BecauseOf {
			n, ok := notes[id]
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			t.Notes++
			if at, ok := parseTS(n.TS); ok {
				cited = append(cited, dated{n, at})
			}
		}
	}

	// Arrival is the first supporting note, not when witness wrote the trait down (a median of
	// 15 days later on the reference clone); ValidFrom only when no dated cited note survives.
	perWeek := map[string]int{}
	var first time.Time
	for i, c := range cited {
		perWeek[weekOf(c.at, loc)]++
		if i == 0 || c.at.Before(first) {
			first = c.at
		}
	}
	if len(cited) > 0 {
		t.Arrival = day(first)
	} else if vf, ok := parseTS(f.Versions[0].ValidFrom); ok {
		t.Arrival = day(vf)
	}
	for w, n := range perWeek {
		t.Weeks = append(t.Weeks, Week{Week: w, N: n})
	}
	sort.Slice(t.Weeks, func(i, j int) bool { return t.Weeks[i].Week < t.Weeks[j].Week })

	sort.SliceStable(cited, func(i, j int) bool {
		if cited[i].n.Poignancy != cited[j].n.Poignancy {
			return cited[i].n.Poignancy > cited[j].n.Poignancy
		}
		return cited[i].at.After(cited[j].at)
	})
	for _, c := range cited[:min(3, len(cited))] {
		t.Evidence = append(t.Evidence, Evidence{Date: day(c.at), Evidence: c.n.Evidence, Note: c.n.Observation})
	}

	// Points: every logged confirmation, in TIME order — log id order is not time order once a
	// long review Run interleaves with a compaction Apply. The open version owns the rows at or
	// after its ValidFrom. Its stored value gets a point of its own (at LastSeen, when it was
	// last set) unless the latest owned row already says that value: `witness merge` adopts
	// the other side's newer confidence WITHOUT logging, and today's stored value
	// must still be the last confirmation drawn.
	pts := append([]stamped(nil), rows...)
	openFrom, openOK := parseTS(cur.ValidFrom)
	var latest *stamped
	for i := range rows {
		r := &rows[i]
		if openOK && !r.at.Before(openFrom) && (latest == nil || !r.at.Before(latest.at)) {
			latest = r // ties: the later-written row wins
		}
	}
	if latest == nil || latest.v != cur.Confidence {
		if ls, ok := parseTS(f.LastSeen); ok {
			pts = append(pts, stamped{ls, cur.Confidence})
		}
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].at.Before(pts[j].at) })
	for _, p := range pts {
		t.Points = append(t.Points, Point{At: day(p.at), Value: p.v})
	}

	// A version is compaction-written when its ValidFrom is the instant compaction archived a
	// direct absorber: compact.go stamps both with one nowStr. Derived read-only, so the core
	// needs no marker column.
	var formedAt []time.Time
	for _, a := range absorbers {
		if at, ok := parseTS(a); ok {
			formedAt = append(formedAt, at)
		}
	}
	if len(absorbers) > 0 {
		t.Formed = &Formed{Merged: len(absorbers)}
		if len(formedAt) > 0 {
			first := formedAt[0]
			for _, at := range formedAt[1:] {
				if at.Before(first) {
					first = at
				}
			}
			t.Formed.Date = day(first)
		}
	}

	type span struct {
		from, to time.Time
		v        float64
	}
	var spans []span
	var rewrites []string
	for i := range f.Versions {
		v := &f.Versions[i]
		from, fromOK := parseTS(v.ValidFrom)
		if i > 0 && !(fromOK && containsInstant(formedAt, from)) {
			t.Rewrites++
			if fromOK {
				rewrites = append(rewrites, day(from))
			}
		}
		if v.ValidTo == "" {
			continue
		}
		to, toOK := parseTS(v.ValidTo)
		// A zero-length version held its value for no time: a multi-window review Run stamps
		// every window with one nowStr, so a facet rewritten in two windows gets a [T,T)
		// version whose value is already a log point. It is not a span.
		if !fromOK || !toOK || !from.Before(to) || loggedWithin(rows, from, to) {
			continue
		}
		spans = append(spans, span{from, to, v.Confidence})
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })
	for _, s := range spans {
		t.Spans = append(t.Spans, Span{From: day(s.from), To: day(s.to), Value: s.v})
	}

	// Confirmed values only — what witness concluded at each review, never a decayed one, or
	// decay alone would put a ↓ on almost every trait. In TIME order: a span (keyed by its
	// start) can sit between logged points, e.g. a version `witness merge` appended unlogged
	// between two logged ones. Spans go first on ties — appended first, and the sort is stable.
	seq := make([]stamped, 0, len(spans)+len(pts))
	for _, s := range spans {
		seq = append(seq, stamped{s.from, s.v})
	}
	seq = append(seq, pts...)
	sort.SliceStable(seq, func(i, j int) bool { return seq[i].at.Before(seq[j].at) })
	var confirmed []float64
	for _, c := range seq {
		confirmed = append(confirmed, c.v)
	}
	if len(confirmed) > 0 {
		t.TrendFrom = confirmed[0]
	}
	t.Trend = trend(confirmed)
	for i := 1; i < len(confirmed); i++ {
		if confirmed[i] != confirmed[i-1] {
			t.Changes++
		}
	}
	return t, rewrites
}

// loggedWithin reports a log row in [from, to). Half-open on purpose: the rewrite that CLOSES
// a version is stamped with that version's ValidTo (reviewer.go writes ValidTo, the next
// ValidFrom and the row from one nowStr) but records the successor's value. Counting it would
// drop the dashed span of every pre-log version the first logged rewrite closed.
func loggedWithin(rows []stamped, from, to time.Time) bool {
	for _, r := range rows {
		if !r.at.Before(from) && r.at.Before(to) {
			return true
		}
	}
	return false
}

func containsInstant(ts []time.Time, t time.Time) bool {
	for _, x := range ts {
		if x.Equal(t) {
			return true
		}
	}
	return false
}

// trend compares the first and last confirmed values. Confidence is stored at 2dp
// (clampConf), so the comparison is in whole points: float subtraction makes 0.60−0.55 =
// 0.0499…, which a raw `< 0.05` would call level while 0.75−0.70 = 0.0500…04 reads up.
func trend(confirmed []float64) string {
	if len(confirmed) <= 1 {
		return "only"
	}
	points := math.Round((confirmed[len(confirmed)-1] - confirmed[0]) * 100)
	switch {
	case math.Abs(points) < 5:
		return "level"
	case points > 0:
		return "up"
	default:
		return "down"
	}
}

// weekOf is the Monday (local) starting t's week.
func weekOf(t time.Time, loc *time.Location) string {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()-(int(l.Weekday())+6)%7, 0, 0, 0, 0, loc).Format(time.DateOnly)
}

// traitName is the on-screen trait: key with spaces, first rune upper-cased, nothing else.
func traitName(key string) string {
	s := strings.ReplaceAll(key, "_", " ")
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError { // empty, or not UTF-8: leave it exactly as stored
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// topicLabel is a dimension's plain-words name on the page: the two default-lens keys that read
// as jargon get a phrase, every other key shows with its underscores as spaces.
func topicLabel(dim string) string {
	switch dim {
	case "biases":
		return "blind spots"
	case "workstyle":
		return "how you work"
	}
	return strings.ReplaceAll(dim, "_", " ")
}
