package commands

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/distill"
)

func compactSnap(where string, grouped, groups, merged, merges int) distill.Progress {
	return distill.Progress{Where: where, Stages: []distill.Stage{
		{Name: "grouping", Done: grouped, Total: groups},
		{Name: "writing merges", Done: merged, Total: merges, Open: grouped < groups},
	}}
}

// On a terminal the line is rewritten in place and says how much is left; the estimate is a
// floor ("≥") while any stage is Open, because work not yet discovered can only add calls.
func TestProgressLineOnATerminal(t *testing.T) {
	var buf bytes.Buffer
	p := &progressLine{w: &buf, tty: true, start: time.Now().Add(-10 * time.Minute)}

	p.update(compactSnap("default/biases", 0, 45, 0, 0)) // the pass begins: nothing done, no estimate yet
	if strings.Contains(buf.String(), "left") {
		t.Errorf("no work done yet, so no estimate: %q", buf.String())
	}
	buf.Reset()
	p.update(compactSnap("default/biases", 5, 45, 0, 0)) // 5 calls in 10 minutes: 2m each, 40 to go
	first := buf.String()
	for _, want := range []string{"\r", "default/biases", "grouping 5/45", "≥1h20m left"} {
		if !strings.Contains(first, want) {
			t.Errorf("first line missing %q: %q", want, first)
		}
	}
	if strings.Contains(first, "writing merges") {
		t.Errorf("a stage with nothing found yet must not print 0/0: %q", first)
	}

	buf.Reset()
	p.update(compactSnap("default/goals", 45, 45, 1, 3))
	second := buf.String()
	if !strings.Contains(second, "writing merges 1/3") || !strings.Contains(second, "~") || strings.Contains(second, "≥") {
		t.Errorf("once nothing is Open the estimate is no longer a floor: %q", second)
	}

	buf.Reset()
	p.update(compactSnap("x", 45, 45, 3, 3))
	third := buf.String()
	if strings.Contains(third, "left") {
		t.Errorf("nothing is left, so no estimate: %q", third)
	}
	if !strings.HasSuffix(third, " ") {
		t.Errorf("a shorter line must blank the longer one's tail with spaces: %q", third)
	}

	buf.Reset()
	p.finish()
	if out := buf.String(); !strings.HasPrefix(out, "\r") || !strings.HasSuffix(out, "\r") || strings.TrimSpace(out) != "" {
		t.Errorf("finish must clear the line: %q", out)
	}
}

// Anywhere else (a pipe, a log) there is no carriage return and one line per Where, so hundreds
// of calls do not become hundreds of lines.
func TestProgressLineInALog(t *testing.T) {
	var buf bytes.Buffer
	p := &progressLine{w: &buf, tty: false, start: time.Now()}
	p.update(compactSnap("default/biases", 0, 45, 0, 0))
	p.update(compactSnap("default/biases", 1, 45, 0, 0))
	p.update(compactSnap("default/biases", 2, 45, 0, 0))
	p.update(compactSnap("default/feedback", 11, 45, 0, 0))
	p.finish()
	out := buf.String()
	if strings.Contains(out, "\r") || strings.Count(out, "\n") != 2 {
		t.Errorf("want 2 plain lines, one per Where:\n%q", out)
	}
}

// An empty Where (a pass that has not reached its first lens yet) must not print a leading
// separator — the first real run showed "   · grouping 0/4".
func TestProgressLineWithoutWhere(t *testing.T) {
	var buf bytes.Buffer
	p := &progressLine{w: &buf, tty: false, start: time.Now()}
	p.update(compactSnap("", 0, 4, 0, 0))
	if out := buf.String(); !strings.HasPrefix(out, "  grouping 0/4") {
		t.Errorf("no Where, no separator: %q", out)
	}
}

// The estimate. Plain elapsed/done made it RISE between completions (it read as slowing down),
// counted setup and a resumed pass's head start as instant work, and printed "≥0s left".
func TestProgressEstimate(t *testing.T) {
	ten := time.Now().Add(-10 * time.Minute)

	// A resumed pass that starts at 30/40 has done no work in THIS run: no estimate, not "~0s".
	p := &progressLine{w: &bytes.Buffer{}, start: ten}
	p.update(compactSnap("lens", 30, 40, 0, 0))
	if out := p.render(); strings.Contains(out, "left") {
		t.Errorf("a head start is not a measured rate: %q", out)
	}
	// Then one call in this run took the whole 10 minutes: 9 left is ~1h30m — not 9 × 10m/31.
	p.update(compactSnap("lens", 31, 40, 0, 0))
	if out := p.render(); !strings.Contains(out, "1h30m left") {
		t.Errorf("the rate must come from this run's work only: %q", out)
	}

	// 4 calls done in 10 minutes (2m30s each), 36 left: ~1h30m — counted DOWN through the call in
	// flight, never below the 35 calls certainly left after it.
	p = &progressLine{w: &bytes.Buffer{}, start: ten}
	p.update(compactSnap("lens", 0, 40, 0, 0))
	p.update(compactSnap("lens", 4, 40, 0, 0))
	if out := p.render(); !strings.Contains(out, "1h30m left") {
		t.Errorf("right after a completion: %q", out)
	}
	ago := func(d time.Duration) { p.start, p.lastDoneAt = p.start.Add(-d), p.lastDoneAt.Add(-d) }
	ago(time.Minute) // a minute into the next call
	if out := p.render(); !strings.Contains(out, "1h29m left") {
		t.Errorf("the estimate must count down between completions: %q", out)
	}
	ago(time.Hour) // a call far slower than the rest
	if out := p.render(); !strings.Contains(out, "1h27m left") {
		t.Errorf("never below the calls certainly left (35 × 2m30s): %q", out)
	}

	// Under a second left says nothing.
	p = &progressLine{w: &bytes.Buffer{}, start: time.Now().Add(-time.Second)}
	p.update(compactSnap("lens", 0, 3, 0, 0))
	p.update(compactSnap("lens", 2, 3, 0, 0)) // 0.5s per call, one left
	if out := p.render(); strings.Contains(out, "left") {
		t.Errorf("no sub-second estimate: %q", out)
	}
}

