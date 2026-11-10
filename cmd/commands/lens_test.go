package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// F065 deleted the converter that rewrote a pre-v0.3.0 lens (a lone lens.md) at every Open. Such
// a lens never mines, and `lens list` showed an enabled one as an orphan whose hint said
// "disable"; a disabled one, and doctor, said nothing. Both must name the actual fix.
func TestOldFormatLensIsNamedWithItsFix(t *testing.T) {
	st := readFixture(t)
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no embedder model: doctor skips the slow load
	for _, name := range []string{"notes", "drafts"} {
		dir := filepath.Join(st.LensesDir(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lens.md"), []byte("## EXTRACT\nmine\n## REVIEW\nrev\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnableLens("notes"); err != nil {
		t.Fatal(err)
	}
	list := captureStdout(t, func() {
		if err := lensList(st); err != nil {
			t.Fatal(err)
		}
	})
	doctor, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	for screen, out := range map[string]string{"lens list": list, "doctor": doctor} {
		for _, name := range []string{"notes", "drafts"} {
			if !strings.Contains(out, name) || !strings.Contains(out, "witness lens register "+name+" ") {
				t.Errorf("%s must name old-format lens %s and its re-register:\n%s", screen, name, out)
			}
		}
		if strings.Count(out, oldLensFormat) != 2 {
			t.Errorf("%s must say %q once per old-format lens:\n%s", screen, oldLensFormat, out)
		}
		// lens.md's headers carried the data name and dimensions; a split into the two prompts
		// alone loses them.
		if !strings.Contains(out, `"# dimensions:" lines go in lens.json`) {
			t.Errorf("%s must say where lens.md's header lines go:\n%s", screen, out)
		}
		if strings.Contains(out, "witness lens disable notes") || strings.Contains(out, "orphan") {
			t.Errorf("%s must not treat an old-format lens as an orphan:\n%s", screen, out)
		}
	}
}

// The re-register hint is a command to paste, so a data dir with a space comes out quoted.
func TestOldFormatHintQuotesThePath(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "home sp"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	want := "witness lens register notes " + shellQuote(filepath.Join(st.LensesDir(), "notes"))
	if got := reRegisterOldLens(st, "notes"); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// F048: `config set --lens` writes the registered lens.json, which a re-register replaces whole,
// so a pinned model or runner vanished behind a bare "[ok] registered". Each lost setting must be
// named with the command that puts it back (quoted for a shell); one the new source keeps is not
// lost, and one it changes is named as replaced, with no hint that would undo the change. Only
// lens.json is compared, so a lens that does not load, before or after, is no exception.
func TestReRegisterWarnsAboutDroppedSettings(t *testing.T) {
	st := readFixture(t)
	if err := st.RegisterLens("math", writeLensDir(t, "", "mine math", "fold math")); err != nil {
		t.Fatal(err)
	}
	for _, set := range []func() error{
		func() error { return st.SetLensModel("math", "extract", "opus[1m]") }, // zsh globs an unquoted [ ]
		func() error { return st.SetLensModel("math", "review", "claude-a") },
		func() error { return st.SetLensRunner("math", "opencode") },
		func() error { return st.SetLensModel(store.LensDefault, "review", "claude-strong") },
	} {
		if err := set(); err != nil {
			t.Fatal(err)
		}
	}
	// The case load-default's help names, "re-seed it after editing broke it": default no longer loads.
	if err := os.WriteFile(filepath.Join(st.LensesDir(), store.LensDefault, "extract.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src := func(cfg string) string {
		dir := writeLensDir(t, "", "mine math", "fold math")
		if err := os.WriteFile(filepath.Join(dir, "lens.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	reg := func(cfg string) func() error { return func() error { return lensRegister(st, "math", src(cfg)) } }
	for _, step := range []struct {
		name      string
		run       func() error
		want, not []string
	}{
		{"keeps runner, changes review_model, drops mine_model", reg(`{"runner":"opencode","review_model":"claude-b"}`), []string{
			"[!] dropped mine_model = opus[1m]  (lens math)",
			"witness config set mine_model 'opus[1m]' --lens math",
			"[!] replaced review_model claude-a → claude-b  (lens math)",
		}, []string{"dropped runner", "replaced runner", "review_model claude-a --lens"}},
		// Keeps both values but does not load (dimensions must be a list): nothing was lost.
		{"keeps both, does not load", reg(`{"dimensions":"x","runner":"opencode","review_model":"claude-b"}`), nil, []string{"[!]"}},
		// The copy that left does not load, and still has both values to lose.
		{"from a copy that does not load", reg(`{}`), []string{
			"[!] dropped runner = opencode  (lens math)",
			"[!] dropped review_model = claude-b  (lens math)",
		}, nil},
		{"load-default over a default that does not load", func() error { return lensLoadDefault(st) }, []string{
			"[!] dropped review_model = claude-strong  (lens default)",
			"witness config set review_model claude-strong --lens default",
		}, nil},
	} {
		out := captureStdout(t, func() {
			if err := step.run(); err != nil {
				t.Fatal(err)
			}
		})
		for _, want := range step.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q in:\n%s", step.name, want, out)
			}
		}
		for _, not := range step.not {
			if strings.Contains(out, not) {
				t.Errorf("%s: %q, but nothing the source keeps or changes was dropped:\n%s", step.name, not, out)
			}
		}
	}
}

// F142: the default lens is seeded once and then read only from the archive's copy, so a prompt
// fix in a newer binary never reached an existing archive and doctor said nothing. doctor and
// `lens show default` name the difference in one line, and stay quiet when there is none.
func TestDefaultLensPromptsDifferFromTheBundle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, st *store.Store, dir string)
		stale  bool
	}{
		{"match", func(*testing.T, *store.Store, string) {}, false},
		{"review.md edited", func(t *testing.T, _ *store.Store, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("an older seed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"emerge.md missing", func(t *testing.T, _ *store.Store, dir string) {
			if err := os.Remove(filepath.Join(dir, "emerge.md")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"no default lens", func(t *testing.T, st *store.Store, _ string) {
			if err := st.DeregisterLens(store.LensDefault); err != nil {
				t.Fatal(err)
			}
		}, false},
		// A disabled default never runs, and load-default would turn it back on.
		{"disabled, review.md edited", func(t *testing.T, st *store.Store, dir string) {
			if err := st.DisableLens(store.LensDefault); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("an older seed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		// No bundle resolves (an empty prompts dir): nothing to claim the copy differs from.
		{"no bundle, review.md edited", func(t *testing.T, _ *store.Store, dir string) {
			t.Setenv("WITNESS_PROMPTS", t.TempDir())
			if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("an older seed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := readFixture(t) // seeds default from the bundled prompts/default
			t.Setenv("WITNESS_ASSETS", t.TempDir())
			tc.change(t, st, filepath.Join(st.LensesDir(), store.LensDefault))
			doctor, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
			screens := map[string]string{"doctor": doctor}
			if tc.name != "no default lens" {
				show, err := runCLI(t, "lens", "show", "default")
				if err != nil {
					t.Fatal(err)
				}
				screens["lens show default"] = show
			}
			for screen, out := range screens {
				if got := strings.Count(out, defaultLensStaleMsg); got != map[bool]int{true: 1, false: 0}[tc.stale] {
					t.Errorf("%s: the stale-prompts line appears %d times, want stale=%v:\n%s", screen, got, tc.stale, out)
				}
			}
		})
	}
}
