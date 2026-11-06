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
	Notebooks []Notebook `json:"notebooks"` // Task 4
	Traits    []Trait    `json:"traits"`    // Task 4; ACTIVE only; index = trait id used everywhere
	Portraits []Portrait `json:"portraits"` // Task 5
}

// Activity measures how much has been recorded: conversation days, turn counts, windows and
// facts. Only conversation sources (Claude Code, OpenCode, Codex) count; sessions from
// `witness ingest` (file:) are excluded from every number.
type Activity struct {
	First       string   `json:"first"`       // YYYY-MM-DD of the first conversation turn; "" if none
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

// Notebook, Trait, Portrait, PortraitFile are placeholders filled by later tasks.
type Notebook struct{}
type Trait struct{}
type Portrait struct{}

// PortraitFile is filled by Task 5. Build accepts a slice of them.
type PortraitFile struct{}

// Build constructs a complete Model from store.ViewInput, computing all activity metrics,
// notebooks, traits and portraits. The snapshot is timestamped at `now` and bucketed by `loc`.
// Later tasks fill Notebooks, Traits and Portraits; this task returns empty slices for them.
func Build(in *store.ViewInput, portraits []PortraitFile, now time.Time, loc *time.Location) *Model {
	return &Model{
		Snapshot:  now.In(loc).Format(time.RFC3339),
		Activity:  buildActivity(in, now, loc),
		Notebooks: []Notebook{},
		Traits:    []Trait{},
		Portraits: []Portrait{},
	}
}
