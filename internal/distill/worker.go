package distill

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
)

// errNoArray is mine()'s sentinel for "the model replied, but its reply contained
// NO parseable JSON observation array" — i.e. prose_drift: the runner conversed with
// the transcript (or refused, or was truncated) instead of emitting the required
// array. This is DISTINCT from the model emitting an explicit empty `[]` (a legit
// "nothing to report" — ParseJSONArray returns that as (empty, nil), see parse.go).
// The empirical work on #57 found a too-weak triage model does this on ~40% of
// sessions; before this sentinel mine() silently bucketed it as a quiet session,
// making a below-floor model indistinguishable from a genuinely uneventful history.
// It is wrapped (kept as an error, not a bool) so MineSession can match it with
// errors.Is WITHOUT changing mine()'s (obs, error) signature — a transport failure
// stays a plain error, so the two failure modes never get conflated.
var errNoArray = errors.New("mine: model reply contained no JSON observation array (prose drift)")

// backoffDelay is the wait before a failed session's delta is retried: 5m, 10m,
// 20m, ... doubling, capped at 6h. The raw turns are NEVER dropped — a transient
// outage (rate limit, network) just delays distillation until it clears.
const (
	backoffBase = 5 * time.Minute
	backoffCap  = 6 * time.Hour
)

func backoffDelay(retries int) time.Duration {
	d := backoffBase
	for i := 1; i < retries && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	return d
}

// Embedder is the slice of the embedder the worker needs. An interface (not the
// concrete *embed.Embedder) so tests can inject a fake without the 470MB model.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// MineFunc runs one extraction pass (one LLM call). Injectable so tests can drive
// the worker without spawning a real model. It is the narrow seam the Worker/
// Reviewer/Summarizer actually call; production wires it to a Runner (see
// runnerMine), tests supply a fake directly.
type MineFunc func(ctx context.Context, model, prompt, input string) (string, error)

// RunnerMine adapts a platform.Runner into the MineFunc seam. This keeps the
// injectable MineFunc for tests while routing production through the single Runner
// (platform.RunnerFor) resolved once per drain. distill knows only platform.Runner
// — never which runtime it is.
func RunnerMine(r platform.Runner) MineFunc {
	return func(ctx context.Context, model, prompt, input string) (string, error) {
		return r.Run(ctx, model, prompt, input)
	}
}

// dedupThreshold: a mined observation whose nearest existing same-lens neighbor
// scores above this is treated as a duplicate and dropped. e5 cosines run high,
// so this is deliberately strict.
const dedupThreshold = 0.93

// Worker processes ONE session's L0 into L1. It is the sole writer of L1 and the
// sole place active + mined observations are combined.
//
// Combine rule (preserves hand-authored quality):
//   - active observations: passed through VERBATIM (authoritative; never re-distilled)
//   - mined observations: kept only if not a near-duplicate of an active one or an
//     already-stored same-lens observation
type Worker struct {
	Store    *store.Store
	Embedder Embedder
	Lenses   []*lens.Lens // default (always) + any config-enabled lenses; all global, applied to every session regardless of source (CC or OpenCode)
	Config   store.Config
	Run      MineFunc // required; production wires RunnerMine(NewRunner(cfg)), tests inject a fake
}

// SessionMining is the result of the MAP half of a distillation pass: everything
// mined for one session that does NOT touch the store's write path or depend on
// any other session. It is produced by MineSession (safe to run for many sessions
// concurrently) and consumed by CommitMining (serial, the sole L1 writer). The
// split is what lets the engine parallelize the expensive LLM mining while keeping
// dedup + writes single-threaded and correct (issue #22).
type SessionMining struct {
	Session         string
	Total           int    // len(raw) at read time — the watermark to advance to on success
	RawHighID       int64  // MAX(raw.id) at read time — the raw "generation" this mine saw (issue #49 C2)
	SessionTS       string // recency anchor for observations lacking their own ts
	StagedThroughID int64  // how far staged was drained, to clear exactly those rows on success
	Active          []store.Observation
	// Per-lens mining results (issue #55). The watermark is per-(session,lens), so
	// each lens's mined observations, its own start watermark (Done), and whether ITS
	// call failed are tracked independently: a codereview transport failure backs off
	// only codereview and never discards a healthy default mine for the same session.
	Lenses      []LensMining
	NothingToDo bool // no new turns and nothing staged — commit just advances every lens's watermark
}

