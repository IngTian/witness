package commands

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

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

	p.update(compactSnap("default/biases", 5, 45, 0, 0))
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

	// The redraw advances the spinner frame without any new progress: the clock must keep moving
	// through a minutes-long model call.
	before := p.frame
	p.mu.Lock()
	p.frame++
	p.draw()
	p.mu.Unlock()
	if p.frame == before {
		t.Error("redraw did not advance")
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
