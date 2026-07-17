// Package lens loads distillation lenses. A lens is the *policy* (what growth
// looks like, how to frame it); the tool is the *mechanism*. The "default" lens
// is global and ships with the binary. Additional lenses are centrally registered
// (`witness lens register <name> <dir>`) and globally enabled (`witness lens
// enable <name>`) — an enabled lens runs on every session. Nothing is read from a
// repo, so a cloned repo can't inject a prompt into your archive.
//
// On-disk shape (issue #75). A lens is a DIRECTORY, not one parsed file:
//
//	<name>/lens.json    structured settings (name, dimensions, per-lens models)
//	<name>/extract.md   the mining (L0→L1) prompt — read whole, no parsing
//	<name>/review.md    the review (L1→L2) prompt — read whole, no parsing
//
// Settings live in JSON (mutated safely by `witness lens set` via a struct round-trip
// — no text surgery, unlike the old `# key:` header directives that were the #71 bug
// class) and prompts live in their own files (a missing prompt is a missing FILE, a
// loud error, never a silently-empty section — the failure the old `## EXTRACT`/
// `## REVIEW` split-parser could produce). The built-in `default` lens ships in the
// SAME 2-file shape under prompts/default/ (its settings are hardcoded in LoadDefault
// rather than a lens.json), so both layouts are one mental model.
package lens

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IngTian/witness/internal/bundle"
	"github.com/IngTian/witness/internal/store"
)

// Lens carries the two prompts the distiller needs plus identity and per-lens
// runner/model overrides. Runner/ExtractModel/ReviewModel are empty by default:
//   - Runner "" → the lens rides the GLOBAL runner (the single runner a slice-1 install
//     uses). A non-empty Runner ("claude"/"opencode") routes this lens's mine+review
//     calls to that runtime instead — so a cheap free-model lens can run on OpenCode
//     while the always-on default stays on the global runner (issue #75 slice 2).
//   - ExtractModel/ReviewModel "" → ride the global stage model for the RESOLVED runner,
//     resolved by distill.ModelFor. A model name is only valid on its runtime, so these
//     are meaningful only together with (or under a matching) Runner.
type Lens struct {
	Name         string // tag written onto observations/facets
	Global       bool   // default=true (the always-on built-in); registered lenses=false
	Dimensions   []string
	Extract      string // prompt for per-session mining -> observations
	Review       string // prompt for the reviewer -> facets
	Runner       string // per-lens runtime ("claude"/"opencode"); "" = global runner
	ExtractModel string // per-lens override for the mine (L0→L1) model; "" = runner default
	ReviewModel  string // per-lens override for the review (L1→L2) model; "" = runner default
}

// LensConfig is the on-disk lens.json schema — the structured settings half of a lens
// directory. Prompts are NOT here (they are the sibling extract.md/review.md files);
// this holds only what a CLI (`witness lens set`) round-trips safely as struct fields.
// omitempty keeps a freshly-scaffolded file terse and a model-less lens from carrying
// empty model keys.
type LensConfig struct {
	Name         string   `json:"name,omitempty"`
	Dimensions   []string `json:"dimensions,omitempty"`
	Runner       string   `json:"runner,omitempty"`
	ExtractModel string   `json:"extract_model,omitempty"`
	ReviewModel  string   `json:"review_model,omitempty"`
}

// LensConfigName / the on-disk filenames of a lens directory. One home for the literals
// so the loader, the store registry, and the CLI never drift on a filename.
const (
	ConfigFile  = "lens.json"
	ExtractFile = "extract.md"
	ReviewFile  = "review.md"
)

// promptsDir resolves the bundled prompts directory. Resolution (bundle.Dir):
// WITNESS_PROMPTS, else $CLAUDE_PLUGIN_ROOT/prompts, else exe-relative (so a
// Windows exec-form hook, with no shell to export CLAUDE_PLUGIN_ROOT, still finds
// the prompts beside the installed binary), else the cwd-relative dev fallback.
func promptsDir() string {
	return bundle.Dir("prompts", "WITNESS_PROMPTS")
}

// LoadDefault loads the always-on global lens from prompts/default/. Its identity and
// dimensions are hardcoded (not a lens.json) because it is the built-in backbone; only
// its two prompts live on disk, in the same extract.md/review.md shape a registered
// lens uses. The default rides the global stage models (no per-lens override), which
// is correct: it runs on EVERY session, so it should be the cheap/consistent default.
func LoadDefault() (*Lens, error) {
	dir := filepath.Join(promptsDir(), "default")
	extract, review, err := readPromptPair(dir)
	if err != nil {
		return nil, err
	}
	return &Lens{
		Name:       store.LensDefault, // canonical name lives in the data layer (store)
		Global:     true,
		Dimensions: DefaultDimensions,
		Extract:    extract,
		Review:     review,
	}, nil
}

// LoadSummarizePrompts loads the L4 summarizer prompts from prompts/summarize/:
// lens.md (per-lens narrative) and unified.md (cross-lens portrait). Same
// on-disk resolution as the lens prompts. (These are summarizer prompts, unrelated to
// the lens-registry directory format.)
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

