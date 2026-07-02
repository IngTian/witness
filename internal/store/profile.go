package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The L4 profile layer: human-readable narrative summaries distilled from the
// facets, one markdown file per lens (<lens>.md) plus a cross-lens unified.md,
// under <dataroot>/profile/. Plain files so the user can open them directly; the
// summarizer (writer) and `witness profile` / get_profile MCP (readers) go through
// these methods.

// ProfileDir is the folder holding the narrative summaries.
func (s *Store) ProfileDir() string { return filepath.Join(s.Root, "profile") }

// profileFileName maps a lens to its summary filename, rejecting anything that
// isn't a plain name — the lens comes from agent/user input (get_profile,
// `witness profile <lens>`), so it must not be able to escape ProfileDir.
func profileFileName(lens string) (string, error) {
	if lens == "" || strings.ContainsAny(lens, `/\`) || strings.Contains(lens, "..") {
		return "", fmt.Errorf("invalid lens name %q", lens)
	}
	return lens + ".md", nil
}

// WriteProfile writes a lens's narrative summary (dir 0700, file 0600). The lens
// "unified" holds the cross-lens portrait. Overwrites — regenerated each review.
func (s *Store) WriteProfile(lens, markdown string) error {
	name, err := profileFileName(lens)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.ProfileDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.ProfileDir(), name), []byte(markdown), 0o600)
}

// ReadProfile returns a lens's narrative summary and whether it exists yet (a
// missing summary is exists=false, not an error, so callers can show a friendly
// "not generated yet" message).
func (s *Store) ReadProfile(lens string) (string, bool, error) {
	name, err := profileFileName(lens)
	if err != nil {
		return "", false, err
	}
	b, err := os.ReadFile(filepath.Join(s.ProfileDir(), name))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}
