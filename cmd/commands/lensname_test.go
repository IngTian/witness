package commands

import (
	"slices"
	"strings"
	"testing"
)

// Since F3 a lens has one name; a `name` left in lens.json is ignored, and `lens register` and `doctor` say so.

// cmd injects the wire command the CLI prints and a refresh.
func TestMCPGetsTheWireCommand(t *testing.T) {
	st := readFixture(t)
	opts := mcpOptions(st)
	if opts.Wire != wireCommand() || opts.Refresh == nil {
		t.Errorf("wire %q (want %q), refresh set %v", opts.Wire, wireCommand(), opts.Refresh != nil)
	}
}

// A lens.json `name` that once renamed a lens is now ignored, so registering one under a name of
// its own succeeds even when another lens holds that name; both commands that show it say so.
func TestAnIgnoredLensJSONNameIsWarned(t *testing.T) {
	st := readFixture(t)
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no model: doctor's embedder check fails fast
	if err := st.RegisterLens("regime", writeLensDir(t, "", "mine", "rev")); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "lens", "register", "corpus-example", writeLensDir(t, "", "mine", "rev", "name", "regime"))
	if err != nil || !strings.Contains(out, `ignored lens.json name "regime"`) || strings.Contains(out, "mines as") {
		t.Fatalf("lens register: %v\n%s", err, out)
	}
	if got := st.RegisteredLenses(); !slices.Equal(got, []string{"corpus-example", "default", "regime"}) {
		t.Fatalf("registered = %v", got)
	}
	doc, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if !strings.Contains(doc, `lens corpus-example: lens.json name "regime" is ignored`) || !strings.Contains(doc, "lens regime already exists") {
		t.Fatalf("doctor must name the ignored lens.json name:\n%s", doc)
	}
}
