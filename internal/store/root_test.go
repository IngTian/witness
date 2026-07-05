package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveRootAdoption locks the data-dir resolution contract (issue #8): the
// binary adopts an existing archive dir IN PLACE — never moving data — and only
// creates the preferred name ("witness") on a truly fresh machine. The rename
// from claude-witness -> witness must therefore never orphan an existing archive.
func TestResolveRootAdoption(t *testing.T) {
	// Drive resolution through XDG_DATA_HOME so we exercise the name list without
	// depending on the real home dir; WITNESS_HOME must be unset for these cases.
	withEnv := func(t *testing.T, base string) {
		t.Setenv("WITNESS_HOME", "")
		os.Unsetenv("WITNESS_HOME")
		t.Setenv("XDG_DATA_HOME", base)
	}

	t.Run("fresh install creates the preferred name (witness)", func(t *testing.T) {
		base := t.TempDir()
		withEnv(t, base)
		got, err := resolveRoot()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(base, "witness"); got != want {
			t.Errorf("fresh install: got %q, want %q", got, want)
		}
	})

	t.Run("legacy-only dir is adopted in place (no move)", func(t *testing.T) {
		base := t.TempDir()
		withEnv(t, base)
		legacy := filepath.Join(base, "claude-witness")
		if err := os.MkdirAll(legacy, 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := resolveRoot()
		if err != nil {
			t.Fatal(err)
		}
		if got != legacy {
			t.Errorf("legacy adoption: got %q, want %q (must use the existing dir, not create witness)", got, legacy)
		}
		// The preferred-name dir must NOT have been created — nothing moved.
		if _, err := os.Stat(filepath.Join(base, "witness")); !os.IsNotExist(err) {
			t.Errorf("resolveRoot must not create a witness dir when a legacy one exists")
		}
	})

	t.Run("preferred name wins when both exist", func(t *testing.T) {
		base := t.TempDir()
		withEnv(t, base)
		for _, n := range []string{"witness", "claude-witness"} {
			if err := os.MkdirAll(filepath.Join(base, n), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		got, err := resolveRoot()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(base, "witness"); got != want {
			t.Errorf("both exist: got %q, want %q (preferred name should win)", got, want)
		}
	})

	t.Run("WITNESS_HOME overrides the name list verbatim", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("XDG_DATA_HOME", base)
		// Even with a legacy dir present, an explicit WITNESS_HOME must win.
		if err := os.MkdirAll(filepath.Join(base, "claude-witness"), 0o700); err != nil {
			t.Fatal(err)
		}
		override := filepath.Join(t.TempDir(), "custom-root")
		t.Setenv("WITNESS_HOME", override)
		got, err := resolveRoot()
		if err != nil {
			t.Fatal(err)
		}
		if got != override {
			t.Errorf("WITNESS_HOME override: got %q, want %q", got, override)
		}
	})
}
