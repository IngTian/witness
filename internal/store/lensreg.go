package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// reservedLens is the filename stem of the cross-lens profile summary
// (profile/unified.md). A lens may not take this name, or its per-lens summary
// would clobber the unified portrait.
const reservedLens = "unified"

// LensesDir is the central lens registry: <root>/lenses/<name>/lens.md. Lenses
// live here (not in repos) so the same definition is shared across all sessions.
func (s *Store) LensesDir() string { return filepath.Join(s.Root, "lenses") }

// RegisterLens copies a lens definition file into the registry under `name`,
// creating/overwriting <root>/lenses/<name>/lens.md.
func (s *Store) RegisterLens(name, srcPath string) error {
	if sanitize(name) == reservedLens {
		return fmt.Errorf("lens name %q is reserved for the cross-lens profile summary", name)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.LensesDir(), sanitize(name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "lens.md"), data, 0o600)
}

// DeregisterLens removes a lens definition from the registry (no-op if absent).
// (It does not touch config; disable the lens separately if it was enabled.)
func (s *Store) DeregisterLens(name string) error {
	return os.RemoveAll(filepath.Join(s.LensesDir(), sanitize(name)))
}

// RegisteredLenses lists the names of lenses in the registry (dirs holding a
// lens.md).
func (s *Store) RegisteredLenses() []string {
	entries, err := os.ReadDir(s.LensesDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.LensesDir(), e.Name(), "lens.md")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}
