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

// Lens kinds — how a lens's observations relate to the transcript structure. This
// drives the (later) input-shaping policy and, today, the model-floor advisory in
// `witness doctor`. The empirical work on #57 established the distinction: arc lenses
// need to see a whole cause→effect→verification arc (chunking loses ~70% of their
// yield and a below-floor model silently prose-drifts), while atomic lenses extract
// per-moment observations that fit in a fragment and tolerate weaker models/chunking.
const (
	KindAtomic = "atomic" // per-moment observations; chunk-tolerant (the built-in growth lens)
	KindArc    = "arc"    // needs a whole-session arc; chunk-fragile, higher model floor
)

// Lens carries the two prompts the distiller needs plus identity and shaping hints.
type Lens struct {
	Name       string // tag written onto observations/facets
	Global     bool   // default=true (the always-on built-in); registered lenses=false
	Dimensions []string
	Extract    string // prompt for per-session mining -> observations
	Review     string // prompt for the reviewer -> facets
	// Kind is "atomic" or "arc" (see the Kind* constants). Declared via the optional
	// `# kind:` lens-file header; a registered lens that omits it defaults to "arc"
	// (LoadRegistered) — the recall-safe choice, since treating an arc lens as atomic
	// loses ~70% of its yield while the reverse only costs a reconciliation pass. The
	// built-in default lens is "atomic" (LoadDefault).
	Kind string
	// ModelFloor is an ADVISORY minimum-model hint (e.g. "sonnet"), declared via the
	// optional `# model_floor:` header; "" = none. It does NOT change which model runs
	// (mining uses the single global TriageModel) — `witness doctor` only WARNS when the
	// configured triage model looks weaker than a lens's floor, since a below-floor model
	// prose-drifts (#57). Enforcement (per-lens model override) is deferred to #69.
	ModelFloor string
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
		Kind:       KindAtomic, // the growth lens extracts per-moment obs — chunk-tolerant
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
	// Default an undeclared/invalid kind to "arc" — the recall-safe choice. A registered
	// lens is more likely arc-shaped (a domain rule needing a whole-session view) than
	// atomic, and mis-labeling an arc lens as atomic loses ~70% of its yield to chunking
	// (#57), whereas the reverse only costs one reconciliation pass. An author who knows
	// their lens is atomic opts in explicitly with `# kind: atomic`.
	if l.Kind != KindAtomic && l.Kind != KindArc {
		l.Kind = KindArc
	}
	return l, nil
}

// parseLensFile parses a simple front-matter-ish lens definition:
//
//	# name: math
//	# dimensions: speed, independence, proof_capability, ...
//	# kind: arc                 (optional)
//	# model_floor: sonnet       (optional)
//	## EXTRACT
//	<extract prompt...>
//	## REVIEW
//	<review prompt...>
//
// Directives (`# key:` lines) are HEADER-ONLY: they are honored only BEFORE the first
// `##` section and NOT inside an HTML comment. That matters because the header is
// usually followed by a `<!-- ... -->` block documenting these very directives — those
// mention lines (`# model_floor: <tier>`) must NOT be parsed as real values and
// clobber the actual header (last-write-wins would otherwise let a comment win). And a
// prompt section's body is passed through VERBATIM, so a `# ...` line inside EXTRACT/
// REVIEW is prompt text, never a directive. The gate makes both cases correct.
func parseLensFile(s string) *Lens {
	l := &Lens{}
	var section string
	var extract, review strings.Builder
	inComment := false // inside an <!-- ... --> block (header directives are ignored there)
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)

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
		case section == "" && strings.HasPrefix(trimmed, "# kind:"):
			l.Kind = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "# kind:")))
		case section == "" && strings.HasPrefix(trimmed, "# model_floor:"):
			l.ModelFloor = strings.TrimSpace(strings.TrimPrefix(trimmed, "# model_floor:"))
		case trimmed == "## EXTRACT":
			section = "extract"
		case trimmed == "## REVIEW":
			section = "review"
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
