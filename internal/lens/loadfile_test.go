package lens

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadFromFileUnchecked reads an arbitrary path, honors the header name, and forces
// Global=false.
func TestLoadFromFileUncheckedHonorsHeaderName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "whatever.md")
	def := "# name: codereview\n# dimensions: rigor, taste\n## EXTRACT\nmine code review\n## REVIEW\nsynth\n"
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadFromFileUnchecked(path)
	if err != nil {
		t.Fatalf("LoadFromFileUnchecked: %v", err)
	}
	if l.Name != "codereview" {
		t.Fatalf("name should come from header, got %q", l.Name)
	}
	if l.Global {
		t.Fatalf("a candidate lens must never be Global")
	}
	if len(l.Dimensions) != 2 || l.Extract == "" {
		t.Fatalf("parsed lens wrong: %+v", l)
	}
}

// With no `# name:` header, the name falls back to the file's basename (sans ext).
func TestLoadFromFileUncheckedNameFallsBackToBasename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my-lens.md")
	def := "## EXTRACT\nmine\n## REVIEW\nsynth\n" // no header
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadFromFileUnchecked(path)
	if err != nil {
		t.Fatalf("LoadFromFileUnchecked: %v", err)
	}
	if l.Name != "my-lens" {
		t.Fatalf("name should fall back to basename-sans-ext, got %q", l.Name)
	}
}

// A file with no EXTRACT section is a usage error — there is nothing to preview.
func TestLoadFromFileUncheckedRequiresExtract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noextract.md")
	def := "# name: x\n## REVIEW\nonly a review here\n"
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFileUnchecked(path); err == nil {
		t.Fatalf("a file with no EXTRACT section must error")
	}
}

// LoadFromFile is STRICT: a header resolving to a reserved name is rejected. The
// lenient LoadFromFileUnchecked accepts it (preview never writes). This split is what
// lets `lens try` fall back to a "candidate" display while the shared strict loader
// stays uncompromised.
func TestLoadFromFileStrictRejectsReservedName(t *testing.T) {
	for _, reserved := range []string{"default", "unified", "Default", "UNIFIED"} {
		t.Run(reserved, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "innocent.md")
			def := "# name: " + reserved + "\n## EXTRACT\nmine\n## REVIEW\nsynth\n"
			if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
				t.Fatal(err)
			}
			// Strict loader rejects.
			if _, err := LoadFromFile(path); err == nil {
				t.Fatalf("LoadFromFile must reject a header resolving to reserved %q", reserved)
			}
			// Lenient loader accepts (a preview can't collide with anything).
			if _, err := LoadFromFileUnchecked(path); err != nil {
				t.Fatalf("LoadFromFileUnchecked must accept reserved %q (preview is read-only): %v", reserved, err)
			}
		})
	}
}

// A missing file surfaces the read error, not a nil-lens panic.
func TestLoadFromFileMissing(t *testing.T) {
	if _, err := LoadFromFile(filepath.Join(t.TempDir(), "nope.md")); err == nil {
		t.Fatalf("expected an error for a missing file")
	}
}
