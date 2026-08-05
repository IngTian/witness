package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The L4 profile layer: human-readable narrative summaries distilled from the
// facets, one markdown file per lens (<lens>.md) plus a cross-lens unified.md,
// under <dataroot>/profile/. Plain files so the user can open them directly; the
// summarizer (writer) and `witness profile` / get_profile MCP (readers) go through
// these methods.

// profileFS is the L4 narrative-profile concern: plain markdown files under
// <root>/profile/. A filesystem leaf — it holds only the data root (never touches
// the DB), so it can be exercised (and mocked at the Phase-B seam) on its own. Root
// is set once at Open and never mutated, so this copy can't drift from Store.Root.
type profileFS struct{ root string }

// ProfileDir is the folder holding the narrative summaries.
func (p *profileFS) ProfileDir() string { return filepath.Join(p.root, "profile") }

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
func (p *profileFS) WriteProfile(lens, markdown string) error {
	name, err := profileFileName(lens)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.ProfileDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.ProfileDir(), name), []byte(markdown), 0o600)
}

// DeleteProfile removes a lens's summary file so ReadProfile reports exists=false
// again (the readers then show the friendly "not generated yet" message rather than a
// blank/empty portrait). Used when a profile stops being applicable — e.g. the unified
// portrait once <2 lenses remain (#44 slice 1a). A missing file is not an error
// (idempotent). Same path-safety as the other methods (rejects an escaping name).
func (p *profileFS) DeleteProfile(lens string) error {
	name, err := profileFileName(lens)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(p.ProfileDir(), name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListProfiles returns the lens names that currently have a summary file on disk, sorted.
// The unified portrait is INCLUDED (as store.ProfileUnified) — callers that only want
// per-lens summaries must filter it out, which is explicit at the call site rather than
// hidden here.
//
// This exists so the summarizer can find ORPHANED summaries: it iterates lenses that have
// facets, so a lens whose facets were dropped (`lens backfill --fresh`, `lens deregister`)
// was never visited and its profile/<lens>.md was left on disk forever. `witness profile
// <lens>` and the MCP get_profile tool read that file directly, so an agent was served a
// narrative built from facets that no longer exist, with no indication it was stale.
//
// A missing profile dir is not an error (nothing generated yet → empty list); neither is an
// unreadable entry name, which is simply skipped rather than failing the whole review.
func (p *profileFS) ListProfiles() ([]string, error) {
	entries, err := os.ReadDir(p.ProfileDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		// Round-trip through the same path guard the writers use, so a hand-created file
		// with a hostile name can never be handed back as a "lens".
		if _, err := profileFileName(name); err != nil {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// ReadProfile returns a lens's narrative summary and whether it exists yet (a
// missing summary is exists=false, not an error, so callers can show a friendly
// "not generated yet" message).
func (p *profileFS) ReadProfile(lens string) (string, bool, error) {
	name, err := profileFileName(lens)
	if err != nil {
		return "", false, err
	}
	b, err := os.ReadFile(filepath.Join(p.ProfileDir(), name))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}
