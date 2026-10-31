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

import (
	"math"
	"strings"
)

// Name constants — the single source of truth for the two special names, in the data
// layer (every store-side check and the lens loader route through them). Since #44
// slice 1a only ProfileUnified is RESERVED; LensDefault is an ordinary lens name kept
// here as a constant because the seed/migration reference it:
//   - LensDefault IS a lens (the person-growth scaffold witness seeds — an ordinary
//     registered lens with no always-on privilege since #44 slice 1a).
//   - ProfileUnified is NOT a lens; it is the reserved filename stem of the cross-lens
//     profile aggregate. Naming it Lens* would be a misnomer — nothing mines "unified".
const (
	// LensDefault is the name of the seeded person-growth lens (an ordinary registered
	// lens since #44 slice 1a; the seed/migration + install scaffold reference it).
	LensDefault = "default"
	// ProfileUnified is the filename stem of the cross-lens profile summary
	// (profile/unified.md) — an aggregate VIEW blending every lens, NOT a lens a
	// session mines. The name is RESERVED (a registered lens may not take it) purely
	// so a per-lens summary can't clobber this profile file. The summarize writer and
	// every profile reader must agree on this exact string, or a profile written to
	// unified.md would be read under a different key.
	ProfileUnified = "unified"
)

// RawRecord is one raw turn-half: a user prompt or an assistant reply, captured
// verbatim from the stable hook fields (UserPromptSubmit.prompt / Stop.last_assistant_message).
type RawRecord struct {
	TS      string `json:"ts"`      // RFC3339 capture time
	Session string `json:"session"` // session_id from the hook payload
	Seq     int    `json:"seq"`     // monotonic per-session ordinal
	Role    string `json:"role"`    // "user" | "assistant"
	// Effort is CAPTURED AND STORED BUT NEVER RENDERED — deliberately recorded here so the next
	// dead-code sweep does not re-derive it. claude/capture.go reads it from the hook payload and
	// raw.go persists and scans it, but platform.RenderTranscript — the ONE path from L0 to a model
	// prompt — emits only role + text, so the effort level reaches SQLite and stops there. The
	// OpenCode importer never sets it at all.
	//
	// KEPT rather than removed: this is L0, the append-only ground truth, so dropping a persisted
	// column costs a migration on every live archive and buys nothing. If effort SHOULD influence
	// distillation, the fix is to render it in RenderTranscript — not to delete the field.
	Effort string `json:"effort,omitempty"` // assistant effort level, if present
	Text   string `json:"text"`             // verbatim content
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
	Rowid       int64     `json:"-"`                   // observations.seq (monotonic AUTOINCREMENT surrogate, #125); set only by ReadObservationsSinceOrdered (the windowed-fold cursor, #123). Not persisted/serialized.
}

// FacetVersion is one bi-temporal version of a facet's value. Per the invalidation
// rule: ValidTo is set ONLY on positive evidence the window ended — a sustained
// contradicting pattern, or recency-expiry for time-bound State. Never on mere
// absence (that only lowers Confidence). Old versions are retained, never deleted.
type FacetVersion struct {
	Value string `json:"value"` // the facet value at this time
	// Claim is the short statement of what this version ASSERTS — one or two sentences,
	// separate from the analysis in Value. It is what the review call is sent: enough to match
	// identity and judge contradiction, without the contrasts, advice, provenance notes and
	// falsifiers that make up most of the prose and are re-sent on every review otherwise.
	// Empty means none has been written yet, and every consumer falls back to Value.
	Claim      string   `json:"claim,omitempty"`
	ValidFrom  string   `json:"valid_from"`         // real-world: when this became true
	ValidTo    string   `json:"valid_to,omitempty"` // real-world: when it stopped (empty = current)
	RecordedAt string   `json:"recorded_at"`        // system: when the reviewer learned it
	BecauseOf  []string `json:"because_of"`         // observation IDs (provenance for re-grounding)
	// Confidence is how sure the reviewer was WHEN THE EVIDENCE LAST ARRIVED — not how sure we
	// should be now. Read it through DecayedConfidence, which accounts for the observations
	// that have gone by since without confirming it.
	Confidence float64 `json:"confidence"` // 0-1; rises on reinforcement, decays on absence
	// ConfidenceSeq is the observation watermark at which Confidence was set: the clock decay
	// is measured against. 0 means unknown (a facet written before this was tracked), which
	// DecayedConfidence treats as "no decay" rather than "infinitely stale".
	ConfidenceSeq int64 `json:"confidence_seq,omitempty"`
}

