package commands

import (
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPrimitivesPlainWhenNoColor(t *testing.T) {
	old := useColor
	useColor = false
	defer func() { useColor = old }()

	if got := header("lenses"); got != "lenses" {
		t.Errorf("header plain = %q, want %q", got, "lenses")
	}
	if got := enabledGlyph(true); got != "*" {
		t.Errorf("enabledGlyph(true) plain = %q, want %q", got, "*")
	}
	if got := enabledGlyph(false); got != "-" {
		t.Errorf("enabledGlyph(false) plain = %q, want %q", got, "-")
	}
	// kvRow pads the name to a fixed width and appends value + note.
	got := kvRow("runner", "opencode", "lens override")
	if want := "runner        opencode (lens override)"; got != want {
		t.Errorf("kvRow plain =\n %q\nwant\n %q", got, want)
	}
	// note omitted when empty
	if got := kvRow("runner", "opencode", ""); got != "runner        opencode" {
		t.Errorf("kvRow no-note plain = %q", got)
	}
	if got := footer("3 lenses"); got != "3 lenses" {
		t.Errorf("footer plain = %q, want %q", got, "3 lenses")
	}
}

// The capture hooks run inside the editor. On a brand-new archive the first event seeds the default
// lens, and that seeding logged an INFO line through slog's stdlib default — stderr — so the very
// first hook printed "2026/… INFO seeded the built-in default lens … (#44 slice 1a)" into the
// editor. Hooks must print nothing, from the first event on.
func TestHooksPrintNothingOnAFreshArchive(t *testing.T) {
	for _, hook := range []string{"session-start", "capture", "session-end"} {
		t.Run(hook, func(t *testing.T) {
			t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "fresh"))
			in, w, _ := os.Pipe()
			_, _ = w.WriteString(`{"session_id":"s1","prompt":"hi"}`)
			w.Close()
			r, ew, _ := os.Pipe()
			oldIn, oldErr, oldArgs := os.Stdin, os.Stderr, os.Args
			os.Stdin, os.Stderr, os.Args = in, ew, []string{"witness", hook}
			// slog's stdlib default writes through the log package, which captured the ORIGINAL
			// stderr at init — swapping os.Stderr alone would not see the leak this test exists for.
			oldLog, oldSlog := log.Writer(), slog.Default()
			log.SetOutput(ew)
			defer func() { log.SetOutput(oldLog); slog.SetDefault(oldSlog) }()
			slog.SetDefault(slog.New(slog.NewTextHandler(ew, nil))) // as at process start: anything logged lands on stderr
			var out []byte
			done := make(chan struct{})
			go func() { out, _ = io.ReadAll(r); close(done) }()
			_ = captureStdout(t, func() { Run() })
			ew.Close()
			<-done
			os.Stdin, os.Stderr, os.Args = oldIn, oldErr, oldArgs
			if len(out) != 0 {
				t.Errorf("%s printed to stderr on a fresh archive: %q", hook, out)
			}
		})
	}
}

// The shared helpers' plain forms — what a pipe, a log and NO_COLOR see. Tests run without a TTY,
// so these are exactly those forms.
func TestStyleHelpersPlainForms(t *testing.T) {
	if got := kvRow("corpus-example", "enabled", "5 dims"); !strings.HasPrefix(got, "corpus-example  enabled") {
		t.Errorf("a 14-character name must keep a gap before its value: %q", got)
	}
	if got := kvRow("lens", "x", ""); got != "lens          x" {
		t.Errorf("short names still pad to 14: %q", got)
	}
	if got := hint("diagnose", "witness doctor"); got != "  -> diagnose      witness doctor" {
		t.Errorf("hint: %q", got)
	}
	if got := title("facets", "default", "2,039 active"); got != "witness facets · default  2,039 active" {
		t.Errorf("title: %q", got)
	}
	if got := title("status", "", ""); got != "witness status" {
		t.Errorf("title with no scope or summary: %q", got)
	}
	if skipGlyph() != "[-]" || bgGlyph() != "[>]" {
		t.Errorf("plain glyphs: %q %q", skipGlyph(), bgGlyph())
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 21415: "21,415", 2411507: "2,411,507", -1200: "-1,200"} {
		if got := humanCount(n); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", n, got, want)
		}
	}
	home, _ := os.UserHomeDir()
	if got := tildePath(filepath.Join(home, ".local", "share", "witness")); got != filepath.Join("~", ".local", "share", "witness") {
		t.Errorf("tildePath under home: %q", got)
	}
	if got := tildePath("/tmp/x"); got != "/tmp/x" {
		t.Errorf("tildePath outside home must not change: %q", got)
	}
	if got := tildePath(home + "other"); got != home+"other" {
		t.Errorf("a sibling of home is not under it: %q", got)
	}
}
