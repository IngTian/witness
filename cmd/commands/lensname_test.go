package commands

import (
	"os"
	"path/filepath"
	"runtime"
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
	if err := st.EnableLens("corpus-example"); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterLens("spaced", writeLensDir(t, "", "mine", "rev", "name", "my lens")); err != nil {
		t.Fatal(err)
	}
	doc, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	flat := strings.Join(strings.Fields(doc), " ") // because-lines wrap
	for _, want := range []string{
		`lens corpus-example: lens.json name "regime" is ignored`,
		`lens regime already exists, so it keeps its registered name: it now mines its history again under corpus-example (one model call per session), and anything an older witness mined as "regime" stays under "regime"`,
		"stop it witness lens disable corpus-example",
		`clear this remove "name" from ` + tildePath(filepath.Join(st.LensesDir(), "corpus-example", "lens.json")),
		`lens spaced: lens.json name "my lens" is ignored`,
		`only (no spaces or special characters), so it keeps its registered name: enabled, it would mine its history again under spaced`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("doctor lacks %q:\n%s", want, doc)
		}
	}
	if strings.Contains(flat, "witness lens disable spaced") {
		t.Errorf("a disabled lens needs no stop hint:\n%s", doc)
	}
}

// While the one-time rename keeps failing, the config already enables the lens under its data
// name, so the lens is paused, not mining under its old one: doctor says the rename is pending.
func TestDoctorSaysALensRenameIsPending(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a read-only directory (not Windows, not root)")
	}
	st := readFixture(t)
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no model: doctor's embedder check fails fast
	if err := st.RegisterLens("corpus-example", writeLensDir(t, "", "mine", "rev", "name", "regime")); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("corpus-example"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMetaString("lens_names_adopted", ""); err != nil { // as on the first Open after the upgrade
		t.Fatal(err)
	}
	if err := os.Chmod(st.LensesDir(), 0o500); err != nil { // the folder rename fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(st.LensesDir(), 0o700) })
	doc, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	flat := strings.Join(strings.Fields(doc), " ")
	if !strings.Contains(flat, "the rename to regime is pending; witness retries it each time it opens the archive, and the lens is paused until it succeeds") ||
		strings.Contains(flat, "mines its history again") {
		t.Errorf("doctor must call the rename pending:\n%s", doc)
	}
}
