package commands

import (
	"os"
	"strings"
	"testing"
)

// answer is a scripted reply to a command's [y/N] prompt; *asked records whether it was shown.
func answer(reply bool, asked *bool) func(string) bool {
	return func(string) bool {
		if asked != nil {
			*asked = true
		}
		return reply
	}
}

// The production prompt: only y/yes is a yes. Anything else — including EOF, which is what a
// script with nobody to answer reads — is a no. The --yes hint is for EOF alone: the first version
// keyed it on "not a terminal", so `</dev/null` (a character device) got no hint and a piped "n" did.
func TestAskYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, " y \n": true, "n\n": false, "\n": false, "": false, "yep\n": false} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(in)
		w.Close()
		old := os.Stdin
		os.Stdin = r
		var got bool
		out := captureStdout(t, func() { got = askYesNo("? ") })
		os.Stdin = old
		r.Close()
		if got != want {
			t.Errorf("askYesNo(%q) = %v, want %v", in, got, want)
		}
		if hinted := strings.Contains(out, "--yes"); hinted != (in == "") {
			t.Errorf("askYesNo(%q): --yes hint shown=%v; want it exactly when nobody answered:\n%s", in, hinted, out)
		}
	}
}

// readLine stops at its newline, so the next prompt on the same reader still gets its answer.
func TestReadLineStopsAtItsNewline(t *testing.T) {
	in := strings.NewReader("30\ny\n")
	if got, eof := readLine(in); got != "30" || eof {
		t.Fatalf("readLine = %q, eof %v", got, eof)
	}
	if rest, _ := readLine(in); rest != "y" {
		t.Fatalf("readLine consumed past its newline; the next reader got %q", rest)
	}
	if _, eof := readLine(in); !eof {
		t.Fatal("an exhausted input must report EOF")
	}
}
