// Package lens loads distillation lenses. A lens is the *policy* (what growth
// looks like, how to frame it); the tool is the *mechanism*. The "default" lens
// is global and ships with the binary. Additional lenses are centrally registered
// (`witness lens register <name> <file>`) and globally enabled (`witness lens
// enable <name>`) — an enabled lens runs on every session. Nothing is read from a
// repo, so a cloned repo can't inject a prompt into your archive.
package lens

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IngTian/witness/internal/bundle"
	"github.com/IngTian/witness/internal/store"
)

// Lens carries the two prompts the distiller needs plus identity.
type Lens struct {
	Name       string // tag written onto observations/facets
	Global     bool   // default=true (the always-on built-in); registered lenses=false
	Dimensions []string
	Extract    string // prompt for per-session mining -> observations
	Review     string // prompt for the reviewer -> facets
}

// promptsDir resolves the bundled prompts directory. Resolution (bundle.Dir):
// WITNESS_PROMPTS, else $CLAUDE_PLUGIN_ROOT/prompts, else exe-relative (so a
// Windows exec-form hook, with no shell to export CLAUDE_PLUGIN_ROOT, still finds
// the prompts beside the installed binary), else the cwd-relative dev fallback.
func promptsDir() string {
	return bundle.Dir("prompts", "WITNESS_PROMPTS")
}

// LoadDefault loads the always-on global lens from prompts/default/.
func LoadDefault() (*Lens, error) {
	dir := filepath.Join(promptsDir(), "default")
	extract, err := os.ReadFile(filepath.Join(dir, "extract.md"))
	if err != nil {
		return nil, err
	}
	review, err := os.ReadFile(filepath.Join(dir, "review.md"))
	if err != nil {
		return nil, err
	}
	return &Lens{
		Name:       store.LensDefault, // canonical name lives in the data layer (store)
		Global:     true,
		Dimensions: DefaultDimensions,
		Extract:    string(extract),
		Review:     string(review),
	}, nil
}

// LoadSummarizePrompts loads the L4 summarizer prompts from prompts/summarize/:
// lens.md (per-lens narrative) and unified.md (cross-lens portrait). Same
// on-disk resolution as the lens prompts.
func LoadSummarizePrompts() (lensPrompt, unifiedPrompt string, err error) {
	dir := filepath.Join(promptsDir(), "summarize")
	l, err := os.ReadFile(filepath.Join(dir, "lens.md"))
	if err != nil {
		return "", "", err
	}
	u, err := os.ReadFile(filepath.Join(dir, "unified.md"))
	if err != nil {
		return "", "", err
	}
	return string(l), string(u), nil
}

// DefaultDimensions is the fixed scaffold for the global lens. Facets within
// each dimension are emergent (named by the distiller), not pre-enumerated.
var DefaultDimensions = []string{
	"thinking",  // decision frameworks, how problems get approached
	"workstyle", // how work gets organized, paced, sequenced
	"habits",    // recurring practices, defaults, rituals
	"traits",    // stable dispositions (with evidence)
	"biases",    // recurring cognitive traps + their triggers
	"state",     // time-varying context that expires (mood, season, focus)
	"goals",     // north star + how it evolves
	"feedback",  // how corrections land; what the user asks of collaborators
}

// LoadRegistered loads a centrally-registered lens by name from the registry dir
// (<root>/lenses/<name>/lens.md). Lenses are global and shared across sessions;
// the caller decides which are enabled. Returns an error if not registered.
func LoadRegistered(name, lensesDir string) (*Lens, error) {
	data, err := os.ReadFile(filepath.Join(lensesDir, name, "lens.md"))
	if err != nil {
		return nil, err
	}
	l := parseLensFile(string(data))
	if l.Name == "" {
		l.Name = name
	}
	// Backstop the reserved-name guard at the RESOLVED name. RegisterLens/EnableLens
	// reject the registry name, but a lens file's `# name:` header overrides that name
	// (parseLensFile above) — so a file registered under an innocent name could still
	// resolve to a reserved identity (e.g. `# name: default`) and collide with the
	// always-on built-in on the shared (session,'default') watermark + observation key.
	// This is the ultimate boundary where the resolved name is known, so enforce it
	// here too rather than trust every caller to re-check.
	if store.ReservedLensName(l.Name) {
		return nil, fmt.Errorf("lens %q resolves to reserved name %q (its `# name:` header impersonates the built-in/unified identity); rename it", name, l.Name)
	}
	l.Global = false
	return l, nil
}

