package lens

import (
	"os"
	"path/filepath"
	"strings"
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

// Regression: a `# key:` line INSIDE an HTML comment (the usual place a lens file
// documents its own directives) must NOT be parsed as a real directive and clobber the
// actual header. Also: a `# ...` line inside a prompt SECTION is verbatim prompt text,
// never a directive. Both are the header-only gate in parseLensFile.
func TestLensDirectivesAreHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	def := "# name: real\n" +
		"# dimensions: a, b\n" +
		"<!--\n" +
		"  Docs for the author. These mentions must be IGNORED:\n" +
		"  # name: not-the-real-name\n" +
		"  # dimensions: x, y, z\n" +
		"-->\n" +
		"## EXTRACT\n" +
		"Emit observations.\n" +
		"# this looks like a directive but it's prompt text\n" +
		"## REVIEW\n" +
		"Synthesize.\n"
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "lens.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadRegistered("real", dir)
	if err != nil {
		t.Fatalf("LoadRegistered: %v", err)
	}
	if l.Name != "real" {
		t.Fatalf("comment `# name:` must not override the real name, got %q", l.Name)
	}
	if len(l.Dimensions) != 2 {
		t.Fatalf("comment `# dimensions:` must not append to the real header, got %v", l.Dimensions)
	}
	// The prompt-section `# ...` line is preserved verbatim as prompt text.
	if !strings.Contains(l.Extract, "# this looks like a directive but it's prompt text") {
		t.Fatalf("a `#` line inside EXTRACT must be kept as prompt text, got:\n%s", l.Extract)
	}
}

// Regression (audit finding): a lens whose header comment is never closed before the
// section markers must NOT silently swallow `## EXTRACT`/`## REVIEW` and yield empty
// prompts — the section markers are structural delimiters that end the header and any
// open comment. A lens with empty Extract/Review would distill on empty system prompts,
// a silent failure surfacing only much later as prose drift.
func TestUnclosedHeaderCommentDoesNotEatSections(t *testing.T) {
	dir := t.TempDir()
	def := "# name: real\n" +
		"# dimensions: a, b\n" +
		"<!--\n" +
		"  the author forgot to close this comment\n" +
		"## EXTRACT\n" +
		"Emit observations.\n" +
		"## REVIEW\n" +
		"Synthesize.\n"
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "lens.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadRegistered("real", dir)
	if err != nil {
		t.Fatalf("LoadRegistered: %v", err)
	}
	if !strings.Contains(l.Extract, "Emit observations.") {
		t.Fatalf("unclosed header comment ate the EXTRACT section, got Extract=%q", l.Extract)
	}
	if !strings.Contains(l.Review, "Synthesize.") {
		t.Fatalf("unclosed header comment ate the REVIEW section, got Review=%q", l.Review)
	}
}

// The reserved-name guard's ultimate backstop: a lens registered under an innocent
// directory name whose `# name:` header resolves to a reserved identity ("default"
// or "unified") must be REJECTED at load — else it would impersonate the always-on
// built-in / the unified summary and collide on the shared lens key. RegisterLens/
// EnableLens guard the registry NAME, but only the resolved header name is known
// here, so this is where the impersonation is caught.
func TestLoadRegisteredRejectsReservedHeaderName(t *testing.T) {
	// Case variants too: the reserved-name check folds case (a "Default" profile file
	// collides with the built-in's on case-insensitive filesystems), so a header that
	// resolves to a case-variant of a reserved name must also be rejected at load.
	for _, reserved := range []string{"default", "unified", "Default", "UNIFIED"} {
		t.Run(reserved, func(t *testing.T) {
			dir := t.TempDir()
			// Registered under an innocent dir name "foo", but the header claims a
			// reserved identity — the exact bypass the registry-name guard can't see.
			if err := os.MkdirAll(filepath.Join(dir, "foo"), 0o755); err != nil {
				t.Fatal(err)
			}
			def := "# name: " + reserved + "\n## EXTRACT\nmine\n## REVIEW\nrev\n"
			if err := os.WriteFile(filepath.Join(dir, "foo", "lens.md"), []byte(def), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRegistered("foo", dir); err == nil {
				t.Fatalf("a lens whose header resolves to reserved %q must be rejected at load", reserved)
			}
		})
	}
}
