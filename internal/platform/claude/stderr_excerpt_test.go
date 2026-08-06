package claude

import (
	"strings"
	"testing"
)

// A failed child's stderr is interpolated into an error that becomes ONE line in witness.log,
// and that stderr is unbounded across the 10-minute timeout — so a chatty or looping `claude`
// could write a single multi-megabyte log line. This bounds it.
func TestStderrExcerptBoundsAnEnormousStderr(t *testing.T) {
	huge := strings.Repeat("noise ", 2_000_000) + "THE REAL ERROR"
	got := stderrExcerpt(huge)

	if len(got) > maxStderrExcerpt+200 { // +marker
		t.Errorf("excerpt is %d bytes; the whole point is to bound the log line", len(got))
	}
	// The TAIL must survive: a failing CLI puts the actual error last, after banners and
	// progress noise, so truncating from the front would keep exactly the useless part.
	if !strings.Contains(got, "THE REAL ERROR") {
		t.Error("the tail was discarded — the diagnostic part of a CLI failure is at the END")
	}
	// The truncation must be VISIBLE, so nobody debugs a silently clipped message.
	if !strings.Contains(got, "elided") {
		t.Errorf("truncation is invisible: %q", got[:min(len(got), 120)])
	}
}

// An ordinary stderr must pass through untouched (trimmed only) — the bound must not alter the
// common case, which is what every existing error message and test relies on.
func TestStderrExcerptLeavesNormalStderrAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"error: model not available",
		"  \n error: AWS auth refresh timed out after 3 minutes \n ",
		strings.Repeat("x", maxStderrExcerpt), // exactly at the limit
	} {
		got := stderrExcerpt(s)
		want := strings.TrimSpace(s)
		if got != want {
			t.Errorf("stderrExcerpt(%d bytes) altered a normal message: got %d bytes", len(s), len(got))
		}
		if strings.Contains(got, "elided") {
			t.Errorf("a normal message was marked truncated: %q", got)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