// LensMining is one lens's slice of a session's mining pass.
type LensMining struct {
	Lens       string
	Done       int // this lens's watermark at read time (turns it had already mined)
	Mined      []store.Observation
	MineFailed bool // a transport error hit THIS lens — commit backs off this lens only
	// Drifted marks that this lens saw prose_drift (a reply with no JSON array,
	// errNoArray) AND produced ZERO observations across ALL of its inputs (#57). It is
	// set only in that all-inputs-drifted-nothing case: a multi-chunk session where one
	// chunk yields a real array is NOT drift (the lens produced obs), so this can't
	// false-positive a partially-successful long session. A drifted lens ADVANCES its
	// watermark exactly like a legit-quiet lens (the data outcome is identical to the
	// pre-#57 silent behavior) — the ONLY difference is that commit counts + surfaces
	// it, so a below-floor triage model becomes visible instead of masquerading as an
	// uneventful history. It is NOT a MineFailed: drift never backs off (that would
	// re-hammer a deterministically-drifting model forever and wedge the backfill queue).
	Drifted bool
}

// Process runs a full fast-path pass for one session: MineSession then CommitMining
// against a fresh store snapshot. It is the serial entry point kept for the
// single-session callers and every existing test. The parallel drain calls
// MineSession (concurrently) and CommitMining (serially) directly instead.
func (w *Worker) Process(ctx context.Context, session string) error {
	m, err := w.MineSession(ctx, session)
	if err != nil {
		return err
	}
	existing, _ := w.Store.ReadObservations("")
	return w.CommitMining(m, &existing)
}

