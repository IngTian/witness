package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/text/width"
)

// Terminal styling for human-facing command output. Purely decorative: colors
// and glyphs are emitted only when stdout is a TTY and NO_COLOR is unset, so
// piped/redirected output (and `--json`) stays plain ASCII. Nothing here changes
// behavior — it only affects how the same information is rendered.

var useColor = isatty.IsTerminal(os.Stdout.Fd()) && os.Getenv("NO_COLOR") == ""

// useColorErr is the same rule for STDERR, where errors and progress go. Kept separate so a
// redirected stdout does not strip color from a terminal's stderr, or the reverse.
var useColorErr = isatty.IsTerminal(os.Stderr.Fd()) && os.Getenv("NO_COLOR") == ""

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func styled(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + ansiReset
}

// styledErr is styled for text bound for stderr.
func styledErr(code, s string) string {
	if !useColorErr {
		return s
	}
	return code + s + ansiReset
}

func bold(s string) string   { return styled(ansiBold, s) }
func dim(s string) string    { return styled(ansiDim, s) }
func green(s string) string  { return styled(ansiGreen, s) }
func yellow(s string) string { return styled(ansiYellow, s) }
func red(s string) string    { return styled(ansiRed, s) }
func cyan(s string) string   { return styled(ansiCyan, s) }

// okGlyph / warnGlyph / badGlyph are the status marks used down the left margin
// of doctor's report. They degrade to ASCII when color is off so a non-UTF8 or
// piped terminal still reads cleanly.
func okGlyph() string {
	if useColor {
		return green("✓")
	}
	return "[ok]"
}

func warnGlyph() string {
	if useColor {
		return yellow("!")
	}
	return "[!]"
}

func badGlyph() string {
	if useColor {
		return red("✗")
	}
	return "[x]"
}

// label renders a fixed-width, dimmed field label so aligned rows line up. The
// padding is applied to the plain text BEFORE coloring, so ANSI escape bytes
// don't throw the column width off.
func label(name string) string {
	const width = 10
	padded := name + ":"
	for len(padded) < width {
		padded += " "
	}
	return dim(padded)
}

// header renders a section title (bold on a TTY, plain otherwise). Callers add
// their own surrounding blank lines for rhythm.
func header(title string) string { return bold(title) }

// enabledGlyph is the on/off mark for lists (lens list, status). Green ● when on,
// dim ○ when off; ASCII * / - when color is disabled so piped/non-UTF8 stays clean.
func enabledGlyph(on bool) string {
	if !useColor {
		if on {
			return "*"
		}
		return "-"
	}
	if on {
		return green("●")
	}
	return dim("○")
}

// kvRow renders an aligned "name  value (note)" row. The name is padded to a fixed
// width on the PLAIN text before coloring, so ANSI escapes don't skew the column.
// note is omitted when empty.
func kvRow(name, value, note string) string {
	const width = 14
	padded := name + "  " // always a gap: a 14-character name ran straight into its value
	for len(padded) < width {
		padded += " "
	}
	row := dim(padded) + value
	if strings.TrimSpace(note) != "" {
		row += " " + dim("("+note+")")
	}
	return row
}

// footer renders a dim one-line summary under a list/section.
func footer(summary string) string { return dim(summary) }

// The shared vocabulary of a witness screen. Every command that changes something ends with ONE
// result line led by its glyph — okGlyph ✓ done · warnGlyph ! done with a warning · badGlyph ✗
// failed · skipGlyph ○ nothing to do · bgGlyph ● started in the background — and puts any next
// steps on hint lines below it, never inside a sentence.

// skipGlyph marks a no-op result: dim ○, or [-] without color.
func skipGlyph() string {
	if useColor {
		return dim("○")
	}
	return "[-]"
}

// bgGlyph marks work handed to the background: green ●, or [>] without color.
func bgGlyph() string {
	if useColor {
		return green("●")
	}
	return "[>]"
}

// hint renders a next step as "  ↳ label  command": the label dim and padded so the commands line
// up and can be copied whole. "->" without color.
func hint(label, command string) string {
	arrow := "->"
	if useColor {
		arrow = "↳"
	}
	return "  " + dim(fmt.Sprintf("%s %-13s", arrow, label)) + " " + command
}

// title is a read command's first line: bold "witness <cmd>", then a dim " · <scope>" and a dim
// summary, each omitted when empty.
func title(cmd, scope, summary string) string {
	t := bold("witness " + cmd)
	if scope != "" {
		t += dim(" · " + scope)
	}
	if summary != "" {
		t += "  " + dim(summary)
	}
	return t
}

// tildePath shows a path under the home directory as ~/… on human surfaces; --json keeps it whole.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}
	return p
}

// fitLine cuts s to the terminal width with an ellipsis, leaving room for `used` columns already
// on the line — on a terminal only, where an 8,000-character facet value would otherwise soft-wrap
// back to column 0 and lose its indent. Piped output keeps the full text, so `> file` and `| grep`
// still see everything.
func fitLine(s string, used int) string {
	if !useColor {
		return s
	}
	return fitTo(s, used, termWidth(os.Stdout))
}

// fitTo is fitLine for a known terminal width (0: unknown, keep s whole).
func fitTo(s string, used, width int) string {
	if width <= 0 {
		return s
	}
	room := width - used - 1
	if room <= 1 || cols(s) <= room {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := cols(string(r))
		if n+w > room-1 { // leave a column for the ellipsis
			break
		}
		b.WriteRune(r)
		n += w
	}
	return b.String() + "…"
}

// cols is the width s takes on a terminal: a CJK character fills two columns, which a rune count
// misses — and this archive's text is often Chinese. fitLine measures with it.
func cols(s string) int {
	n := 0
	for _, r := range s {
		switch width.LookupRune(r).Kind() {
		case width.EastAsianWide, width.EastAsianFullwidth:
			n += 2
		default:
			n++
		}
	}
	return n
}

// countOf is pluralize with humanCount's thousands separators: "2,039 facets", "1 lens".
func countOf(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return humanCount(n) + " " + plural
}

// shortTime renders an RFC3339 stamp the way a person reads it: "today 05:29", "yesterday 23:10",
// or "2026-10-05". Anything unparseable is returned as given.
func shortTime(ts string) string { return shortTimeAt(ts, time.Now()) }

func shortTimeAt(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	t, now = t.Local(), now.Local()
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "today " + t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04")
	default:
		return t.Format("2006-01-02")
	}
}

// humanCount groups thousands so a six-digit count is readable at a glance.
func humanCount(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + humanCount(-n)
	}
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pluralize renders "<n> <singular|plural>" picking the form by count, so summary lines
// read "1 lens" / "2 lenses" instead of the ungrammatical "1 lenses".
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}
