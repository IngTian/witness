package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReservedLensName reports whether a lens name is reserved and may not be taken by
// a registered lens. Two names are reserved (both defined in types.go, the single
// source of truth):
//   - LensDefault ("default") — the always-on built-in lens's identity. A second
//     lens under this name would share the built-in's (session,'default') watermark
//     and observation key, corrupting the backbone lens's data (two prompts writing
//     Lens='default', one progress row, cross-contaminated dedup).
//   - LensUnified ("unified") — the cross-lens profile summary's filename stem; a
//     per-lens summary under this name would clobber the unified portrait.
//
// This is the ONE piece of legitimate default-lens specialness that lives at the
// identity layer: default is not treated differently by the engine (every lens is
// just a prompt + a name), but its name is protected so no registered lens can
// impersonate it. The check is on the sanitized name (registry filesystem key),
// case-FOLDED: the reserved identities collide with the built-ins on the case-
// insensitive filesystems witness's primary platforms use (macOS APFS, Windows
// NTFS), where profile/Default.md and profile/default.md are the SAME file. A case-
// sensitive check would let `register Default` through, and its per-lens summary
// would then silently clobber the built-in's profile — exactly the impersonation
// this guard exists to prevent. Folding closes that bypass on every platform.
func ReservedLensName(name string) bool {
	n := strings.ToLower(sanitize(name))
	return n == LensUnified || n == LensDefault
}

// LensesDir is the central lens registry: <root>/lenses/<name>/lens.md. Lenses
// live here (not in repos) so the same definition is shared across all sessions.
func (s *Store) LensesDir() string { return filepath.Join(s.Root, "lenses") }

// RegisterLens copies a lens definition file into the registry under `name`,
// creating/overwriting <root>/lenses/<name>/lens.md.
func (s *Store) RegisterLens(name, srcPath string) error {
	if ReservedLensName(name) {
		return fmt.Errorf("lens name %q is reserved (the always-on built-in lens or the cross-lens summary); choose another name", name)
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
