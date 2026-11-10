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
		if strings.Contains(out, "witness lens disable notes") || strings.Contains(out, "orphan") {
			t.Errorf("%s must not treat an old-format lens as an orphan:\n%s", screen, out)
		}
	}
}

// F048: `config set --lens` writes the registered lens.json, which a re-register replaces whole,
// so a pinned model or runner vanished behind a bare "[ok] registered". Each lost setting must be
// named with the command that puts it back; one the new source keeps is not lost.
func TestReRegisterWarnsAboutDroppedSettings(t *testing.T) {
	st := readFixture(t)
	if err := st.RegisterLens("math", writeLensDir(t, "", "mine math", "fold math")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLensModel("math", "extract", "claude-opus-x"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLensRunner("math", "opencode"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLensModel(store.LensDefault, "review", "claude-strong"); err != nil {
		t.Fatal(err)
	}
	keep := writeLensDir(t, "", "mine math", "fold math")
	if err := os.WriteFile(filepath.Join(keep, "lens.json"), []byte(`{"runner":"opencode"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := lensRegister(st, "math", keep); err != nil {
			t.Fatal(err)
		}
		if err := lensLoadDefault(st); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"[!] dropped mine_model = claude-opus-x  (lens math)",
		"witness config set mine_model claude-opus-x --lens math",
		"[!] dropped review_model = claude-strong  (lens default)",
		"witness config set review_model claude-strong --lens default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dropped runner") {
		t.Errorf("a setting the new source keeps was not dropped:\n%s", out)
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
