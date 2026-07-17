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
		if err := cmdLens([]string{"list"}); err != nil {
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
