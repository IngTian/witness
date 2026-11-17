package lens

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadFromDirUnchecked reads an arbitrary directory and names the lens after it: a `name` in
// its lens.json is ignored, as it is for a registered lens.
func TestLoadFromDirUncheckedIgnoresConfigName(t *testing.T) {
	root := t.TempDir()
	dir := writeLensDir(t, root, "whatever", nil, "mine code review", "synth")
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"name":"codereview","dimensions":["rigor","taste"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadFromDirUnchecked(dir)
	if err != nil {
		t.Fatalf("LoadFromDirUnchecked: %v", err)
	}
	if l.Name != "whatever" {
		t.Fatalf("name should be the directory's, got %q", l.Name)
	}
	if len(l.Dimensions) != 2 || l.Extract == "" {
		t.Fatalf("loaded lens wrong: %+v", l)
	}
}

// With no lens.json (or no name in it), the name falls back to the directory's basename.
func TestLoadFromDirUncheckedNameFallsBackToBasename(t *testing.T) {
	root := t.TempDir()
	dir := writeLensDir(t, root, "my-lens", nil, "mine", "synth")
	l, err := LoadFromDirUnchecked(dir)
	if err != nil {
		t.Fatalf("LoadFromDirUnchecked: %v", err)
	}
	if l.Name != "my-lens" {
		t.Fatalf("name should fall back to the dir basename, got %q", l.Name)
	}
}

// A directory with no extract.md is a usage error — there is nothing to preview/mine.
func TestLoadFromDirUncheckedRequiresExtract(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "noextract")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only review.md — no extract.md at all.
	if err := os.WriteFile(filepath.Join(dir, ReviewFile), []byte("only a review here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromDirUnchecked(dir); err == nil {
		t.Fatalf("a directory with no extract.md must error")
	}
}