// MineSession is the MAP half: read the delta, drain staged, and run every lens's
// extract prompt (the LLM calls — the expensive, parallelizable part). It performs
// NO L1 writes and reads NO cross-session state, so the engine may call it for many
// sessions concurrently. Embeddings for active obs are computed here too (the
// embedder guards itself with a mutex, so concurrent callers serialize on it
// briefly — cheap relative to the LLM call). All dedup-against-corpus and writes
// happen later in CommitMining.
func (w *Worker) MineSession(ctx context.Context, session string) (*SessionMining, error) {
	raw, err := w.Store.ReadRaw(session)
	if err != nil {
		return nil, fmt.Errorf("read L0: %w", err)
	}
	total := len(raw)

	// Capture the raw "generation" this mine reads — the highest raw.id. CommitMining
	// advances each lens's watermark only if this id still exists, so a replace-import/
	// cleanup that deletes raw mid-mine can't have a stale count blind-written over it
	// (#49 C2). The generation is session-level (a property of raw), shared by all lenses.
	rawHighID := w.Store.MaxRawID(session)

	m := &SessionMining{Session: session, Total: total, RawHighID: rawHighID, StagedThroughID: 0}
	if total > 0 {
		m.SessionTS = raw[total-1].TS
	}

	// 1. Active observations (staged in-session via MCP) — authoritative. We drain
	// them and remember how far (StagedThroughID) so CommitMining can delete exactly
	// those rows after they're written; on a write failure they stay for retry.
	active, stagedThroughID, _ := w.Store.DrainStaged(session)
	for i := range active {
		active[i].Source = "active"
	}
	m.Active = active
	m.StagedThroughID = stagedThroughID

	// 2. Mine each lens over ITS OWN delta (issue #55). The watermark is per-
	// (session,lens), so a lens caught up to `total` has nothing to do, while a
	// just-enabled lens (watermark 0) mines the whole session — this is what lets a
	// new lens backfill without re-mining `default`. Per-lens failure is isolated: a
	// transport error on one lens's call sets only that lens's MineFailed, so its
	// delta stays pending and retries while healthy sibling lenses still commit and
	// advance. (A parse-miss is NOT a failure — mine() returns it as a quiet session,
	// so the lens still advances past an uneventful delta.)
	now := time.Now()
	anyDelta := false
	for _, ln := range w.Lenses {
		done := w.Store.DistilledCount(session, ln.Name)
		// Honor this lens's OWN retry backoff even though the SESSION was offered. The
		// pending query is session-granular (it offers a session while ANY active lens
		// is behind-and-ready), so a healthy sibling lens keeps the session in the queue
		// while THIS lens is still sleeping out a failure. Without this gate MineSession
		// would re-run a backed-off lens's `claude -p` on every sibling-driven drain —
		// hammering exactly the failing lens the backoff exists to spare (issue #55; the
		// offer gate is per-lens-aware, the mining loop must be too). Skip it ENTIRELY:
		// CommitMining only advances lenses present in m.Lenses, so a skipped lens keeps
		// its watermark AND its next_attempt untouched and retries once the backoff
		// elapses and the pending query re-offers the session for it.
		if total > done && w.Store.LensBackedOff(session, ln.Name, now) {
			continue
		}
		lm := LensMining{Lens: ln.Name, Done: done}
		if total > done {
			anyDelta = true
			// Track drift across ALL of this lens's inputs (a session may render to
			// several chunks). We only flag the lens as Drifted if it produced NO obs
			// anywhere AND at least one input drifted — so a long session where one chunk
			// extracts fine is never miscounted as drift (see LensMining.Drifted).
			producedObs, sawDrift := false, false
			for _, transcript := range distillInputs(w.Store, session, raw[done:]) {
				obs, err := w.mine(ctx, ln, session, transcript)
				if err != nil {
					if errors.Is(err, errNoArray) {
						// prose drift: the model replied but emitted no array (likely below the
						// lens's model floor). NOT a transport failure — do not back off; the
						// watermark advances like a quiet session, and commit surfaces the drift.
						sawDrift = true
						slog.Warn("distill: prose drift; model may be below lens floor (advancing, surfaced)",
							"session", session, "lens", ln.Name, "err", err)
						continue
					}
					// A real transport error (rate limit, network, timeout) — back this lens off.
					slog.Error("mine failed", "session", session, "lens", ln.Name, "err", err)
					lm.MineFailed = true
					continue
				}
				if len(obs) > 0 {
					producedObs = true
				}
				lm.Mined = append(lm.Mined, obs...)
			}
			lm.Drifted = sawDrift && !producedObs
		}
		m.Lenses = append(m.Lenses, lm)
	}

	// Nothing new to mine for any lens and nothing staged → commit is a no-op
	// (every lens already at the watermark; the CAS stamp would be idempotent).
	if !anyDelta && len(active) == 0 {
		m.NothingToDo = true
		return m, nil
	}

	// 3. Embed active + each lens's mined so dedup + later recall work. Done in the
	// map phase so the serial commit stays cheap; the embedder's own mutex makes this
	// concurrency-safe. A mined obs whose embedding fails is dropped now — same as
	// before, where the combine loop skipped it on Embed error.
	for i := range m.Active {
		if len(m.Active[i].Embedding) == 0 {
			if v, err := w.Embedder.Embed(m.Active[i].Observation); err == nil {
				m.Active[i].Embedding = v
			}
		}
	}
	for li := range m.Lenses {
		kept := m.Lenses[li].Mined[:0]
		for i := range m.Lenses[li].Mined {
			v, err := w.Embedder.Embed(m.Lenses[li].Mined[i].Observation)
			if err != nil {
				continue
			}
			m.Lenses[li].Mined[i].Embedding = v
			kept = append(kept, m.Lenses[li].Mined[i])
		}
		m.Lenses[li].Mined = kept
	}
	return m, nil
}

