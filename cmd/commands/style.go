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

// stderrIsTerminal is the one raw TTY check for STDERR, where errors, spinner frames and
// progress lines go; a var so a test can pin the redirected case.
var stderrIsTerminal = isatty.IsTerminal(os.Stderr.Fd())

// useColorErr is the same rule for STDERR. Kept separate so a redirected stdout does not strip
// color from a terminal's stderr, or the reverse.
var useColorErr = stderrIsTerminal && os.Getenv("NO_COLOR") == ""

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// pen is the decoration rule for ONE output stream. Color and the fancy glyphs (✓ ↳ ━) travel
// together: both only when that stream is a terminal and NO_COLOR is unset; otherwise the ASCII
// stand-ins, so a pipe, a log, or a non-UTF-8 terminal reads cleanly. There are two pens because the
// streams can differ — `witness facets > file` on a terminal has a plain stdout and a styled stderr —
// and every helper below asks one of them instead of re-deciding the rule itself.
type pen struct{ on bool }

// outPen is stdout's pen.
func outPen() pen { return pen{on: useColor} }

// errPen is stderr's pen — plain under --json as well, where an error is data inside {"error": …}.
func errPen() pen { return pen{on: useColorErr && !jsonOutputMode()} }

// paint wraps s in an ANSI style when the pen is on.
func (p pen) paint(code, s string) string {
	if !p.on || code == "" || s == "" {
		return s
	}
	return code + s + ansiReset
}

// pick chooses the fancy form or its ASCII stand-in.
func (p pen) pick(fancy, plain string) string {
	if p.on {
		return fancy
	}
	return plain
}

// mark is a colored glyph with its ASCII stand-in.
func (p pen) mark(code, fancy, plain string) string { return p.paint(code, p.pick(fancy, plain)) }

// hint is a next step, `indent` columns in: "↳ label  command" — the label dim and padded so the
// commands line up and can be copied whole.
func (p pen) hint(indent int, label, command string) string {
	return strings.Repeat(" ", indent) + p.paint(ansiDim, fmt.Sprintf("%s %-13s", p.pick("↳", "->"), label)) + " " + command
}

func styled(code, s string) string { return outPen().paint(code, s) }

// styledErr is styled for text bound for stderr, including text that will be part of an ERROR
// message, which reportError prints whole to stderr after its ✗. Plain when stderr is not a
// terminal, and always plain under --json, where the message is data inside {"error": …}.
func styledErr(code, s string) string { return errPen().paint(code, s) }

func bold(s string) string   { return styled(ansiBold, s) }
func dim(s string) string    { return styled(ansiDim, s) }
func green(s string) string  { return styled(ansiGreen, s) }
func yellow(s string) string { return styled(ansiYellow, s) }
func red(s string) string    { return styled(ansiRed, s) }
func cyan(s string) string   { return styled(ansiCyan, s) }

// okGlyph / warnGlyph / badGlyph are the status marks result rows lead with, on every
// command's screen. They degrade to ASCII when color is off so a non-UTF8 or
// piped terminal still reads cleanly.
func okGlyph() string   { return outPen().mark(ansiGreen, "✓", "[ok]") }
func warnGlyph() string { return outPen().mark(ansiYellow, "!", "[!]") }
func badGlyph() string  { return outPen().mark(ansiRed, "✗", "[x]") }

// header renders a section title (bold on a TTY, plain otherwise). Callers add
// their own surrounding blank lines for rhythm.
func header(title string) string { return bold(title) }

// enabledGlyph is the on/off mark for lists (lens list, status). Green ● when on,
// dim ○ when off; ASCII * / - when color is disabled so piped/non-UTF8 stays clean.
func enabledGlyph(on bool) string {
	if on {
		return outPen().mark(ansiGreen, "●", "*")
	}
	return outPen().mark(ansiDim, "○", "-")
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
func skipGlyph() string { return outPen().mark(ansiDim, "○", "[-]") }

// bgGlyph marks work handed to the background: green ●, or [>] without color.
func bgGlyph() string { return outPen().mark(ansiGreen, "●", "[>]") }

// hint renders a next step as "  ↳ label  command": the label dim and padded so the commands line
// up and can be copied whole. "->" without color.
func hint(label, command string) string { return outPen().hint(2, label, command) }

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

// countOf renders a count with humanCount's thousands separators: "2,039 facets", "1 lens".
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
// read "1 lens" / "2 lenses" instead of the ungrammatical "1 lenses". It is countOf, so every
// count on a screen groups its thousands the same way.
func pluralize(n int, singular, plural string) string { return countOf(n, singular, plural) }
