// Package lens loads distillation lenses. A lens is the *policy* (what growth
// looks like, how to frame it); the tool is the *mechanism*. The "default" lens
// is global and ships with the binary. Additional lenses are centrally registered
// (`witness lens register <name> <file>`) and globally enabled (`witness lens
// enable <name>`) — an enabled lens runs on every session. Nothing is read from a
// repo, so a cloned repo can't inject a prompt into your archive.
package lens

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	witness "github.com/IngTian/claude-witness"
	"github.com/IngTian/claude-witness/internal/bundle"
)

// Lens carries the two prompts the distiller needs plus identity.
type Lens struct {
	Name       string // tag written onto observations/facets
	Global     bool   // default=true (the always-on built-in); registered lenses=false
	Dimensions []string
	Extract    string // prompt for per-session mining -> observations
	Review     string // prompt for the reviewer -> facets
}

// promptsDirOverride resolves an ON-DISK prompts directory if one is configured
// or discoverable: WITNESS_PROMPTS, else $CLAUDE_PLUGIN_ROOT/prompts, else
// exe-relative (Unix plugin/checkout layout). It returns "" when none exists, in
// which case callers fall back to the prompts embedded in the binary. Unlike the
// old promptsDir(), it does NOT return a cwd-relative guess — that path silently
// resolved to a nonexistent ./prompts for a standalone binary; the embedded FS is
// the correct, always-available default now.
func promptsDirOverride() string {
	// An explicit env var always wins, even if we can't stat it (surfaces a real
	// misconfiguration as a read error rather than silently using the embed).
	if d := os.Getenv("WITNESS_PROMPTS"); d != "" {
		return d
	}
	if root := os.Getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		return filepath.Join(root, "prompts")
	}
	// A prompts/ dir sitting beside (or one level up from) the binary — the Unix
	// checkout/plugin layout. bundle.Dir also returns a cwd-relative last resort,
	// so only accept the result if it actually exists on disk.
	if d := bundle.Dir("prompts", ""); dirExists(d) {
		return d
	}
	return ""
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// readPrompt returns the contents of a prompt template at rel (a slash-separated
// path under prompts/, e.g. "default/extract.md"). It prefers an on-disk override
// directory when configured, and otherwise reads from the prompts embedded in the
// binary — so a standalone executable with no sibling prompts/ still works.
func readPrompt(rel string) ([]byte, error) {
	if dir := promptsDirOverride(); dir != "" {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	}
	// embed.FS paths are always slash-separated and rooted at the embed arg.
	return witness.Prompts.ReadFile(path.Join("prompts", rel))
}

// LoadDefault loads the always-on global lens from prompts/default/.
func LoadDefault() (*Lens, error) {
	extract, err := readPrompt("default/extract.md")
	if err != nil {
		return nil, err
	}
	review, err := readPrompt("default/review.md")
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
// on-disk-override-then-embedded resolution as the lens prompts.
func LoadSummarizePrompts() (lensPrompt, unifiedPrompt string, err error) {
	l, err := readPrompt("summarize/lens.md")
	if err != nil {
		return "", "", err
	}
	u, err := readPrompt("summarize/unified.md")
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
