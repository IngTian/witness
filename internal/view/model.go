// Package view builds read-only HTML snapshots of the witness archive. It transforms store
// rows into a human-readable JSON model (Model) that the browser renders. The builder is
// pure computation: it reads ViewInput once, computes every number the UI shows, and returns
// a snapshot. The HTML never recomputes activity metrics or reruns queries — the Go side
// does all the work, the client only draws.
//
// A snapshot is timestamped and local: activity windows and day boundaries are computed in
// the machine's time zone (loc), and the snapshot records the instant it was created. The
// view is read-only; editing or curating the archive requires other commands.
package view

import (
	"time"

	"github.com/IngTian/witness/internal/store"
)

// Model is the complete snapshot data for the read-only HTML view. Every field is non-nil:
// empty slices are `[]`, never `null`, so the client never checks existence.
type Model struct {
	Snapshot  string     `json:"snapshot"` // now, RFC3339 in loc
	Activity  Activity   `json:"activity"`
	Notebooks []Notebook `json:"notebooks"`          // lenses with ≥1 active trait, by name
	Traits    []Trait    `json:"traits"`             // ACTIVE only; index = trait id used everywhere
	Portraits []Portrait `json:"portraits"`          // Task 5
	E2EStart  string     `json:"e2eStart,omitempty"` // test hook only: "changed" to land on the second page
}

// Activity measures how much has been recorded: conversation days, turn counts, windows and
// facts. Only conversation sources (Claude Code, OpenCode, Codex) count; sessions from
// `witness ingest` (file:) are excluded from every number.
type Activity struct {
	First       string   `json:"first"`       // YYYY-MM-DD of the first conversation turn or note; "" if none
	Last        string   `json:"last"`        // YYYY-MM-DD of now
	Days        []Day    `json:"days"`        // days with ≥1 conversation turn, ascending
	Cuts        []int    `json:"cuts"`        // ≤4 ascending distinct turn thresholds; shade = 1 + count(cut < turns)
	Windows     []Window `json:"windows"`     // per kind, ascending, from First's window to Last's (only Last's if First == "")
	TotalTraits int      `json:"totalTraits"` // active traits, all notebooks
}

// Day is one calendar day (local midnight to midnight) with ≥1 conversation turn. Days
// without any conversation turn are not represented.
type Day struct {
	Date          string `json:"date"` // YYYY-MM-DD
	Conversations int    `json:"conv"` // distinct sessions with a turn on this day
	Turns         int    `json:"turns"`
	Notes         int    `json:"notes"` // notes whose parsed ts falls on this day
}

// Window is one calendar-aligned time span: a month (Jan 1 – Jan 31), quarter (Q1 = Jan–Mar),
// half (H1 = Jan–Jun) or year. Metrics are computed over conversation sources only.
type Window struct {
	Kind          string `json:"kind"`  // "month" | "quarter" | "half" | "year"
	Start         string `json:"start"` // inclusive calendar bounds, YYYY-MM-DD
	End           string `json:"end"`
	Label         string `json:"label"` // "Nov 2026", "Q4 2026", "H2 2026", "2026"
	Conversations int    `json:"conv"`  // DISTINCT sessions with a turn in the window
	Turns         int    `json:"turns"`
	Notes         int    `json:"notes"`
	NewTraits     int    `json:"newTraits"` // Task 4 wires this
	Rewrites      int    `json:"rewrites"`  // Task 4 wires this
	ActiveDays    int    `json:"activeDays"`
	SpanDays      int    `json:"spanDays"`   // the window clipped to [First, Last], in days; 0 if disjoint
	SoFar         bool   `json:"soFar"`      // the window contains Last
	LongestRun    int    `json:"longestRun"` // consecutive conversation days within the window
	Busiest       *Day   `json:"busiest"`    // most turns; ties → earliest; nil if none
}

// Notebook is one lens with ≥1 active trait, shown by its name. Notebooks sort by name.
type Notebook struct {
	Name   string  `json:"name"`
	Corpus bool    `json:"corpus"` // every note of this lens comes from a file: session (and it has ≥1 note)
	Topics []Topic `json:"topics"` // sorted by label
}

// Topic is one dimension of a notebook, under its plain-words label.
type Topic struct {
	Key       string `json:"key"`       // the dimension
	Label     string `json:"label"`     // §3 vocabulary
	Traits    []int  `json:"traits"`    // trait ids, by Arrival ascending, then Name
	AxisStart string `json:"axisStart"` // YYYY-MM-01 of the topic's earliest Arrival
}

