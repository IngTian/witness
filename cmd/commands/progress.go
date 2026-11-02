package commands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-isatty"

	"github.com/IngTian/witness/internal/distill"
)

// progressLine is the determinate sibling of the spinner (spinner.go): for a foreground pass that
// knows how much work it has — "grouping 11/45 · writing merges 2/12 · 24m12s · ≥1h35m left" —
// where the spinner only says "still going". Same contract: on stderr, so stdout keeps only the
// command's real output.
//
// On a terminal it is one line rewritten in place, REDRAWN EVERY SECOND even when no work has
// finished: a single model call can take minutes, and a clock that stops for two minutes reads as a
// hang (seen on a real compaction run). Anywhere else (a pipe, a log) it prints one plain line each
// time the Where changes, so hundreds of calls do not become hundreds of lines and no carriage
// return lands in a log. It pads with spaces rather than an erase-line escape, which an old Windows
// console prints as garbage.
type progressLine struct {
	mu    sync.Mutex
	w     io.Writer
	tty   bool
	start time.Time
	cur   distill.Progress
	seen  bool
	frame int
	width int    // runes in the last line drawn, to blank its tail
	last  string // non-terminal: the Where last printed
	stop  chan struct{}
	done  chan struct{}

	// The estimate's inputs. The rate is measured at the LAST COMPLETION, from work done in this
	// run only (base is what was already done at the first update — a resumed pass must not count
	// its head start as instant), and the time since that completion is then counted down. Plain
	// elapsed/done instead made the estimate RISE between completions — it read as slowing down.
	base       int
	lastDone   int
	lastDoneAt time.Time
	lastDraw   time.Time
	minRedraw  time.Duration // 0 in tests; a 20k-input pass must not mean 20k terminal writes
}

const progressRedraw = time.Second

// startProgress begins a progress line on stderr. Pass its update method as a distill.ProgressFunc
// and call finish when the pass ends. Never nil.
func startProgress() *progressLine {
	// The clock starts at the first update, not here: setup (opening the store, a migration, listing
	// the work) is not the pass, and counting it once made a 5 s import open on "~3m18s left".
	p := &progressLine{w: os.Stderr, tty: isatty.IsTerminal(os.Stderr.Fd()), minRedraw: 100 * time.Millisecond}
	if p.tty {
		p.stop, p.done = make(chan struct{}), make(chan struct{})
		go p.tick()
	}
	return p
}

func (p *progressLine) tick() {
	defer close(p.done)
	t := time.NewTicker(progressRedraw)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			if p.seen {
				p.frame++
				p.draw()
			}
			p.mu.Unlock()
		}
	}
}

// update records a snapshot and draws it. Safe for concurrent use.
func (p *progressLine) update(pr distill.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	first := !p.seen
	p.cur, p.seen = pr, true
	now := time.Now()
	done := 0
	for _, st := range pr.Stages {
		done += st.Done
	}
	if first {
		if p.start.IsZero() {
			p.start = now
		}
		p.base = done
		p.lastDone, p.lastDoneAt = done, p.start
	} else if done != p.lastDone {
		p.lastDone, p.lastDoneAt = done, now
	}
	if p.tty {
		if p.minRedraw == 0 || now.Sub(p.lastDraw) >= p.minRedraw {
			p.draw()
		}
		return
	}
	if first || pr.Where != p.last {
		fmt.Fprintln(p.w, p.render())
		p.last = pr.Where
	}
}

// draw rewrites the terminal line. Caller holds mu.
func (p *progressLine) draw() {
	line := spinnerFrames[p.frame%len(spinnerFrames)] + " " + p.render()
	n := utf8.RuneCountInString(line)
	fmt.Fprintf(p.w, "\r%s%s", line, spaces(max(0, p.width-n)))
	p.width = n
	p.lastDraw = time.Now()
}

// render is the line without the spinner frame. Caller holds mu.
func (p *progressLine) render() string {
	var parts []string
	if p.cur.Where != "" {
		parts = append(parts, p.cur.Where)
	}
	done, total, open := 0, 0, false
	for _, s := range p.cur.Stages {
		if s.Total == 0 && !s.Open {
			continue // a stage with nothing to do yet says nothing
		}
		if s.Total > 0 {
			parts = append(parts, fmt.Sprintf("%s %d/%d", s.Name, s.Done, s.Total))
		}
		done, total, open = done+s.Done, total+s.Total, open || s.Open
	}
	elapsed := time.Since(p.start)
	parts = append(parts, shortDuration(elapsed))
	if worked := p.lastDone - p.base; worked > 0 && total > done {
		rate := p.lastDoneAt.Sub(p.start) / time.Duration(worked)
		remaining := time.Duration(total - done)
		// Count down through the call in flight, but never below the calls certainly left after it.
		left := max(rate*remaining-time.Since(p.lastDoneAt), rate*(remaining-1))
		if left >= time.Second { // "≥0s left" says nothing
			bound := "~"
			if open { // work not yet discovered can only add to it
				bound = "≥"
			}
			parts = append(parts, bound+shortDuration(left)+" left")
		}
	}
	return "  " + strings.Join(parts, " · ")
}

// finish stops the redraw and clears the line, so whatever is printed next starts clean.
// Idempotent; blocks until the redraw goroutine has stopped writing.
func (p *progressLine) finish() {
	if p.stop != nil {
		select {
		case <-p.stop:
		default:
			close(p.stop)
			<-p.done
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && p.width > 0 {
		fmt.Fprintf(p.w, "\r%s\r", spaces(p.width))
		p.width = 0
	}
}

// shortDuration prints 45s, 6m12s, 1h05m — the precision a person waiting on a model wants.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
