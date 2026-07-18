package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Legacy lens-format migration (issue #75). Pre-#75 a registered lens was ONE file,
// lenses/<name>/lens.md, with a `# name:`/`# dimensions:` header and `## EXTRACT` /
// `## REVIEW` sections parsed at load time. #75 replaced that with a directory of
// lens.json + extract.md + review.md (read whole, no parsing), and deleted the fragile
// load-time parser.
//
// This file is the ONE place the old format still lives — a frozen, one-shot converter
// that runs at Open (like the DB's legacyL0Rename / schema migrations). It contains legacy
// handling instead of letting "is this an old lens?" checks leak across the codebase. The
// old split-parser here is SAFE where the deleted one wasn't: it runs ONCE at startup, is
// never on the live mine path, and is never mutated by the CLI (the #71/#57 hazards were
// exactly those two properties). After migration a legacy lens is indistinguishable from a
// natively-registered one, so no downstream code needs an old-format branch.
//
// Conversion is NON-destructive and idempotent: it writes the three new files ALONGSIDE
// the old lens.md (a dir with extract.md is already-migrated and skipped), leaving lens.md
// in place as a harmless artifact rather than risking data loss on a delete. An enabled
// legacy lens keeps working across the upgrade — no manual re-register needed.

// migrateLegacyLenses converts every pre-#75 lens.md-only registry directory to the new
// directory format. Best-effort per lens: a malformed one is left as-is (it will still be
// surfaced by RegisteredLenses only once it has an extract.md, so an unconvertible lens
// simply stays invisible, exactly as before). Returns the count converted (for logging).
func (r *lensReg) migrateLegacyLenses() int {
	entries, err := os.ReadDir(r.LensesDir())
	if err != nil {
		return 0 // no registry yet (fresh install) — nothing to migrate
	}
	converted := 0
	for _, e := range entries {
		if !e.IsDir() || isLensStagingDir(e.Name()) {
			continue
		}
		dir := filepath.Join(r.LensesDir(), e.Name())
		// Already new-format (has extract.md) → skip. This makes the pass idempotent.
		if _, err := os.Stat(filepath.Join(dir, lensExtractFile)); err == nil {
			continue
		}
		oldPath := filepath.Join(dir, "lens.md")
		data, err := os.ReadFile(oldPath)
		if err != nil {
			continue // no lens.md → not a legacy lens (some other dir); leave it
		}
		if r.convertLegacyLensFile(dir, string(data)) {
			converted++
		}
	}
	return converted
}

// convertLegacyLensFile parses one old-format lens.md body and writes the new files into
// dir. Returns true if it wrote a usable lens (non-empty EXTRACT). It writes lens.json only
// when there is a name/dimensions directive worth recording — a lens whose name was implicit
// (from the dir) needs no lens.json, matching a natively-registered minimal lens.
func (r *lensReg) convertLegacyLensFile(dir, body string) bool {
	name, dims, extract, review := parseLegacyLensFile(body)
	if strings.TrimSpace(extract) == "" {
		return false // no mining prompt → not convertible; leave the old file untouched
	}
	if err := os.WriteFile(filepath.Join(dir, lensExtractFile), []byte(extract), 0o600); err != nil {
		return false
	}
	if strings.TrimSpace(review) != "" {
		_ = os.WriteFile(filepath.Join(dir, lensReviewFile), []byte(review), 0o600)
	}
	// Only emit lens.json when it carries something (an explicit name that differs from the
	// dir, or dimensions). A bare name matching the dir is redundant (loadDir falls back to
	// the dir name), so a minimal lens gets no lens.json — same shape as `lens register` of a
	// dir with no lens.json.
	cfg := map[string]any{}
	if n := strings.TrimSpace(name); n != "" && n != filepath.Base(dir) {
		cfg["name"] = n
	}
	if len(dims) > 0 {
		cfg["dimensions"] = dims
	}
	if len(cfg) > 0 {
		if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(dir, lensConfigFile), append(out, '\n'), 0o600)
		}
	}
	return true
}

// parseLegacyLensFile is the FROZEN pre-#75 lens parser, kept only for one-shot migration.
// Format:
//
//	# name: math
//	# dimensions: speed, proof
//	## EXTRACT
//	<extract prompt...>
//	## REVIEW
//	<review prompt...>
//
// Directives are header-only (before the first `##` section). A `## EXTRACT`/`## REVIEW`
// line is a structural delimiter checked first, so a malformed header can't swallow a
// section (the same defensive ordering the original had). Section bodies are verbatim.
// This is deliberately a copy of the deleted parseLensFile — frozen history, never grown.
func parseLegacyLensFile(s string) (name string, dims []string, extract, review string) {
	var section string
	var eb, rb strings.Builder
	inComment := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "## EXTRACT":
			section, inComment = "extract", false
			continue
		case "## REVIEW":
			section, inComment = "review", false
			continue
		}
		if section == "" {
			if !inComment && strings.HasPrefix(trimmed, "<!--") {
				inComment = true
			}
			if inComment {
				if strings.Contains(trimmed, "-->") {
					inComment = false
				}
				continue
			}
		}
		switch {
		case section == "" && strings.HasPrefix(trimmed, "# name:"):
			name = strings.TrimSpace(strings.TrimPrefix(trimmed, "# name:"))
		case section == "" && strings.HasPrefix(trimmed, "# dimensions:"):
			for _, d := range strings.Split(strings.TrimPrefix(trimmed, "# dimensions:"), ",") {
				if d = strings.TrimSpace(d); d != "" {
					dims = append(dims, d)
				}
			}
		default:
			switch section {
			case "extract":
				eb.WriteString(line + "\n")
			case "review":
				rb.WriteString(line + "\n")
			}
		}
	}
	return name, dims, strings.TrimSpace(eb.String()), strings.TrimSpace(rb.String())
}