// CommitMining is the REDUCE half: given one session's mining result and a pointer
// to the RUNNING corpus snapshot, dedup the mined observations, write L1, advance
// the watermark, and clear staged rows. It is the SOLE L1 writer and MUST run
// serially. `existing` is threaded by pointer and APPENDED with each session's
// newly-written observations, so a later session in the same drain dedups against
// an earlier one's writes — strictly better than a per-session fresh snapshot, and
// what makes parallel mining safe without a cross-session dedup gap.
func (w *Worker) CommitMining(m *SessionMining, existing *[]store.Observation) error {
	if m.NothingToDo {
		// Every lens is already at the watermark; the stamp is idempotent. Advance each
		// lens (CAS-guarded per #49 C2 so a raw replace/cleanup mid-mine can't blind-
		// advance a generation we didn't see — not advancing leaves the pair pending).
		for _, lm := range m.Lenses {
			if _, err := w.Store.MarkDistilledIfCurrent(m.Session, lm.Lens, m.Total, m.RawHighID); err != nil {
				return err
			}
		}
		return nil
	}

	// Combine what to write: active verbatim + each SUCCESSFUL lens's mined minus
	// near-duplicates (of active or of the running corpus, which includes earlier
	// sessions' writes in THIS drain). A lens that hit a transport failure contributes
	// nothing this round; its delta stays pending and re-mines when the failure clears
	// (S1 at-least-once, NEVER-drop) — but a HEALTHY sibling lens still commits, which
	// is the whole point of the per-lens watermark (issue #55).
	combined := append([]store.Observation{}, m.Active...)
	for li := range m.Lenses {
		if m.Lenses[li].MineFailed {
			continue
		}
		for i := range m.Lenses[li].Mined {
			mo := m.Lenses[li].Mined[i]
			if vector.NearestScore(m.Active, mo.Embedding, mo.Lens) >= dedupThreshold {
				continue
			}
			if vector.NearestScore(*existing, mo.Embedding, mo.Lens) >= dedupThreshold {
				continue
			}
			combined = append(combined, mo)
		}
	}

	// Drop anything whose exact obsID is already in L1. This makes the pass
	// idempotent on re-run: re-drained active obs and identical re-mines (after a
	// crash) are skipped rather than duplicated. `seen` also dedups within the batch.
	seen := make(map[string]bool, len(*existing))
	for _, o := range *existing {
		seen[o.ID] = true
	}
	var toWrite []store.Observation
	for _, o := range combined {
		if seen[o.ID] {
			continue
		}
		seen[o.ID] = true
		if o.TS == "" {
			o.TS = m.SessionTS
		}
		toWrite = append(toWrite, o)
	}

	// Write L1 first (so a crash mid-write re-runs the delta; obsID dedup keeps that
	// re-run from duplicating). Active obs are written regardless of any lens's mining
	// outcome — they're authoritative and independent of `claude -p`, so a mining
	// outage must not delay them.
	if err := w.Store.AppendObservations(toWrite); err != nil {
		return fmt.Errorf("append L1: %w", err)
	}

	// Advance each lens's watermark INDEPENDENTLY. A failed lens backs off (its delta
	// stays pending); a successful lens advances — but ONLY if the raw generation it
	// mined is still current (#49 C2 CAS). The CAS guard depends only on (session,
	// rawHighID), which every lens of this session shares, so all successful lenses
	// return the same currency verdict; we capture it to gate staged-clearing below.
	generationCurrent := false
	driftCount := 0
	lastDriftLens := ""
	for _, lm := range m.Lenses {
		if lm.MineFailed {
			n := w.Store.IncRetry(m.Session, lm.Lens)
			_ = w.Store.SetNextAttempt(m.Session, lm.Lens, time.Now().Add(backoffDelay(n)))
			slog.Warn("distill: lens mining failed; backing off (delta stays pending, never dropped)",
				"session", m.Session, "lens", lm.Lens, "attempt", n, "backoff", backoffDelay(n).String())
			continue
		}
		// A drifted lens advances just like a successful one (its data outcome — zero obs
		// for this delta — is identical to the pre-#57 silent behavior). We only tally it
		// so the drift can be surfaced (doctor + backfill summary): drift is NOT a
		// transport failure, so it must NOT back off (that would re-hammer a
		// deterministically-below-floor model forever and wedge the backfill queue).
		if lm.Drifted {
			driftCount++
			lastDriftLens = lm.Lens
		}
		w.Store.ResetRetry(m.Session, lm.Lens)
		advanced, err := w.Store.MarkDistilledIfCurrent(m.Session, lm.Lens, m.Total, m.RawHighID)
		if err != nil {
			return err
		}
		if advanced {
			generationCurrent = true
		} else {
			slog.Warn("distill: raw changed under mine; lens watermark held, will re-mine",
				"session", m.Session, "lens", lm.Lens, "mined_to", m.Total)
		}
	}
	// Record drift AFTER the advance loop, and only when the generation was still current
	// (a drift over a since-replaced generation didn't really "happen" for the archive —
	// the session will re-mine the new generation, so counting it would be misleading).
	if driftCount > 0 && generationCurrent {
		if err := w.Store.RecordDrift(driftCount, m.Session, lastDriftLens); err != nil {
			// Surfacing is best-effort bookkeeping; a meta-write hiccup must never fail a
			// commit whose L1/watermark writes already succeeded.
			slog.Warn("distill: could not record drift counter", "session", m.Session, "err", err)
		}
	}

	// Feed the just-written observations into the running snapshot so a later session
	// in this drain dedups against them (the cross-session dedup guarantee).
	*existing = append(*existing, toWrite...)

	// Clear the staged rows we drained — LAST, so a crash before it just re-drains
	// harmlessly (obsID dedup absorbs the re-write). Gate on generationCurrent: if a
	// replace-import/cleanup deleted raw mid-mine, hold staged so the re-mine of the
	// new generation re-drains them. If EVERY lens failed (generationCurrent stays
	// false with no CAS), staged is likewise held and re-drains next attempt.
	if generationCurrent {
		w.Store.ClearStagedThrough(m.Session, m.StagedThroughID)
	}
	return nil
}

