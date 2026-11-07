package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// sameLensDef must look at EVERY lens file: one it skipped would compare equal while differing,
// and merge would drop the incoming copy — the emerge.md loss store.LensFiles exists to prevent.
func TestSameLensDefComparesEveryLensFile(t *testing.T) {
	write := func(dir, skip, changed string) {
		for _, f := range store.LensFiles {
			if f.Name == skip {
				continue
			}
			body := "body of " + f.Name
			if f.Name == changed {
				body = "edited " + f.Name
			}
			if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	a, same := t.TempDir(), t.TempDir()
	write(a, "", "")
	write(same, "", "")
	if !sameLensDef(a, same) {
		t.Fatal("identical lens dirs compare different")
	}
	for _, f := range store.LensFiles {
		edited, missing := t.TempDir(), t.TempDir()
		write(edited, "", f.Name)
		write(missing, f.Name, "")
		if sameLensDef(a, edited) {
			t.Errorf("a lens dir with a different %s compares equal", f.Name)
		}
		if sameLensDef(a, missing) || sameLensDef(missing, a) {
			t.Errorf("a lens dir without %s compares equal to one with it", f.Name)
		}
	}
}
