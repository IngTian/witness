package distill

// Progress is a snapshot of a long foreground pass, for a caller that shows it. The passes that
// need it make tens to hundreds of model calls — measured: compacting one 2,039-facet lens is 45
// grouping calls at ~2 minutes each before a single merge is written — and a prompt that sits
// silent for an hour is indistinguishable from a hang.
type Progress struct {
	Where  string // what is being worked on: "default/biases", a lens, a session
	Stages []Stage
}

// Stage counts one kind of work, usually one kind of model call.
type Stage struct {
	Name        string
	Done, Total int
	// Open means Total can still grow — work is discovered as the pass goes (compaction learns a
	// dimension's merges only after grouping it) — so any estimate of time left is a floor.
	Open bool
}

// ProgressFunc receives snapshots. nil means silent. A pass that works in parallel calls it from
// several goroutines, so an implementation must be safe for concurrent use.
type ProgressFunc func(Progress)
