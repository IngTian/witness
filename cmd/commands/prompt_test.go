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
