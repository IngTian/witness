package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// A pre-#75 old-format lens (a lone lens.md) is migrated at store.Open (issue #75), so by
// the time any command runs it is a normal registered lens — it appears in `lens list`
// with no "old format" noise and, if it was enabled, keeps running. This replaces the
// earlier "warn about un-migrated legacy" behavior: migration means there is nothing to
// warn about.
func TestLensListShowsMigratedLegacyLens(t *testing.T) {
	root := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", root)
	// Seed a legacy lens.md registry dir BEFORE any Open, so Open's migration converts it.
	oldDir := filepath.Join(root, "lenses", "codereview")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "lens.md"),
		[]byte("# name: codereview\n## EXTRACT\nmine\n## REVIEW\nrev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Open runs the migration; enable the (now-migrated) lens.
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("codereview"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := captureStdout(t, func() {
		if err := withLensStore(lensList); err != nil {
			t.Fatalf("lens list: %v", err)
		}
	})
	if strings.Contains(out, "no additional lenses registered") {
		t.Fatalf("a migrated legacy lens must appear in `lens list`, got:\n%s", out)
	}
	if !strings.Contains(out, "codereview") || !strings.Contains(out, "enabled") {
		t.Fatalf("migrated legacy lens should list as enabled, got:\n%s", out)
	}
	if strings.Contains(out, "OLD FORMAT") {
		t.Fatalf("a migrated lens must NOT be flagged as old format, got:\n%s", out)
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
