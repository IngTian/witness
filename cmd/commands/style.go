package commands

import (
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// Terminal styling for human-facing command output. Purely decorative: colors
// and glyphs are emitted only when stdout is a TTY and NO_COLOR is unset, so
// piped/redirected output (and `--json`) stays plain ASCII. Nothing here changes
// behavior — it only affects how the same information is rendered.

var useColor = isatty.IsTerminal(os.Stdout.Fd()) && os.Getenv("NO_COLOR") == ""

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
	padded := name
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
