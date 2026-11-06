package commands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// A minimal spinner for the one place a witness command blocks on a model: read-time L4
// regeneration (issue #100), measured at ~13s. Without it `witness profile` prints nothing for
// twelve seconds and looks hung, which is indistinguishable from a wedged runner.
//
// It follows the same contract as style.go: decorative only, and silent unless stdout is a real
// TTY with NO_COLOR unset. That is what keeps `witness profile > file`, `| head`, `--json`, and
// CI logs free of escape sequences and carriage returns — the frames would otherwise land in the
// captured markdown — and the MCP stdio server silent whatever its stderr is.
//
// Written to STDERR, deliberately: the profile body is stdout and a progress animation is not part
// of it. So stderr must be a terminal too. Checking stdout alone put every frame into a redirected
// stderr: a real `witness worker review 2>review.log` on a terminal wrote 216 carriage returns and
// 860 escape codes into the log.

// spinnerFrames is the braille cycle: single-width, present in the fonts a modern terminal uses,
// and visually quiet next to the existing glyphs in style.go.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerInterval = 90 * time.Millisecond

// spinner animates a one-line status until Stop is called. The zero value is not usable; call
// startSpinner. A nil *spinner is safe to Stop, so callers need no nil check.
type spinner struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
	msg  string
	out  io.Writer // nil → os.Stderr; set by tests so frames are asserted, not printed
}

// w is the animation sink: stderr in production, a test-supplied buffer otherwise.
func (s *spinner) w() io.Writer {
	if s.out != nil {
		return s.out
	}
	return os.Stderr
}

// startSpinner begins animating msg on stderr, returning nil when output is not an interactive
// terminal (piped, redirected, NO_COLOR, or a test). A nil return still Stops safely, so the
// caller never branches on it.
func startSpinner(msg string) *spinner {
	if !useColor || !stderrIsTerminal {
		return nil
	}
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{}), msg: msg}
	go s.run()
	return s
}

func (s *spinner) run() {
	defer close(s.done)
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			// \r returns to column 0 and the trailing spaces cover a previously longer line.
			fmt.Fprintf(s.w(), "\r%s %s   ", dim(spinnerFrames[i%len(spinnerFrames)]), dim(s.msg))
			i++
		}
	}
}

// Stop halts the animation and erases the line, so whatever the caller prints next starts on a
// clean row. Idempotent and nil-safe; it BLOCKS until the goroutine has stopped writing, which is
// what prevents a stray frame from interleaving with the profile body.
func (s *spinner) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		// Overwrite the frame with spaces, then return to column 0. Writing \r alone would
		// leave the last frame visible on the line the caller is about to print over.
		fmt.Fprintf(s.w(), "\r%s\r", spaces(len(s.msg)+6))
	})
}

func spaces(n int) string { return strings.Repeat(" ", n) }
