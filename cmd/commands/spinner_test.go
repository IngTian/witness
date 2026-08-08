package commands

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// The spinner must be SILENT unless stdout is an interactive terminal.
//
// This is the property that keeps `witness profile > portrait.md`, `| head`, `--json`, CI logs, and
// the MCP stdio server free of braille frames and carriage returns. Under `go test` stdout is not a
// TTY, so useColor is false and startSpinner must return nil — and a nil spinner must be safe to
// Stop, so no caller needs a nil check.
func TestSpinnerIsSilentWhenNotATerminal(t *testing.T) {
	if useColor {
		t.Skip("stdout is a TTY in this environment; this test pins the non-TTY path")
	}
	sp := startSpinner("working…")
	if sp != nil {
		t.Error("startSpinner must return nil when stdout is not an interactive terminal; " +
			"otherwise animation frames land in redirected output and in the MCP stdio stream")
	}
	// Must not panic. A caller writes `sp.Stop()` with no nil check by design.
	sp.Stop()
	sp.Stop() // idempotent
}

// A real spinner stops cleanly, is idempotent, and Stop blocks until the writer is done.
//
// Stop blocking matters: it is what prevents a final frame from interleaving with the profile
// markdown the caller prints immediately afterwards. Driven by constructing the struct directly,
// since startSpinner correctly refuses to animate under `go test`.
func TestSpinnerStopIsIdempotentAndWaitsForTheWriter(t *testing.T) {
	// Capture the frames instead of printing them: a test that animates the real stderr
	// scribbles over `go test` output, and asserting the bytes is stronger than eyeballing them.
	var buf lockedBuffer
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{}), msg: "x", out: &buf}
	go s.run()

	// Let at least one frame fire so the goroutine is genuinely mid-loop.
	time.Sleep(2 * spinnerInterval)

	done := make(chan struct{})
	go func() {
		s.Stop()
		s.Stop() // second call must not double-close or hang
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return; a caller would hang before printing the profile")
	}

	// The writer goroutine must be finished, not merely signalled.
	select {
	case <-s.done:
	default:
		t.Error("Stop returned while the animation goroutine was still able to write; a stray " +
			"frame can interleave with the profile body")
	}

	// It animated at all (else this test would pass on a spinner that never wrote), and the
	// output ENDS with the erase sequence — so the caller's next line starts on a clean row
	// rather than on top of a leftover frame.
	out := buf.String()
	if !strings.Contains(out, spinnerFrames[0]) {
		t.Errorf("no frame was written; the test would then pass on a dead spinner. got %q", out)
	}
	if !strings.HasSuffix(out, "\r") {
		t.Errorf("output must end by returning to column 0 after erasing, got %q", out)
	}
	if !strings.Contains(out, spaces(len("x")+6)) {
		t.Errorf("the erase must overwrite the frame with spaces, got %q", out)
	}
}

// lockedBuffer is a bytes.Buffer safe for the animation goroutine to write while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The frames are single-width and the erase covers the whole line.
//
// A multi-rune frame or an erase shorter than the message leaves visible debris on the line the
// caller is about to print over, which looks like corrupted output rather than a cosmetic glitch.
func TestSpinnerFramesAndEraseAreWellFormed(t *testing.T) {
	if len(spinnerFrames) == 0 {
		t.Fatal("no spinner frames")
	}
	for _, f := range spinnerFrames {
		if n := len([]rune(f)); n != 1 {
			t.Errorf("frame %q is %d runes; frames must be single-width or the line jitters", f, n)
		}
	}
	msg := "distilling the profile"
	if got := spaces(len(msg) + 6); len(got) <= len(msg) {
		t.Errorf("the erase string (%d chars) must exceed the message (%d) to cover the glyph, "+
			"the spaces, and the padding", len(got), len(msg))
	}
	if strings.TrimSpace(spaces(8)) != "" {
		t.Error("spaces() must produce only spaces")
	}
}