// A pass with tens of thousands of inputs must not mean tens of thousands of terminal writes.
func TestProgressLineThrottlesRedraws(t *testing.T) {
	var buf bytes.Buffer
	p := &progressLine{w: &buf, tty: true, start: time.Now(), minRedraw: time.Hour}
	for i := 0; i < 1000; i++ {
		p.update(compactSnap("lens", i, 1000, 0, 0))
	}
	if n := strings.Count(buf.String(), "\r"); n != 1 {
		t.Errorf("1,000 updates inside one redraw interval drew %d times; want 1 (the ticker catches up)", n)
	}
}

// The terminal form: a bar and a percentage, styled only when color is on, fitted so it never
// wraps — a wrapped line turns every \r redraw into a new line.
func TestProgressBarOnATerminal(t *testing.T) {
	var buf bytes.Buffer
	p := &progressLine{w: &buf, tty: true, start: time.Now()}
	p.update(compactSnap("default/biases", 11, 44, 0, 0)) // 25%
	out := buf.String()
	if !strings.Contains(out, strings.Repeat("#", 5)+strings.Repeat("-", 17)) || !strings.Contains(out, " 25%") {
		t.Errorf("want a 22-cell ASCII bar (no color: style.go plain rule) a quarter full and 25%%: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("no color unless color is on: %q", out)
	}
	for _, tc := range []struct{ done, filled int }{{22, 11}, {44, 22}} {
		buf.Reset()
		p.update(compactSnap("default/biases", tc.done, 44, 0, 0))
		if got := strings.Count(buf.String(), "#"); got != tc.filled {
			t.Errorf("%d/44: %d cells filled, want %d: %q", tc.done, got, tc.filled, buf.String())
		}
	}

	buf.Reset()
	p.color = true
	p.update(compactSnap("default/biases", 12, 44, 0, 0))
	styled := buf.String()
	if !strings.Contains(styled, "━") || strings.Contains(styled, "#") {
		t.Errorf("color on: the box-drawing bar, not ASCII: %q", styled)
	}
	for _, code := range []string{ansiCyan, ansiBold, ansiDim} {
		if !strings.Contains(styled, code) {
			t.Errorf("color on: missing %q in %q", code, styled)
		}
	}
	// Padding is measured on the plain text: a colored line must not count escape bytes as width.
	if p.width != utf8.RuneCountInString(regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(strings.TrimPrefix(styled, "\r"), "")) {
		t.Errorf("width %d counted escape bytes", p.width)
	}
}

func TestProgressLineFitsTheTerminal(t *testing.T) {
	wide := &progressLine{w: &bytes.Buffer{}, tty: true, start: time.Now()}
	wide.update(compactSnap("default/biases", 6, 45, 0, 0))
	full := wide.width

	// Too narrow even without the bar: cut, never wrap, and the line still starts where it should.
	var tiny bytes.Buffer
	pt := &progressLine{w: &tiny, tty: true, start: time.Now(), cols: func() int { return 20 }}
	pt.update(compactSnap("default/biases", 6, 45, 0, 0))
	if pt.width > 19 || !strings.Contains(tiny.String(), "default/") {
		t.Errorf("cols=20: %d runes drawn, %q", pt.width, tiny.String())
	}

	bar := func(out string) int { return strings.Count(out, "#") + strings.Count(out, "-") }
	for _, tc := range []struct {
		cols    int
		barWant int // cells
	}{{full + 1, 22}, {full - 5, 14}, {full - 13, 8}, {40, 0}} {
		var buf bytes.Buffer
		cols := tc.cols
		p := &progressLine{w: &buf, tty: true, start: time.Now(), cols: func() int { return cols }}
		p.update(compactSnap("default/biases", 6, 45, 0, 0))
		if p.width > cols-1 {
			t.Errorf("cols=%d: drew %d runes; it would wrap", cols, p.width)
		}
		if got := bar(buf.String()); got != tc.barWant {
			t.Errorf("cols=%d: a %d-cell bar, want %d (shrink before dropping): %q", cols, got, tc.barWant, buf.String())
		}
		if !strings.Contains(buf.String(), "default/") {
			t.Errorf("cols=%d: the Where must survive: %q", cols, buf.String())
		}
	}
}

// The redraw goroutine and a parallel pass's updates share the line. Run with -race.
func TestProgressLineIsSafeForConcurrentUse(t *testing.T) {
	var buf bytes.Buffer
	p := &progressLine{w: &buf, tty: true, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	go p.tick()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				p.update(compactSnap("lens", i, 200, 0, 0))
			}
		}(g)
	}
	wg.Wait()
	p.finish()
	p.finish() // idempotent
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:                          "45s",
		6*time.Minute + 12*time.Second:            "6m12s",
		time.Hour + 5*time.Minute + 9*time.Second: "1h05m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
