package store

import "time"

// Narrow consumer-facing interfaces (issue #73-C1, Phase B). These live IN package
// store — the bottom of the import stack — so consumers depend INWARD on the slice of
// the API they actually use, never on the whole *Store god-object. *Store satisfies
// every one of them by method promotion from its embedded concerns, so passing a real
// Store is unchanged; a consumer can now also be driven by a hand-written fake that
// implements just its interface, with no real *sql.DB (the whole point of C1 — see the
// worker's fake-backed test).
//
// Each interface is the EXACT method set one consumer needs, grouped by the concern it
// maps to. They are additive: adding a method a consumer newly needs is a one-line
// change here plus the consumer's own call. Compile-time assertions at the bottom keep
// *Store conformant so a drifting signature fails the build, not a caller.

// --- capture path (platform adapters) ----------------------------------------

// RawWriter is the L0 append surface the capture adapters use (Claude Code capture,
// live turn ingestion). NextSeq reads the current count to number the next turn.
type RawWriter interface {
	NextSeq(session string) int
	AppendRaw(r RawRecord) error
}

// SessionClassifier writes which platform owns a session (the per-session axis,
// issue #21). Both capture adapters stamp it at first sight.
type SessionClassifier interface {
	SetSessionPlatform(session, platform string)
}

// SessionPlatformReader reads the persisted owning platform of a session. It is the
// read-only counterpart platform.ForSession needs to resolve a session's runtime; a
// nil reader (or an unstamped session) falls through to the id-prefix rule.
type SessionPlatformReader interface {
	SessionPlatform(session string) string
}

// CaptureStore is what a hook-fed capture adapter needs: append raw turns numbered by
// the current count, and stamp the owning platform. It is the store slice threaded
// through platform.Capturer.Capture.
type CaptureStore interface {
	RawWriter
	SessionClassifier
}

// --- import path (platform adapters) ------------------------------------------

// MetaKV is the small-scalar bookkeeping an importer uses for its own durable
// watermarks (no schema ownership) — the metaKV concern's public surface.
type MetaKV interface {
	MetaString(key string) string
	SetMetaString(key, value string) error
}

// ImportStore is what a platform importer needs: its own source lock, watermark
// bookkeeping, the raw-import commit, a raw-count probe, and platform stamping. It is
// the store slice threaded through platform.Platform.Import (satisfied by *Store; the
// OpenCode Importer holds one).
type ImportStore interface {
	MetaKV
	SessionClassifier
	ImportLock(name string) (unlock func(), ok bool)
	RawCount(session string) int
	ApplyRawImport(meta SessionMeta, records []RawRecord, stateKey, stateValue string, replace bool) error
}

// --- MCP server ---------------------------------------------------------------

// ObservationReader reads L1 observations (embeddings decoded) for recall.
type ObservationReader interface {
	ReadObservations(lens string) ([]Observation, error)
}

// ActiveObservationSink is the in-session staging surface the MCP record tool uses:
// stage under a per-session cap, and disambiguate a cap-hit from a benign duplicate.
type ActiveObservationSink interface {
	StageObservationCapped(o Observation, limit int) (bool, error)
	StagedExists(session, obsID string) bool
}

// MCPStore is the exact store surface the MCP server needs (recall + record +
// profile/facets reads + a targeted observation delete). Passing this instead of
// *Store lets the server be built against a fake.
type MCPStore interface {
	ObservationReader
	ActiveObservationSink
	ReadFacets() ([]Facet, error)
	ReadProfile(lens string) (string, bool, error)
	DeleteObservation(obsID string) (bool, error)
}

// --- distillation engine (the C1 headline: a fake-drivable worker) ------------

// Queue is the distillation-queue surface the Worker drives: read the L0/L1 inputs,
// commit L1 output, and advance the per-(session,lens) watermark / backoff / drift.
// This is the interface that lets the worker run against a fake with NO real *sql.DB
// — the concrete deliverable of #73-C1. It is exactly the method set worker.go +
// drain.go call, nothing wider.
type Queue interface {
	// inputs
	ReadRawSnapshot(session string) (recs []RawRecord, rawHighID int64, err error)
	RawCount(session string) int
	ReadObservations(lens string) ([]Observation, error)
	DrainStaged(session string) (obs []Observation, throughID int64, err error)
	// per-(session,lens) watermark / readiness
	DistilledCount(session, lens string) int
	LensBackedOff(session, lens string, now time.Time) bool
	PendingInputChars(session string, lenses []string) int
	// commit + advance
	AppendObservations(obs []Observation) error
	MarkDistilledIfCurrent(session, lens string, count int, rawHighID int64) (bool, error)
	ClearStagedThrough(session string, throughID int64)
	// retry / backoff / drift bookkeeping
	IncRetry(session, lens string) int
	ResetRetry(session, lens string)
	SetNextAttempt(session, lens string, at time.Time) error
	SetDrift(session, lens string) error
	RecordDrift(n int, session, lens string) error
}

// ReviewStore is the surface the Reviewer drives: read L1 (embeddings slimmed off),
// replace the L2 facet profile, and stamp the review cadence.
type ReviewStore interface {
	ReadFacets() ([]Facet, error)
	ReadObservationsLite(lens string) ([]Observation, error)
	WriteFacets(facets []Facet) error
	StampReview() error
}

// SummaryStore is the surface the Summarizer drives: read facets, read/write the L4
// narrative profile files, and its own meta watermark for change tracking.
type SummaryStore interface {
	MetaKV
	ReadFacets() ([]Facet, error)
	ReadProfile(lens string) (string, bool, error)
	WriteProfile(lens, markdown string) error
}

// Compile-time proof that *Store satisfies every consumer interface. If a concern
// method signature drifts, THIS breaks — a clear store-package failure — rather than
// a distant consumer or fake.
var (
	_ CaptureStore          = (*Store)(nil)
	_ ImportStore           = (*Store)(nil)
	_ MCPStore              = (*Store)(nil)
	_ Queue                 = (*Store)(nil)
	_ ReviewStore           = (*Store)(nil)
	_ SummaryStore          = (*Store)(nil)
	_ ObservationReader     = (*Store)(nil)
	_ MetaKV                = (*Store)(nil)
	_ RawWriter             = (*Store)(nil)
	_ SessionClassifier     = (*Store)(nil)
	_ SessionPlatformReader = (*Store)(nil)
	_ ActiveObservationSink = (*Store)(nil)
)
