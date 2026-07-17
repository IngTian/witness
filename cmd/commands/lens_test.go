package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// `lens list` must surface a pre-#75 old-format lens (a lone lens.md, no extract.md) even
// when it is the ONLY thing in the registry — that legacy-only case is exactly the
// upgraded-user population the warning exists for, and an early `len(reg)==0` return used
// to swallow it. Regression for the re-audit finding.
func TestLensListWarnsOnLegacyOnlyRegistry(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A registry holding ONLY an old-format dir (lens.md, no extract.md), and it's enabled.
	oldDir := filepath.Join(st.LensesDir(), "codereview")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "lens.md"),
		[]byte("# name: codereview\n## EXTRACT\nmine\n## REVIEW\nrev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("codereview"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := cmdLens([]string{"list"}); err != nil {
			t.Fatalf("lens list: %v", err)
		}
	})
	if strings.Contains(out, "no additional lenses registered") {
		t.Fatalf("legacy-only registry wrongly reported as empty:\n%s", out)
	}
	if !strings.Contains(out, "OLD FORMAT") || !strings.Contains(out, "codereview") {
		t.Fatalf("legacy lens must be loudly flagged in `lens list`, got:\n%s", out)
	}
	// It was enabled, so the flag must say so (silent-stop is the worst case).
	if !strings.Contains(out, "ENABLED but NOT running") {
		t.Fatalf("an enabled legacy lens must be flagged as enabled-but-not-running, got:\n%s", out)
	}
}
