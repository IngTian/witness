package lens

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadFromDirUnchecked reads an arbitrary directory, honors the lens.json name, and
// forces Global=false.
func TestLoadFromDirUncheckedHonorsConfigName(t *testing.T) {
	root := t.TempDir()
	dir := writeLensDir(t, root, "whatever",
		&LensConfig{Name: "codereview", Dimensions: []string{"rigor", "taste"}},
		"mine code review", "synth")
	l, err := LoadFromDirUnchecked(dir)
	if err != nil {
		t.Fatalf("LoadFromDirUnchecked: %v", err)
	}
	if l.Name != "codereview" {
		t.Fatalf("name should come from lens.json, got %q", l.Name)
	}
	if l.Global {
		t.Fatalf("a candidate lens must never be Global")
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

// LoadFromDir is STRICT: a lens.json name resolving to a reserved name is rejected. The
// lenient LoadFromDirUnchecked accepts it (preview never writes). This split is what
// lets `lens try` fall back to a "candidate" display while the shared strict loader
// stays uncompromised.
func TestLoadFromDirStrictRejectsReservedName(t *testing.T) {
	for _, reserved := range []string{"default", "unified", "Default", "UNIFIED"} {
		t.Run(reserved, func(t *testing.T) {
			root := t.TempDir()
			dir := writeLensDir(t, root, "innocent", &LensConfig{Name: reserved}, "mine", "synth")
			// Strict loader rejects.
			if _, err := LoadFromDir(dir); err == nil {
				t.Fatalf("LoadFromDir must reject a lens.json name resolving to reserved %q", reserved)
			}
			// Lenient loader accepts (a preview can't collide with anything).
			if _, err := LoadFromDirUnchecked(dir); err != nil {
				t.Fatalf("LoadFromDirUnchecked must accept reserved %q (preview is read-only): %v", reserved, err)
			}
		})
	}
}

// A missing directory surfaces the read error, not a nil-lens panic.
func TestLoadFromDirMissing(t *testing.T) {
	if _, err := LoadFromDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatalf("expected an error for a missing directory")
	}
}
