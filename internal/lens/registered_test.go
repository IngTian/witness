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
