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

	// Terminal styling. color follows style.go's rule but for STDERR, where the line goes: a
	// terminal, NO_COLOR unset. cols reports the terminal width (nil: unbounded, as in tests) so a
	// line never wraps — a wrapped line breaks the \r redraw into a new line every second.
	color bool
	cols  func() int
}

const progressRedraw = time.Second

// startProgress begins a progress line on stderr. Pass its update method as a distill.ProgressFunc
// and call finish when the pass ends. Never nil.
func startProgress() *progressLine {
	// The clock starts at the first update, not here: setup (opening the store, a migration, listing
	// the work) is not the pass, and counting it once made a 5 s import open on "~3m18s left".
	p := &progressLine{w: os.Stderr, tty: isatty.IsTerminal(os.Stderr.Fd()), minRedraw: 100 * time.Millisecond}
	p.color = p.tty && os.Getenv("NO_COLOR") == ""
	p.cols = func() int {
		if n := termWidth(os.Stderr); n > 0 {
			return n
		}
		return 80
	}
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

// progressView is what both renderings show, computed once per draw.
type progressView struct {
	where       string
	stages      [][2]string // {name, "6/45"}
	done, total int
	open        bool
	elapsed     string
	left        string // "" when there is no estimate yet
}

// view computes the snapshot's numbers. Caller holds mu.
func (p *progressLine) view() progressView {
	v := progressView{where: p.cur.Where}
	for _, s := range p.cur.Stages {
		if s.Total == 0 && !s.Open {
			continue // a stage with nothing to do yet says nothing
		}
		if s.Total > 0 {
			v.stages = append(v.stages, [2]string{s.Name, fmt.Sprintf("%d/%d", s.Done, s.Total)})
		}
		v.done, v.total, v.open = v.done+s.Done, v.total+s.Total, v.open || s.Open
	}
	v.elapsed = shortDuration(time.Since(p.start))
	if worked := p.lastDone - p.base; worked > 0 && v.total > v.done {
		rate := p.lastDoneAt.Sub(p.start) / time.Duration(worked)
		remaining := time.Duration(v.total - v.done)
		// Count down through the call in flight, but never below the calls certainly left after it.
		left := max(rate*remaining-time.Since(p.lastDoneAt), rate*(remaining-1))
		if left >= time.Second { // "≥0s left" says nothing
			v.left = shortDuration(left)
		}
	}
	return v
}

// bound marks an estimate: "≥" while work not yet discovered can only add to it, else "~".
func bound(open bool) string {
	if open {
		return "≥"
	}
	return "~"
}

// render is the plain one-line form, used in logs. Caller holds mu.
func (p *progressLine) render() string {
	v := p.view()
	var parts []string
	if v.where != "" {
		parts = append(parts, v.where)
	}
	for _, st := range v.stages {
		parts = append(parts, st[0]+" "+st[1])
	}
	parts = append(parts, v.elapsed)
	if v.left != "" {
		parts = append(parts, bound(v.open)+v.left+" left")
	}
	return "  " + strings.Join(parts, " · ")
}

// progressBarCells are the bar widths tried, widest first: a narrow terminal gets a shorter bar
// before it loses the bar altogether.
var progressBarCells = []int{22, 14, 8}

var asciiSpinner = []string{"|", "/", "-", "\\"}

// segment is one styled run of the terminal line; plain is what it occupies on screen.
type segment struct{ plain, code string }

// terminalSegments lays the terminal line out:
//
//	⠙ default/biases  ━━━━━━━━━━──────────────  13%  grouping 6/45 · writing merges 0/6  11m40s · ≥1h12m left
//
// cells is the bar width; 0 drops it. Caller holds mu.
func (p *progressLine) terminalSegments(cells int) []segment {
	v := p.view()
	spin, fill, empty := spinnerFrames[p.frame%len(spinnerFrames)], "━", "─"
	if pn := (pen{on: p.color}); !pn.on { // style.go's rule: no color, ASCII decoration too
		spin, fill, empty = asciiSpinner[p.frame%len(asciiSpinner)], "#", "-"
	}
	segs := []segment{{spin, ansiCyan}, {" ", ""}}
	if v.where != "" {
		segs = append(segs, segment{v.where, ansiBold}, segment{"  ", ""})
	}
	if cells > 0 && v.total > 0 {
		filled := v.done * cells / v.total
		segs = append(segs,
			segment{strings.Repeat(fill, filled), ansiCyan},
			segment{strings.Repeat(empty, cells-filled), ansiDim},
			segment{fmt.Sprintf(" %3d%%", v.done*100/v.total), ansiBold},
			segment{"  ", ""})
	}
	for i, st := range v.stages {
		if i > 0 {
			segs = append(segs, segment{" · ", ansiDim})
		}
		segs = append(segs, segment{st[0] + " ", ansiDim}, segment{st[1], ""})
	}
	segs = append(segs, segment{"  ", ""}, segment{v.elapsed, ansiDim})
	if v.left != "" {
		mark := ansiDim
		if v.open {
			mark = ansiYellow
		}
		segs = append(segs, segment{" · ", ansiDim}, segment{bound(v.open), mark}, segment{v.left, ""}, segment{" left", ansiDim})
	}
	return segs
}

func segmentsWidth(segs []segment) int {
	n := 0
	for _, s := range segs {
		n += utf8.RuneCountInString(s.plain)
	}
	return n
}

// draw rewrites the terminal line, fitted to the terminal: the bar shrinks, then goes, then the tail
// is cut, so the line never wraps. Padding is measured on the PLAIN text, so color codes cannot skew
// it. Caller holds mu.
func (p *progressLine) draw() {
	limit := 0
	if p.cols != nil {
		limit = p.cols() - 1 // the last column would wrap on some terminals
	}
	segs := p.terminalSegments(0)
	for _, cells := range progressBarCells {
		if s := p.terminalSegments(cells); limit <= 0 || segmentsWidth(s) <= limit {
			segs = s
			break
		}
	}
	var b strings.Builder
	n := 0
	for _, s := range segs {
		text := s.plain
		if w := utf8.RuneCountInString(text); limit > 0 && n+w > limit {
			text = string([]rune(text)[:max(0, limit-n)])
		}
		n += utf8.RuneCountInString(text)
		text = pen{on: p.color}.paint(s.code, text)
		b.WriteString(text)
	}
	fmt.Fprintf(p.w, "\r%s%s", b.String(), spaces(max(0, p.width-n)))
	p.width = n
	p.lastDraw = time.Now()
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