// minedObs is the shape we ask the extract prompt to return as a JSON array.
type minedObs struct {
	Dimension   string `json:"dimension"`
	Observation string `json:"observation"`
	Evidence    string `json:"evidence"`
	Poignancy   int    `json:"poignancy"`
}

func (w *Worker) mine(ctx context.Context, ln *lens.Lens, session, transcript string) ([]store.Observation, error) {
	reply, err := w.Run(ctx, w.Config.TriageModel, ln.Extract, transcript)
	if err != nil {
		return nil, err
	}
	raw, perr := ParseJSONArray[minedObs](reply)
	if perr != nil {
		// The model replied (no transport error) but its reply contained NO parseable
		// JSON array at all — prose drift (it conversed/refused instead of extracting).
		// This is NOT the same as an explicit empty `[]` (legit quiet): ParseJSONArray
		// returns that as ([]T{}, nil) and we fall through below with zero obs. Signal
		// drift with the errNoArray sentinel so MineSession can count + surface it (the
		// #57 model-floor signal) instead of silently treating a below-floor model as an
		// uneventful session. Watermark handling is decided by the caller (advance-on-
		// drift, same data outcome as before — just loud now).
		return nil, fmt.Errorf("%w (lens=%s reply_len=%d)", errNoArray, ln.Name, len(strings.TrimSpace(reply)))
	}
	var obs []store.Observation
	for _, m := range raw {
		if strings.TrimSpace(m.Observation) == "" {
			continue
		}
		if m.Poignancy < 1 {
			m.Poignancy = 1
		}
		obs = append(obs, store.Observation{
			ID:          obsID(session, ln.Name, m.Observation),
			Session:     session,
			Lens:        ln.Name,
			Dimension:   m.Dimension,
			Observation: m.Observation,
			Evidence:    m.Evidence,
			Poignancy:   m.Poignancy,
			Source:      "mined",
		})
	}
	return obs, nil
}

