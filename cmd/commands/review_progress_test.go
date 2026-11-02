package commands

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
)

// Each phase gets its own line, and so its own clock: a line estimates time left from when it
// started, so one carried from lens to lens (or from clustering into verifying) would compute the
// second phase's estimate from the first phase's elapsed time.
func TestPhaseProgressStartsALinePerPhase(t *testing.T) {
	var bufs []*bytes.Buffer
	p := &phaseProgress{start: func() *progressLine {
		b := &bytes.Buffer{}
		bufs = append(bufs, b)
		return &progressLine{w: b, start: time.Now()} // not a terminal: one plain line per Where
	}}
	fold := func(where string, done int) distill.Progress {
		return distill.Progress{Where: where, Stages: []distill.Stage{{Name: "folding windows", Done: done, Total: 3}}}
	}
	p.update(fold("default", 0))
	p.update(fold("default", 1))
	p.update(fold("career", 0))
	p.update(distill.Progress{Where: "career", Stages: []distill.Stage{
		{Name: "clustering", Done: 0, Total: 14}, {Name: "verifying arcs", Open: true}}})
	p.update(distill.Progress{Where: "career", Stages: []distill.Stage{
		{Name: "clustering", Done: 1, Total: 14}, {Name: "verifying arcs", Open: true}}})
	p.update(distill.Progress{Where: "career", Stages: []distill.Stage{{Name: "verifying arcs", Done: 0, Total: 5}}})
	p.finish()

	want := []string{"default · folding windows 0/3", "career · folding windows 0/3", "career · clustering 0/14", "career · verifying arcs 0/5"}
	if len(bufs) != len(want) {
		t.Fatalf("want %d lines (one per lens and phase), started %d", len(want), len(bufs))
	}
	for i, w := range want {
		if out := bufs[i].String(); strings.Count(out, "\n") != 1 || !strings.Contains(out, w) {
			t.Errorf("line %d: want one line with %q, got %q", i, w, out)
		}
	}
}

// On a terminal the line being replaced must be finished first: it is cleared, and its once-a-second
// redraw stops, or two lines would take turns overwriting the same row.
func TestPhaseProgressFinishesTheLineItReplaces(t *testing.T) {
	var lines []*progressLine
	var bufs []*bytes.Buffer
	p := &phaseProgress{start: func() *progressLine {
		b := &bytes.Buffer{}
		l := &progressLine{w: b, tty: true, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
		go l.tick()
		lines, bufs = append(lines, l), append(bufs, b)
		return l
	}}
	p.update(distill.Progress{Where: "default", Stages: []distill.Stage{{Name: "folding windows", Total: 2}}})
	p.update(distill.Progress{Where: "career", Stages: []distill.Stage{{Name: "folding windows", Total: 2}}})
	l := lines[0]
	l.mu.Lock()
	cleared := strings.HasSuffix(bufs[0].String(), "\r") && l.width == 0
	l.mu.Unlock()
	select {
	case <-l.done:
	default:
		t.Error("the replaced line's redraw is still running")
	}
	if !cleared {
		t.Errorf("the replaced line was not cleared before the next one drew: %q", bufs[0].String())
	}
	p.finish()
}
