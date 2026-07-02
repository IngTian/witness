// Package lens loads distillation lenses. A lens is the *policy* (what growth
// looks like, how to frame it); the tool is the *mechanism*. The "default" lens
// is global and ships with the binary. Additional lenses are centrally registered
// (`witness lens register <name> <file>`) and globally enabled (`witness lens
// enable <name>`) — an enabled lens runs on every session. Nothing is read from a
// repo, so a cloned repo can't inject a prompt into your archive.
package lens

import (
	"os"
	"path/filepath"
	"strings"
)

// Lens carries the two prompts the distiller needs plus identity.
type Lens struct {
	Name       string // tag written onto observations/facets
	Global     bool   // default=true (the always-on built-in); registered lenses=false
	Dimensions []string
	Extract    string // prompt for per-session mining -> observations
	Review     string // prompt for the reviewer -> facets
}

// promptsDir resolves the bundled prompts directory.
func promptsDir() string {
	if d := os.Getenv("WITNESS_PROMPTS"); d != "" {
		return d
	}
	if root := os.Getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		return filepath.Join(root, "prompts")
	}
	return "prompts"
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
		Name:       "default",
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
	l.Global = false
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
func parseLensFile(s string) *Lens {
	l := &Lens{}
	var section string
	var extract, review strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "# name:"):
			l.Name = strings.TrimSpace(strings.TrimPrefix(trimmed, "# name:"))
		case strings.HasPrefix(trimmed, "# dimensions:"):
			for _, d := range strings.Split(strings.TrimPrefix(trimmed, "# dimensions:"), ",") {
				if d = strings.TrimSpace(d); d != "" {
					l.Dimensions = append(l.Dimensions, d)
				}
			}
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
