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
	Importer
}

// Capturer is the optional capability for hook-fed platforms that write one L0
// record from a raw event payload. Best-effort by contract: capture must never
// break a session, so callers log the error and carry on. Reconcile-only
// platforms such as OpenCode deliberately do not implement it.
type Capturer interface {
	Capture(st store.CaptureStore, data []byte, now time.Time) (ok bool, err error)
}

// Importer pulls external native sessions into L0 (the reconcile path). An empty
// session list performs the platform's global incremental import; otherwise only
// the requested native session ids are reconciled. OpenCode reads its SQLite
// store, while Claude's hook-fed implementation is a no-op.
type Importer interface {
	Import(ctx context.Context, st store.ImportStore, sessionIDs []string) (ImportStats, error)
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

// ChunkPolicy is how a distillation pass tells a platform to shape a session's L0
// into model input. It is the whole cross-runtime chunking contract in one value:
//
//   - MaxChars is a per-input character budget (the local input model is plain text,
//     not token-metered). MaxChars <= 0 means NEVER chunk — render the whole session
//     as one transcript. This is the DEFAULT (store.Config.ChunkMaxChars = 0), and
//     the measured #57 conclusion is why: on arc/reasoning lenses, splitting a
//     session into independent windows loses ~70% of the observations (a bug found
//     early and fixed late spans the whole session; no single window holds the arc)
//     and inflates prose-drift ~20× (a context-less fragment induces the model to
//     converse instead of extract). So chunking is NOT a quality knob — it is a
//     LAST-RESORT timeout/OOM guard for a genuinely oversized session, opt-in via a
//     positive budget.
//
// It is a STRUCT rather than a bare int on purpose: it is the seam where lens-kind
// awareness (arc vs atomic) and chunk-then-merge reconciliation plug in later
// (#57's queued design) with no interface churn — a future field is added here and
// the two platforms read it, nothing else moves.
type ChunkPolicy struct {
	MaxChars int // per-input char budget; <=0 = send the whole session (default, best quality)
}

// InputRenderer shapes a session's raw L0 records into the one-or-more model inputs
// a distillation pass mines, under a ChunkPolicy. It is a PER-SESSION capability
// resolved by ForSession — deliberately independent of which engine runs the mining,
// so a Claude runner distilling imported OpenCode sessions shapes them by the same
// rule. Post-#57 the rule is SOURCE-AGNOSTIC: both hook-fed (Claude) and
// structured-log (OpenCode) sources mine whole by default and split only when a
// session overflows the policy budget. That parity is the fix for the CC-vs-OpenCode
// divergence (#56 B1) where OpenCode's unconditional chunking structurally
// under-extracted long arc-heavy sessions. The interface stays per-platform so a
// third runtime — or the future arc-aware merge path — can still shape differently.
type InputRenderer interface {
	// RenderInputs returns the model input(s) for a session's raw records: one whole
	// transcript when the policy budget is off or the session fits, or several
	// overlapping windows when it overflows a positive budget.
	RenderInputs(raw []store.RawRecord, policy ChunkPolicy) []string
}

// RenderTranscript renders raw records as "ROLE: text\n\n". It is the shared,
// source-neutral transcript format every runtime's RenderInputs emits — the whole
// session for a hook-fed source (Claude), one string per overlapping chunk for a
// structured-log source (OpenCode). It lives in the leaf platform package (like
// WrapCorpus) so both paths format identically from one source and a new runtime
// gets it for free — rather than one runtime importing another as a de-facto base.
func RenderTranscript(raw []store.RawRecord) string {
	var b strings.Builder
	for _, r := range raw {
		b.WriteString(strings.ToUpper(r.Role))
		b.WriteString(": ")
		b.WriteString(r.Text)
		b.WriteString("\n\n")
	}
	return b.String()
}

// chunkOverlapRecords is how many trailing records each split window re-includes at
// the head of the next, so a rule that straddles a boundary has a chance to appear
// whole in one window. It only matters when the policy budget forces a split.
const chunkOverlapRecords = 2

// RenderChunks is the ONE source-agnostic shaper every platform's RenderInputs
// delegates to (it lives here, beside RenderTranscript, so a new runtime gets both
// for free rather than importing another runtime as a de-facto base). It applies the
// #57 policy uniformly:
//
//   - policy.MaxChars <= 0  → the whole session as ONE transcript (the default and
//     the best-quality path; see ChunkPolicy for why chunking degrades arc lenses).
//   - policy.MaxChars > 0   → split into overlapping windows each under the budget,
//     a last-resort guard so a genuinely oversized session doesn't force a single
//     model call that times out / OOMs. A session that already fits still renders to
//     exactly one chunk, so turning the budget on is a no-op for normal sessions and
//     only bites the few giants (the measured 19-of-24 fit-whole finding).
//
// Splitting is greedy: accumulate records until the next one would exceed maxChars,
// emit that window, then rewind chunkOverlapRecords for the next. A single record
// larger than the budget is emitted alone (never dropped, never infinite-loops).
func RenderChunks(raw []store.RawRecord, policy ChunkPolicy) []string {
	if len(raw) == 0 {
		return nil
	}
	if policy.MaxChars <= 0 {
		return []string{RenderTranscript(raw)}
	}
	var chunks []string
	start := 0
	for start < len(raw) {
		end := start
		chars := 0
		for end < len(raw) {
			entryChars := len(raw[end].Role) + len(raw[end].Text) + 4
			if end > start && chars+entryChars > policy.MaxChars {
				break
			}
			chars += entryChars
			end++
		}
		if end == start {
			end++
		}
		chunks = append(chunks, RenderTranscript(raw[start:end]))
		if end >= len(raw) {
			break
		}
		next := end
		if chunkOverlapRecords > 0 && end-start > chunkOverlapRecords {
			next = end - chunkOverlapRecords
		}
		if next <= start {
			next = end
		}
		start = next
	}
	return chunks
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
// at startup, not a runtime condition. The name is normalized (lower+trim) with the
// SAME rule ByName uses, so a platform whose Name() is non-lowercase/padded is still
// resolvable (otherwise a third-party "Codex" would register yet be unreachable —
// the exact extensibility this package promises).
func Register(p Platform) {
	name := normalizeName(p.Name())
	if name == "" {
		panic("platform: Register called with empty Name()")
	}
	if _, dup := registry[name]; dup {
		panic("platform: duplicate registration for " + name)
	}
	registry[name] = p
}

// normalizeName is the ONE normalization rule for platform names, used by both
// Register and ByName so they can never key differently.
func normalizeName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// ByName returns the registered platform for name, or (nil, false) if unknown.
// Callers that must not silently accept an unknown platform (the fail-closed
// contract) check the bool.
func ByName(name string) (Platform, bool) {
	p, ok := registry[normalizeName(name)]
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
// and default, so this never fails and never needs a DB row to exist. It takes the
// narrow store.SessionPlatformReader (issue #73-C1), not the whole *store.Store, since
// resolution only reads the persisted owning-platform column. Callers pass either a
// live *store.Store or an untyped nil (the "no store available" path), so a plain
// nil-interface check is the right guard — the same intent as the former `st != nil`.
func ForSession(st store.SessionPlatformReader, session string) Platform {
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
