package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// unknownRunnerError is the fail-closed error when a runner name doesn't resolve to
// a registered RunnerProvider (a typo'd cfg.Runner / WITNESS_RUNNER, or a name with
// no runner capability). Surfacing it beats silently falling back to a default.
func unknownRunnerError(name string) error {
	return fmt.Errorf("unknown distillation runner %q (want claude or opencode)", name)
}

// Platform is one agent runtime (Claude Code, OpenCode, …) as a first-class type,
// replacing the bare "claude"/"opencode" strings that were re-parsed across the
// engine (issue #21). It composes small capability interfaces; PR3a introduces
// the per-session ones (Identity, InputRenderer). Distillation-engine capabilities
// (Runner) and install/capture/import capabilities are folded in by later PRs.
//
// Two axes, deliberately kept separate (see the two registry lookups below):
//   - PER-SESSION owner: which platform PRODUCED a session (by id prefix / the
//     persisted session_meta.platform). Governs how L0 is shaped into model input.
//   - GLOBAL runner: which engine DISTILLS (one for the whole process). Not here.
type Platform interface {
	Identity
	InputRenderer
	Capturer
	Importer
}

// Capturer writes one L0 record from a raw hook/event payload. The []byte
// signature is uniform across platforms: Claude unmarshals its typed HookEvent
// internally, OpenCode parses its event JSON. Best-effort by contract — capture
// must never break a session, so callers log the error and carry on. ok reports
// whether a record was actually written (false = ignored/duplicate payload, not an
// error). Implementations also record the session's owning platform
// (SetSessionPlatform) so ForSession is column-authoritative going forward.
type Capturer interface {
	Capture(st *store.Store, data []byte, now time.Time) (ok bool, err error)
}

// Importer pulls external native sessions into L0 (the reconcile path). OpenCode
// reads its SQLite store (taking the sync lock internally); Claude is hook-fed, so
// its Import is a no-op returning zero stats. This collapses the importcmd switch.
type Importer interface {
	Import(ctx context.Context, st *store.Store) (ImportStats, error)
}

// Identity is a platform's naming/namespacing surface: its stable name and the
// session-id prefix that marks a session as belonging to it in L0.
type Identity interface {
	// Name is the stable identifier persisted in config/meta ("claude", "opencode").
	Name() string
	// SessionPrefix is prepended to native session ids in L0 to namespace them
	// ("opencode:"); "" means unprefixed (Claude — the default/unmarked source).
	SessionPrefix() string
}

// InputRenderer shapes a session's raw L0 records into the one-or-more model
// inputs a distillation pass mines. This is a PER-SESSION capability resolved by
// ForSession — deliberately independent of which engine runs the mining, so a
// Claude runner distilling imported OpenCode sessions still chunks them correctly.
type InputRenderer interface {
	// RenderInputs returns the model input(s) for a session's raw records: a single
	// transcript for hook-fed sources, or several overlapping chunks for sources
	// with long structured logs.
	RenderInputs(raw []store.RawRecord) []string
}

// registry holds the registered platforms, keyed by Name(). It is populated by
// each platform subpackage's init(), so importing a subpackage (even blank) makes
// its platform available. Registration happens at process start before any
// lookup, so no lock is needed for reads.
var registry = map[string]Platform{}

// defaultName is the platform a session with no recognized prefix belongs to.
// This encodes the asymmetric rule "unmarked == Claude" that predates the
// registry (CC sessions were never prefixed), so old archives resolve unchanged.
const defaultName = "claude"

// Register adds a platform to the registry. Intended for a subpackage init();
// panics on a duplicate or empty name, since that is a programming error caught
// at startup, not a runtime condition.
func Register(p Platform) {
	name := p.Name()
	if name == "" {
		panic("platform: Register called with empty Name()")
	}
	if _, dup := registry[name]; dup {
		panic("platform: duplicate registration for " + name)
	}
	registry[name] = p
}

// ByName returns the registered platform for name, or (nil, false) if unknown.
// Callers that must not silently accept an unknown platform (the fail-closed
// contract) check the bool.
func ByName(name string) (Platform, bool) {
	p, ok := registry[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// Default returns the default platform (Claude), the owner of any session with no
// recognized prefix. Panics if it was never registered — that means the binary
// forgot to blank-import the claude subpackage, a build-wiring bug.
func Default() Platform {
	p, ok := registry[defaultName]
	if !ok {
		panic("platform: default platform " + defaultName + " not registered (missing blank import?)")
	}
	return p
}

// ForSession resolves the PER-SESSION owning platform, in priority order:
//
//  1. the persisted session_meta.platform column (authoritative once written by
//     capture/import, and backfilled for existing rows at migration);
//  2. else the L0 session-id prefix (a registered platform whose SessionPrefix
//     matches), so a row that predates the column still resolves correctly;
//  3. else Default() (Claude — the unmarked source).
//
// st may be nil (or the column empty) — resolution then falls through to prefix
// and default, so this never fails and never needs a DB row to exist.
func ForSession(st *store.Store, session string) Platform {
	if st != nil {
		if name := st.SessionPlatform(session); name != "" {
			if p, ok := ByName(name); ok {
				return p
			}
		}
	}
	return forSessionByPrefix(session)
}

// forSessionByPrefix is the prefix-only resolution (step 2+3), split out so it can
// be used where no store is available and unit-tested directly.
func forSessionByPrefix(session string) Platform {
	for _, p := range registry {
		if pre := p.SessionPrefix(); pre != "" && strings.HasPrefix(session, pre) {
			return p
		}
	}
	return Default()
}
