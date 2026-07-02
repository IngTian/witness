// Package store defines the on-disk data model and IO for the witness layers,
// plus the filesystem-as-queue used by the detached worker.
//
// Layering (raw -> observations -> facets -> profile; L0/L1/L2 are shorthand):
//
//	raw          — GROUND TRUTH transcript, append-only, never LLM-touched   (L0)
//	observations — derived, append-only; written ONLY by the worker          (L1)
//	facets       — derived, bi-temporal; written ONLY by the reviewer        (L2)
//	profile      — narrative summary distilled from facets; human-readable   (L4)
//
// (The Lx tags are shorthand; L3 is intentionally unused — the profile is a prose
// rendering that sits directly on top of the facets.)
//
// Every observation and facet carries a `lens` tag. The "default" lens is
// global (runs on every session); repo lenses are scoped (opt-in per repo).
package store

// Lens tag constants. Default is the always-on, cross-domain lens.
const LensDefault = "default"

// RawRecord is one raw turn-half: a user prompt or an assistant reply, captured
// verbatim from the stable hook fields (UserPromptSubmit.prompt / Stop.last_assistant_message).
type RawRecord struct {
	TS      string `json:"ts"`               // RFC3339 capture time
	Session string `json:"session"`          // session_id from the hook payload
	Seq     int    `json:"seq"`              // monotonic per-session ordinal
	Role    string `json:"role"`             // "user" | "assistant"
	Effort  string `json:"effort,omitempty"` // assistant effort level, if present
	Text    string `json:"text"`             // verbatim content
}

// SessionMeta records non-content facts about a session that the worker needs —
// chiefly the cwd, so it can resolve which repo (and thus which opted-in lens)
// a session belongs to. Written once, on first capture of a session.
type SessionMeta struct {
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
	Started string `json:"started"`
}

// Observation is an atomic, evidence-anchored thing the distiller noticed in one
// session, tagged by lens. Immutable once written. Poignancy (1-10) drives the
// review trigger. Source is "mined" (worker-extracted) or "active" (recorded
// in-session via MCP and passed through verbatim).
type Observation struct {
	ID          string    `json:"id"`                  // stable id, e.g. "obs_<hash>"
	TS          string    `json:"ts"`                  // when the underlying moment occurred
	Session     string    `json:"session"`             // originating session_id
	Lens        string    `json:"lens"`                // "default" | repo lens name
	Dimension   string    `json:"dimension"`           // lens-defined axis (e.g. "thinking")
	Observation string    `json:"observation"`         // the noticed thing, one sentence
	Evidence    string    `json:"evidence"`            // short verbatim/paraphrase anchor into L0
	Poignancy   int       `json:"poignancy"`           // 1-10 salience (drives review trigger)
	Source      string    `json:"source"`              // "mined" | "active"
	Embedding   []float32 `json:"embedding,omitempty"` // 384-d e5-small vector
}

// FacetVersion is one bi-temporal version of a facet's value. Per the invalidation
// rule: ValidTo is set ONLY on positive evidence the window ended — a sustained
// contradicting pattern, or recency-expiry for time-bound State. Never on mere
// absence (that only lowers Confidence). Old versions are retained, never deleted.
type FacetVersion struct {
	Value      string   `json:"value"`              // the facet value at this time
	ValidFrom  string   `json:"valid_from"`         // real-world: when this became true
	ValidTo    string   `json:"valid_to,omitempty"` // real-world: when it stopped (empty = current)
	RecordedAt string   `json:"recorded_at"`        // system: when the reviewer learned it
	BecauseOf  []string `json:"because_of"`         // observation IDs (provenance for re-grounding)
	Confidence float64  `json:"confidence"`         // 0-1; decays on absence, rises on reinforcement
}

// Facet is the evolving record of one named attribute within a lens+dimension,
// e.g. lens="default", dimension="thinking", key="resolving_uncertainty".
// Its Versions slice IS the change history — the whole point of the system.
type Facet struct {
	Lens      string         `json:"lens"`
	Dimension string         `json:"dimension"`
	Key       string         `json:"key"`       // emergent within the dimension
	LastSeen  string         `json:"last_seen"` // most recent supporting observation
	Versions  []FacetVersion `json:"versions"`  // append-as-it-changes; current = ValidTo==""
}

// Current returns the live version of a facet (the one with no ValidTo), or nil.
func (f *Facet) Current() *FacetVersion {
	for i := len(f.Versions) - 1; i >= 0; i-- {
		if f.Versions[i].ValidTo == "" {
			return &f.Versions[i]
		}
	}
	return nil
}
