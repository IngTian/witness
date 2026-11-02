package commands

import (
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	long := strings.Repeat("x", 9000)
	if got := fitLine(long, 8); got != long {
		t.Errorf("piped output keeps the full text (got %d runes)", len(got))
	}
}

// The pen is the one place a stream's decoration rule lives: on, color AND fancy glyphs; off, plain
// text AND their ASCII stand-ins. Every glyph and hint helper is built on it.
func TestPenDecidesColorAndGlyphsTogether(t *testing.T) {
	on, off := pen{on: true}, pen{on: false}
	if got := on.mark(ansiGreen, "✓", "[ok]"); got != ansiGreen+"✓"+ansiReset {
		t.Errorf("on: %q", got)
	}
	if got := off.mark(ansiGreen, "✓", "[ok]"); got != "[ok]" {
		t.Errorf("off: %q", got)
	}
	if got := on.paint(ansiDim, ""); got != "" {
		t.Errorf("painting nothing must emit no escape codes: %q", got)
	}
	if got := off.hint(16, "define it", "witness lens register x <dir>"); got != strings.Repeat(" ", 16)+"-> define it     witness lens register x <dir>" {
		t.Errorf("off hint: %q", got)
	}
	if got := on.hint(2, "diagnose", "witness doctor"); got != "  "+ansiDim+"↳ diagnose     "+ansiReset+" witness doctor" {
		t.Errorf("on hint: %q", got)
	}
	// Under --json, stderr's pen is off even on a terminal: the error is data.
	old, oldArgs := useColorErr, os.Args
	defer func() { useColorErr, os.Args = old, oldArgs }()
	useColorErr, os.Args = true, []string{"witness", "facets", "--json"}
	if errPen().on {
		t.Error("errPen must be off under --json")
	}
	os.Args = []string{"witness", "facets"}
	if !errPen().on {
		t.Error("errPen must follow useColorErr otherwise")
	}
}

// fitTo cuts by terminal COLUMNS: a CJK character fills two, and this archive's text is often
// Chinese — a rune count let such lines overflow the terminal by several columns.
func TestFitToCountsColumnsNotRunes(t *testing.T) {
	if got := fitTo("abcdefghij", 0, 8); got != "abcdef…" {
		t.Errorf("ASCII: %q", got)
	}
	got := fitTo("中文中文中文中文", 0, 9) // 8 columns of room: 3 characters (6) + the ellipsis
	if got != "中文中…" || cols(got) > 8 {
		t.Errorf("CJK must be cut by columns: %q (%d columns)", got, cols(got))
	}
	if got := fitTo("short", 0, 80); got != "short" {
		t.Errorf("a line that fits is untouched: %q", got)
	}
	if got := fitTo(strings.Repeat("x", 500), 0, 0); len(got) != 500 {
		t.Error("unknown width keeps the text whole")
	}
}

// -j is every command's --json shorthand, so an error under -j must be JSON too.
func TestJSONModeSeesTheShorthand(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	for args, want := range map[string]bool{"facets -j": true, "status --json": true, "doctor --json=true": true, "facets": false, "lens list": false} {
		os.Args = append([]string{"witness"}, strings.Fields(args)...)
		if got := jsonOutputMode(); got != want {
			t.Errorf("jsonOutputMode(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestShortTime(t *testing.T) {
	now := time.Date(2026, 11, 2, 15, 0, 0, 0, time.Local)
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	for ts, want := range map[string]string{
		at(-2 * time.Hour):       "today 13:00",
		at(-20 * time.Hour):      "yesterday 19:00",
		at(-30 * 24 * time.Hour): now.Add(-30 * 24 * time.Hour).Format("2006-01-02"),
		"not a time":             "not a time",
		"":                       "",
	} {
		if got := shortTimeAt(ts, now); got != want {
			t.Errorf("shortTimeAt(%q) = %q, want %q", ts, got, want)
		}
	}
}