// Trait is one ACTIVE facet as page 2 shows it. Every number is computed here so the client
// never re-derives one; see traits.go for how each is defined.
type Trait struct {
	Lens      string     `json:"lens"`
	Dimension string     `json:"dim"`
	Key       string     `json:"key"`
	Name      string     `json:"name"`      // key, underscores → spaces, first rune upper-cased
	Summary   string     `json:"summary"`   // current version's Claim ("" → client says "No short summary yet.")
	Notes     int        `json:"notes"`     // distinct cited note ids that exist, over ALL versions
	Arrival   string     `json:"arrival"`   // YYYY-MM-DD (loc) of the earliest cited existing note; else first version's ValidFrom
	Today     float64    `json:"today"`     // Current().DecayedConfidence(in.HighWater) — what `witness facets` prints
	Points    []Point    `json:"points"`    // solid confirmations, ascending
	Spans     []Span     `json:"spans"`     // dashed "final value of this rewrite" segments, ascending
	Trend     string     `json:"trend"`     // "up" | "down" | "level" | "only"
	TrendFrom float64    `json:"trendFrom"` // the first confirmed value
	Rewrites  int        `json:"rewrites"`  // review-written versions after the first
	Changes   int        `json:"changes"`   // confirmed values that differ from the previous confirmed value
	Formed    *Formed    `json:"formed"`    // non-nil iff ≥1 facet has MergedInto == this trait
	Weeks     []Week     `json:"weeks"`     // sparse: weeks (Monday start, loc) with ≥1 cited note
	Evidence  []Evidence `json:"evidence"`  // ≤3 strongest cited notes
}

// Point is one confirmed value on the solid confidence line.
type Point struct {
	At    string  `json:"at"` // YYYY-MM-DD (loc)
	Value float64 `json:"v"`
}

// Span is a version from before the confidence log: only its final value is known, so it is
// drawn dashed across its whole lifetime rather than as a point on a date it may not have held.
type Span struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Value float64 `json:"v"`
}

// Formed says a trait was made by compaction: the earliest ArchivedAt among its direct
// absorbers, and their count.
type Formed struct {
	Date   string `json:"date"`
	Merged int    `json:"merged"`
}

// Week is one Monday-start local week with ≥1 cited note.
type Week struct {
	Week string `json:"w"`
	N    int    `json:"n"`
}

// Evidence is THE note allow-list: the only note text in the snapshot is these three fields
// of each active trait's ≤3 strongest notes. Adding a field here widens what leaves the
// archive; TestEvidenceOrderAndAllowList pins the key set.
type Evidence struct {
	Date     string `json:"date"`
	Evidence string `json:"evidence"`
	Note     string `json:"note"`
}

// PortraitFile is one portrait as the command found it on disk, with its stored sources. Build
// never reads a file or the database itself: the caller gathers these, so Build stays pure.
type PortraitFile struct {
	ID        string                // store.ProfileUnified or a lens name
	Markdown  string                // the file, byte for byte
	Written   time.Time             // file mtime
	Stale     bool                  // Summarizer.Stale: `witness profile` would rewrite it
	Sources   store.PortraitSources // CurrentPortraitSources (merges followed, retired dropped)
	SourcesOK bool                  // false: no sources for this exact text
}

// Portrait is one portrait as the reading column draws it. Sentence numbers (Sentence.N, the
// index into Links, Keyword.Sentences) are sentence.SplitSentences' numbering of the whole
// file — the numbering the stored links were written against.
type Portrait struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`    // "overall" (unified) | "notebook" | "other" (a corpus notebook)
	Title    string    `json:"title"`   // overall: its leading heading ("" if none); else the lens name
	Written  string    `json:"written"` // YYYY-MM-DD (loc) of the file mtime
	Stale    bool      `json:"stale"`
	Sources  bool      `json:"sources"`  // false → "Sources for this portrait aren't recorded yet."
	Blocks   []Block   `json:"blocks"`   // in file order
	Links    [][]int   `json:"links"`    // one per sentence: ≤3 active trait ids, direct ones, else partial ones
	Keywords []Keyword `json:"keywords"` // ≤8; [] without sources
}

// Block is a heading, a paragraph or a list item. Only headings carry Text; the other two
// carry their sentences, so every word of them is clickable.
type Block struct {
	Kind      string     `json:"kind"`      // "h" | "p" | "li"
	Text      string     `json:"text"`      // headings only
	Sentences []Sentence `json:"sentences"` // p / li
}

// Sentence is one sentence, split into runs so bold lead sentences stay bold.
type Sentence struct {
	N    int   `json:"n"` // global sentence number
	Runs []Run `json:"runs"`
}

// Run is a stretch of a sentence that is all bold or all plain.
type Run struct {
	Text string `json:"t"`
	Bold bool   `json:"b"`
}

// Keyword is a chip: clicking it highlights these sentence numbers.
type Keyword struct {
	Word      string `json:"word"`
	Sentences []int  `json:"sentences"`
}

// Build constructs a complete Model from store.ViewInput, computing all activity metrics,
// notebooks, traits and portraits. The snapshot is timestamped at `now` and bucketed by `loc`.
// Traits come first because the activity windows count their arrivals and rewrites. E2EStart is
// never set; only the e2e test sets it.
func Build(in *store.ViewInput, portraits []PortraitFile, now time.Time, loc *time.Location) *Model {
	notebooks, traits, tally := buildTraits(in, loc)
	return &Model{
		Snapshot:  now.In(loc).Format(time.RFC3339),
		Activity:  buildActivity(in, now, loc, tally),
		Notebooks: notebooks,
		Traits:    traits,
		Portraits: buildPortraits(portraits, notebooks, traits, loc),
		// E2EStart: deliberately omitted; set only in e2e_test.go
	}
}