// readPromptPair reads a lens directory's extract.md + review.md. Each whole file IS
// the prompt — there is no parsing and no section splitting, so a prompt can never be
// silently empty from a mis-parse: a missing extract.md is a hard, named error. review.md
// is allowed to be absent/empty (a lens may mine without a custom review prompt), but
// extract.md is required — it is the mining prompt, the reason the lens exists.
func readPromptPair(dir string) (extract, review string, err error) {
	e, err := os.ReadFile(filepath.Join(dir, ExtractFile))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", ExtractFile, err)
	}
	if strings.TrimSpace(string(e)) == "" {
		return "", "", fmt.Errorf("%s is empty (the mining prompt is required)", ExtractFile)
	}
	// review.md may not exist; treat absence as an empty review prompt rather than an error.
	r, rerr := os.ReadFile(filepath.Join(dir, ReviewFile))
	if rerr != nil && !os.IsNotExist(rerr) {
		return "", "", fmt.Errorf("read %s: %w", ReviewFile, rerr)
	}
	return string(e), string(r), nil
}

// loadDir loads a lens from a directory in the new format: lens.json (settings) +
// extract.md + review.md (prompts). The lens.json is OPTIONAL — a lens with none rides
// the defaults (name from the caller, no dimensions, no per-lens models) so a minimal
// two-prompt directory still loads. But a legacy directory holding ONLY a single
// lens.md (the pre-#75 sectioned format) is rejected with an actionable error rather
// than silently mis-loaded: the format changed, and re-registering is the fix.
func loadDir(dir, fallbackName string) (*Lens, error) {
	extract, review, err := readPromptPair(dir)
	if err != nil {
		// Distinguish "old sectioned lens.md, no new prompt files" from a genuinely broken
		// directory so the message tells the user exactly what to do.
		if _, statErr := os.Stat(filepath.Join(dir, ExtractFile)); os.IsNotExist(statErr) {
			if _, mdErr := os.Stat(filepath.Join(dir, "lens.md")); mdErr == nil {
				return nil, fmt.Errorf("lens %q is in the old single-file format (lens.md); the format changed to %s + %s + %s — re-register it with `witness lens register %s <dir>`", fallbackName, ConfigFile, ExtractFile, ReviewFile, fallbackName)
			}
		}
		return nil, err
	}
	l := &Lens{Name: fallbackName, Extract: extract, Review: review}
	// lens.json is optional; when present it supplies name/dimensions/models.
	if data, jerr := os.ReadFile(filepath.Join(dir, ConfigFile)); jerr == nil {
		var cfg LensConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", ConfigFile, err)
		}
		if strings.TrimSpace(cfg.Name) != "" {
			l.Name = strings.TrimSpace(cfg.Name)
		}
		l.Dimensions = cfg.Dimensions
		l.Runner = strings.TrimSpace(cfg.Runner)
		l.ExtractModel = strings.TrimSpace(cfg.ExtractModel)
		l.ReviewModel = strings.TrimSpace(cfg.ReviewModel)
	} else if !os.IsNotExist(jerr) {
		return nil, fmt.Errorf("read %s: %w", ConfigFile, jerr)
	}
	return l, nil
}

// LoadRegistered loads a centrally-registered lens by name from the registry dir
// (<root>/lenses/<name>/). Lenses are global and shared across sessions; the caller
// decides which are enabled. Returns an error if not registered or malformed.
func LoadRegistered(name, lensesDir string) (*Lens, error) {
	l, err := loadDir(filepath.Join(lensesDir, name), name)
	if err != nil {
		return nil, err
	}
	// Backstop the reserved-name guard at the RESOLVED name. RegisterLens/EnableLens
	// reject the registry name, but a lens.json `name` field can override that name
	// (loadDir above) — so a lens registered under an innocent name could still resolve
	// to a reserved identity (e.g. "default") and collide with the always-on built-in
	// on the shared (session,'default') watermark + observation key. This is the ultimate
	// boundary where the resolved name is known, so enforce it here too rather than trust
	// every caller to re-check.
	if store.ReservedLensName(l.Name) {
		return nil, fmt.Errorf("lens %q resolves to reserved name %q (its lens.json name impersonates the built-in/unified identity); rename it", name, l.Name)
	}
	l.Global = false
	return l, nil
}

// LoadFromDirUnchecked loads a candidate lens from an ARBITRARY directory path (not the
// registry) for preview/testing — the `witness lens try` path. It parses the directory
// the same way as a registered lens, but is intentionally LENIENT about identity: the
// name falls back to the directory's basename, then the literal "candidate", so a
// work-in-progress lens missing a lens.json name still previews. It does NOT apply the
// reserved-name gate (see LoadFromDir for the strict variant) — a preview never writes
// to the archive, so an impersonating name can't collide with anything. It still
// requires a non-empty extract.md (that is the prompt being previewed). Global is
// forced false — a candidate is never the always-on built-in.
func LoadFromDirUnchecked(dir string) (*Lens, error) {
	base := strings.TrimSpace(filepath.Base(strings.TrimRight(dir, string(filepath.Separator))))
	l, err := loadDir(dir, base)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(l.Name) == "" {
		l.Name = "candidate"
	}
	l.Global = false
	return l, nil
}

// LoadFromDir is the STRICT arbitrary-path loader: LoadFromDirUnchecked plus the
// reserved-name gate LoadRegistered enforces (a directory whose resolved name is a
// reserved identity — "default"/"unified", case-folded — is rejected). Callers that
// only PREVIEW (never write) may catch the reserved-name error and fall back to the
// Unchecked variant with a display name of "candidate"; callers that would persist
// under the resolved name must use this one.
func LoadFromDir(dir string) (*Lens, error) {
	l, err := LoadFromDirUnchecked(dir)
	if err != nil {
		return nil, err
	}
	if store.ReservedLensName(l.Name) {
		return nil, fmt.Errorf("lens dir %q resolves to reserved name %q (the built-in/unified identity); rename it in lens.json", dir, l.Name)
	}
	return l, nil
}