// PreviewMine mines ONE session through ONE lens WITHOUT touching the archive — the
// read-only engine behind `witness lens try`. It is a deliberately stripped-down twin
// of MineSession's inner loop, and its differences are the whole point:
//
//   - Whole session, not the delta. It renders the ENTIRE raw history (done=0), never
//     the per-lens watermark's un-mined tail. Reusing MineSession would preview only
//     raw[done:], so an already-enabled lens would show nothing — silently gutting the
//     feature. It also ignores LensBackedOff, so a lens sleeping off a failure still
//     previews.
//   - No embedder. It calls mine() directly (which never embeds), so the 470MB model
//     is never loaded and Worker.Embedder may be nil.
//   - No writes. It calls NEITHER CommitMining, AppendObservations, the watermark
//     (MarkDistilled*), staged (DrainStaged/ClearStagedThrough), backoff, NOR
//     RecordDrift. The archive is untouched; a preview is safe to run repeatedly.
//
// It returns the observations mine() would have produced, the chunk count (how many
// inputs the session rendered to — >1 flags an arc-fragile split for the caller), and
// whether the lens DRIFTED (prose_drift: at least one input returned no JSON array AND
// the lens produced zero observations across ALL inputs — the same all-inputs rule as
// LensMining.Drifted, so a preview's drift reading matches the engine's). A transport
// error (rate limit, timeout) is returned as-is — that is a real failure, not drift.
//
// run is the MineFunc the caller wired from an open Runner (RunnerMine); PreviewMine
// owns no runner lifecycle. cfg supplies the triage model. st is used only to read raw.
func PreviewMine(ctx context.Context, run MineFunc, cfg store.Config, st *store.Store, session string, ln *lens.Lens) (obs []store.Observation, chunkCount int, drifted bool, err error) {
	raw, err := st.ReadRaw(session)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read L0: %w", err)
	}
	// A bare Worker is enough for mine(): it reads only Run, Config, and the lens arg —
	// never Store, Embedder, or Lenses. Embedder stays nil (mine never embeds), Store is
	// nil to make it structurally impossible for this preview to reach a write path.
	w := &Worker{Config: cfg, Run: run}
	inputs := distillInputs(st, session, raw) // whole session — no watermark slice
	chunkCount = len(inputs)
	producedObs, sawDrift := false, false
	for _, transcript := range inputs {
		mined, mErr := w.mine(ctx, ln, session, transcript)
		if mErr != nil {
			if errors.Is(mErr, errNoArray) {
				sawDrift = true // prose drift on this input; keep going — another chunk may extract
				continue
			}
			return nil, chunkCount, false, mErr // real transport error — surface it
		}
		if len(mined) > 0 {
			producedObs = true
		}
		obs = append(obs, mined...)
	}
	// Same rule as LensMining.Drifted: flag drift only when the lens produced NOTHING
	// anywhere AND at least one input drifted, so a session where one chunk extracts
	// fine is never miscounted.
	return obs, chunkCount, sawDrift && !producedObs, nil
}

func obsID(session, lens, text string) string {
	h := sha1.Sum([]byte(session + "|" + lens + "|" + text))
	return "obs_" + fmt.Sprintf("%x", h[:6])
}

// mustJSON is a small helper for embedding structured input into prompts.
func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