// LoadFromFileUnchecked loads a candidate lens from an ARBITRARY file path (not the
// registry) for preview/testing — the `witness lens try` path. It parses the file the
// same way as a registered lens, but is intentionally LENIENT about identity: the name
// falls back to the file's basename (sans extension), then the literal "candidate", so
// a work-in-progress prompt file missing a `# name:` header still previews. It does NOT
// apply the reserved-name gate (see LoadFromFile for the strict variant) — a preview
// never writes to the archive, so an impersonating name can't collide with anything.
// It still requires a non-empty EXTRACT (that is the prompt being previewed); a file
// with no EXTRACT section is a usage error, not a silent empty run. Global is forced
// false — a candidate is never the always-on built-in.
func LoadFromFileUnchecked(path string) (*Lens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	l := parseLensFile(string(data))
	if l.Name == "" {
		base := filepath.Base(path)
		l.Name = strings.TrimSpace(strings.TrimSuffix(base, filepath.Ext(base)))
	}
	if l.Name == "" {
		l.Name = "candidate"
	}
	if strings.TrimSpace(l.Extract) == "" {
		return nil, fmt.Errorf("lens file %q has no EXTRACT section (nothing to preview)", path)
	}
	l.Global = false
	return l, nil
}

// LoadFromFile is the STRICT arbitrary-path loader: LoadFromFileUnchecked plus the
// reserved-name gate LoadRegistered enforces (a file whose resolved `# name:` is a
// reserved identity — "default"/"unified", case-folded — is rejected). Callers that
// only PREVIEW (never write) may catch the reserved-name error and fall back to the
// Unchecked variant with a display name of "candidate"; callers that would persist
// under the resolved name must use this one. Keeping the gate here (not relaxing the
// shared loader) means the strict path stays the default and the lenient path is an
// explicit, preview-only opt-in.
func LoadFromFile(path string) (*Lens, error) {
	l, err := LoadFromFileUnchecked(path)
	if err != nil {
		return nil, err
	}
	if store.ReservedLensName(l.Name) {
		return nil, fmt.Errorf("lens file %q resolves to reserved name %q (the built-in/unified identity); rename its `# name:` header", path, l.Name)
	}
	return l, nil
}

// parseLensFile parses a simple front-matter-ish lens definition:
//
//	# name: math
//	# dimensions: speed, independence, proof_capability, ...
//	## EXTRACT
//	<extract prompt...>
//	## REVIEW
//	<review prompt...>
//
// Directives (`# key:` lines) are HEADER-ONLY: they are honored only BEFORE the first
// `##` section and NOT inside an HTML comment. That matters because the header is
// usually followed by a `<!-- ... -->` block documenting these very directives — those
// mention lines (`# name: ...`) must NOT be parsed as real values and clobber the
// actual header (last-write-wins would otherwise let a comment win). And a prompt
// section's body is passed through VERBATIM, so a `# ...` line inside EXTRACT/REVIEW is
// prompt text, never a directive. The gate makes both cases correct.
func parseLensFile(s string) *Lens {
	l := &Lens{}
	var section string
	var extract, review strings.Builder
	inComment := false // inside an <!-- ... --> block (header directives are ignored there)
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)

		// A `## EXTRACT`/`## REVIEW` line is a STRUCTURAL delimiter: it always ends the
		// header (and forcibly closes any open header comment) and starts its section.
		// Checking it FIRST — before the comment-swallow below — means a malformed or
		// unclosed `<!--` in the header can't silently eat the section markers and leave
		// a lens with empty prompts (a silent-failure the reviewer catches as drift only
		// much later). Tradeoff: a bare `## EXTRACT` line sitting inside a documentation
		// comment would start the section early — but that is rare and produces a
		// visibly-wrong prompt, not a silent-empty one, so it's the safer failure mode.
		switch trimmed {
		case "## EXTRACT":
			section = "extract"
			inComment = false
			continue
		case "## REVIEW":
			section = "review"
			inComment = false
			continue
		}

		// Track HTML-comment nesting on lines that belong to the HEADER (a comment inside
		// a prompt section is verbatim prompt text, handled by the section writer below).
		if section == "" {
			if !inComment && strings.HasPrefix(trimmed, "<!--") {
				inComment = true
			}
			if inComment {
				if strings.Contains(trimmed, "-->") {
					inComment = false
				}
				continue // a header comment line is documentation, never a directive
			}
		}

		switch {
		// Directives are header-only: once a `##` section has started, a `# key:` line is
		// prompt content and falls through to the section writer.
		case section == "" && strings.HasPrefix(trimmed, "# name:"):
			l.Name = strings.TrimSpace(strings.TrimPrefix(trimmed, "# name:"))
		case section == "" && strings.HasPrefix(trimmed, "# dimensions:"):
			for _, d := range strings.Split(strings.TrimPrefix(trimmed, "# dimensions:"), ",") {
				if d = strings.TrimSpace(d); d != "" {
					l.Dimensions = append(l.Dimensions, d)
				}
			}
		default:
			switch section {
			case "extract":
				extract.WriteString(line + "\n")
			case "review":
				review.WriteString(line + "\n")
			}
		}
	}
	l.Extract = strings.TrimSpace(extract.String())
	l.Review = strings.TrimSpace(review.String())
	return l
}
