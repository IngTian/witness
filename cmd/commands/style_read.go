package commands

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Helpers the read surfaces (facets, observations, profile, doctor, config, lens) share on top of
// style.go. Same contract: decoration only on a terminal, an ASCII stand-in everywhere else, and
// nothing here is ever reached by a --json path.

// glyph picks the fancy form on a terminal and its ASCII stand-in in a pipe or under NO_COLOR.
func glyph(fancy, plain string) string { return outPen().pick(fancy, plain) }

// rule is a dim horizontal rule, n wide but never wider than the terminal ("-" when plain).
func rule(n int) string {
	if w := termWidth(os.Stdout); useColor && w > 2 && n > w-2 {
		n = w - 2
	}
	return dim(strings.Repeat(glyph("─", "-"), n))
}

// result is a state-changing command's one result line: glyph first, then the message, then a dim
// parenthesized note when there is one.
func result(g, msg, note string) string {
	s := g + " " + msg
	if note != "" {
		s += "  " + dim("("+note+")")
	}
	return s
}

// errLine is result for stderr, where a notice must not interleave with stdout's data: its own
// color rule (useColorErr), so `witness profile > file` keeps the file clean and the terminal
// still gets the glyph.
func errLine(fancy, plain, color, msg string) string {
	return errPen().mark(color, fancy, plain) + " " + msg
}

// wrapHanging word-wraps s to the terminal width, starting at column `used` and continuing every
// later line at that same column — so a long list keeps its indent instead of soft-wrapping back to
// column 0. Line breaks already in s are kept (a facet value can have paragraphs). Piped output is
// returned whole.
func wrapHanging(s string, used int) string {
	width := termWidth(os.Stdout)
	if !useColor || width <= used+10 {
		return s
	}
	pad := strings.Repeat(" ", used)
	var b strings.Builder
	for li, line := range strings.Split(s, "\n") {
		if li > 0 {
			b.WriteString("\n")
			b.WriteString(pad)
		}
		col := used
		for i, w := range strings.Fields(line) {
			if i > 0 {
				if col+1+cols(w) >= width {
					b.WriteString("\n")
					b.WriteString(pad)
					col = used
				} else {
					b.WriteByte(' ')
					col++
				}
			}
			b.WriteString(w)
			col += cols(w)
		}
	}
	return b.String()
}

// fits reports whether a line n columns wide fits the terminal. Piped output has no width, so
// everything fits there.
func fits(n int) bool {
	w := termWidth(os.Stdout)
	return !useColor || w <= 0 || n < w
}

var mdBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)

// markdownLine renders one line of an LLM-authored profile for a terminal: a "#"-heading prints
// bold cyan without its hashes, and **x** prints bold. Plain output gets the line verbatim, so
// `witness profile > file.md` is still the markdown on disk.
func markdownLine(line string) string {
	if !useColor {
		return line
	}
	if t := strings.TrimLeft(line, "#"); t != line && strings.HasPrefix(t, " ") && len(line)-len(t) <= 4 {
		return bold(cyan(strings.TrimSpace(mdBold.ReplaceAllString(t, "$1"))))
	}
	return mdBold.ReplaceAllStringFunc(line, func(m string) string { return bold(m[2 : len(m)-2]) })
}

// confidence renders a facet confidence in a fixed 4-wide column: green when the archive is sure
// (>= 0.8), dim when it is not (< 0.5), plain in between.
func confidence(c float64) string {
	s := fmt.Sprintf("%.2f", c)
	switch {
	case c >= 0.8:
		return green(s)
	case c < 0.5:
		return dim(s)
	}
	return s
}

// humanSize renders a character count the way a person reads it: 950, 12.3k, 2.8M.
func humanSize(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return humanCount(int(n))
}
