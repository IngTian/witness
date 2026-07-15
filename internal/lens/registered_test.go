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
	// A registered lens with no `# kind:` header defaults to arc (recall-safe), and no
	// `# model_floor:` leaves the floor empty — backward-compat for pre-#57 lens files.
	if l.Kind != KindArc {
		t.Fatalf("kind-less registered lens must default to %q, got %q", KindArc, l.Kind)
	}
	if l.ModelFloor != "" {
		t.Fatalf("floor-less lens must have empty ModelFloor, got %q", l.ModelFloor)
	}

	if _, err := LoadRegistered("missing", dir); err == nil {
		t.Fatalf("expected error for unregistered lens")
	}
}

// The optional `# kind:` / `# model_floor:` headers (#57 PR2) parse when present, and
// an unknown kind normalizes to the recall-safe arc default.
func TestLoadRegisteredParsesKindAndFloor(t *testing.T) {
	dir := t.TempDir()
	write := func(name, def string) *Lens {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "lens.md"), []byte(def), 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := LoadRegistered(name, dir)
		if err != nil {
			t.Fatalf("LoadRegistered(%s): %v", name, err)
		}
		return l
	}

	// Explicit atomic + a model floor.
	a := write("atomiclens", "# name: atomiclens\n# kind: atomic\n# model_floor: sonnet\n## EXTRACT\nx\n## REVIEW\ny\n")
	if a.Kind != KindAtomic {
		t.Fatalf("explicit `# kind: atomic` must parse as %q, got %q", KindAtomic, a.Kind)
	}
	if a.ModelFloor != "sonnet" {
		t.Fatalf("`# model_floor: sonnet` must parse, got %q", a.ModelFloor)
	}

	// Explicit arc (case-insensitive kind).
	if l := write("arclens", "# name: arclens\n# kind: ARC\n## EXTRACT\nx\n## REVIEW\ny\n"); l.Kind != KindArc {
		t.Fatalf("`# kind: ARC` must normalize to %q, got %q", KindArc, l.Kind)
	}

	// An unknown/garbage kind normalizes to arc (recall-safe), not left as-is.
	if l := write("weird", "# name: weird\n# kind: banana\n## EXTRACT\nx\n## REVIEW\ny\n"); l.Kind != KindArc {
		t.Fatalf("unknown kind must normalize to %q, got %q", KindArc, l.Kind)
	}
}

// Regression: a `# key:` line INSIDE an HTML comment (the usual place a lens file
// documents its own directives) must NOT be parsed as a real directive and clobber the
// actual header. Also: a `# ...` line inside a prompt SECTION is verbatim prompt text,
// never a directive. Both are the header-only gate in parseLensFile.
func TestLensDirectivesAreHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	def := "# name: real\n" +
		"# kind: atomic\n" +
		"# model_floor: sonnet\n" +
		"<!--\n" +
		"  Docs for the author. These mentions must be IGNORED:\n" +
		"  # kind: arc | atomic\n" +
		"  # model_floor: <tier, e.g. opus>\n" +
		"  # name: not-the-real-name\n" +
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
	if l.Kind != KindAtomic {
		t.Fatalf("comment `# kind: arc` must not override the real `atomic`, got %q", l.Kind)
	}
	if l.ModelFloor != "sonnet" {
		t.Fatalf("comment `# model_floor: <tier...>` must not override the real `sonnet`, got %q", l.ModelFloor)
	}
	// The prompt-section `# ...` line is preserved verbatim as prompt text.
	if !strings.Contains(l.Extract, "# this looks like a directive but it's prompt text") {
		t.Fatalf("a `#` line inside EXTRACT must be kept as prompt text, got:\n%s", l.Extract)
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
