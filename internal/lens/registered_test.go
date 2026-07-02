package lens

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRegistered(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "math"), 0o755); err != nil {
		t.Fatal(err)
	}
	def := "# name: math\n# dimensions: speed, proof\n## EXTRACT\nmine growth\n## REVIEW\nsynthesize\n"
	if err := os.WriteFile(filepath.Join(dir, "math", "lens.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := LoadRegistered("math", dir)
	if err != nil || l == nil {
		t.Fatalf("LoadRegistered: l=%v err=%v", l, err)
	}
	if l.Name != "math" || l.Global || l.Extract == "" || l.Review == "" || len(l.Dimensions) != 2 {
		t.Fatalf("parsed lens wrong: %+v", l)
	}

	if _, err := LoadRegistered("missing", dir); err == nil {
		t.Fatalf("expected error for unregistered lens")
	}
}