// Decay constants for DecayedConfidence. Exported so a caller can explain the numbers it shows.
const (
	// FacetDecayHalfLifeObs is how many observations may pass, without a facet being cited,
	// before its confidence halves.
	//
	// Measured on a real archive: a facet is cited about once every 1,486 observations in the
	// `default` lens (22,501 citations across 16,353 observations for 2,039 facets) and 886 in
	// `math`. At this half-life a facet confirmed at 0.9 reads ~0.64 after one typical gap —
	// visibly less certain, not alarming — and reaches the floor after roughly 10,000
	// uncited observations, which is a facet that genuinely stopped showing up.
	//
	// The rate is the one real judgement call here. Too gentle and every facet pins at
	// ceiling, which is the ratchet this fixes wearing a different hat; too aggressive and a
	// quiet-but-real trait reads as doubtful. It is deliberately a named constant rather than
	// a literal so the number can be argued with.
	FacetDecayHalfLifeObs = 3000.0

	// FacetConfidenceFloor is the value decay asymptotes to and never passes.
	//
	// It is load-bearing, not a safety rail. The invalidation rule above applyFacet is explicit
	// that absence must NEVER invalidate a facet — only lower confidence — so a decay that
	// could reach 0 would quietly turn "no evidence lately" into "not true", which is the one
	// inference this archive must not make. A quiet trait stays on the books, flagged as
	// uncertain, and one citation restores it.
	FacetConfidenceFloor = 0.1
)

// DecayedConfidence is what a reader should see: the stored confidence decayed for the
// observations that have passed since it was set, floored at FacetConfidenceFloor.
//
// currentSeq is the archive's high-water observation seq. A zero ConfidenceSeq (a facet that
// predates the column) or a currentSeq at or below it means no elapsed evidence, so no decay —
// never the reverse, because guessing "infinitely stale" would decay a whole archive to the
// floor on first read.
func (v *FacetVersion) DecayedConfidence(currentSeq int64) float64 {
	if v.ConfidenceSeq <= 0 || currentSeq <= v.ConfidenceSeq {
		return v.Confidence
	}
	if v.Confidence <= FacetConfidenceFloor {
		return v.Confidence // already at or under the floor; decay has nothing to do
	}
	elapsed := float64(currentSeq - v.ConfidenceSeq)
	decayed := v.Confidence * math.Pow(0.5, elapsed/FacetDecayHalfLifeObs)
	if decayed < FacetConfidenceFloor {
		return FacetConfidenceFloor
	}
	return math.Round(decayed*100) / 100
}

// Facet is the evolving record of one named attribute within a lens+dimension,
// e.g. lens="default", dimension="thinking", key="resolving_uncertainty".
// Its Versions slice IS the change history — the whole point of the system.
//
// ArchivedAt/MergedInto are the STRUCTURAL half of that history. Versions record how one
// facet's value moved; these record how the set of facets itself was reshaped — which is a
// different kind of change and was previously not recorded at all, because nothing ever
// removed or combined a facet. A facet is never deleted: it is archived, keeping its full
// version history, and the active profile is the derived subset where ArchivedAt is empty.
type Facet struct {
	Lens      string         `json:"lens"`
	Dimension string         `json:"dimension"`
	Key       string         `json:"key"`       // emergent within the dimension
	LastSeen  string         `json:"last_seen"` // most recent supporting observation
	Versions  []FacetVersion `json:"versions"`  // append-as-it-changes; current = ValidTo==""

	// ArchivedAt is when compaction took this facet out of the active profile (empty =
	// active). Archived facets stay in the database with every version intact; they are
	// simply no longer shown to the model or served as current state.
	ArchivedAt string `json:"archived_at,omitempty"`
	// MergedInto is the "dimension/key" of the facet that absorbed this one, empty when the
	// facet was retired outright rather than folded into another. It is the LINK that makes
	// an active facet expandable: given a survivor, the facets naming it here are its
	// tributaries, and theirs in turn, so a compact node unfolds into the full trajectory
	// that produced it. Stored as text, not a row id, deliberately — WriteFacets replaces
	// every row on each review, so ids are not stable across writes but lens|dimension|key
	// is the facet's real identity (applyFacet keys on it too).
	MergedInto string `json:"merged_into,omitempty"`
	// ArchiveReason is the model's one-line justification for the merge or retirement. It is
	// persisted because compaction reshapes the profile on a model's judgment: the link alone
	// tells you THAT nine facets became one, never why it judged them one trait. Without it
	// the audit trail for the one destructive-looking operation in witness lives in a log line
	// that rotates away.
	ArchiveReason string `json:"archive_reason,omitempty"`
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

// Gist returns what to show a model or a listing: the short claim when one exists, else the
// full value. The fallback is what lets an archive that has never been compacted keep working
// unchanged — and what makes adding claims a progressive improvement rather than a migration.
func (v *FacetVersion) Gist() string {
	if strings.TrimSpace(v.Claim) != "" {
		return v.Claim
	}
	return v.Value
}

// Active reports whether this facet is part of the live profile — the set that feeds the
// model, the portrait, and MCP. Compaction archives rather than deletes, so "all facets" and
// "the current profile" are no longer the same question.
func (f *Facet) Active() bool { return f.ArchivedAt == "" }

// FacetRef is the "dimension/key" form used by MergedInto and by the compaction prompt. The
// lens is implicit: compaction only ever groups facets within one lens.
func FacetRef(dimension, key string) string { return dimension + "/" + key }

// ParseFacetRef splits a FacetRef back into its parts. Dimensions and keys are snake_case
// slugs, so the first "/" is the separator; ok is false for anything that is not a well-formed
// ref (an empty MergedInto, or a value from an older/hand-edited database).
func ParseFacetRef(ref string) (dimension, key string, ok bool) {
	i := strings.Index(ref, "/")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}
