package distill

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
)

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
			for _, transcript := range distillInputs(w.Store, session, raw[done:]) {
				obs, err := w.mine(ctx, ln, session, transcript)
				if err != nil {
					slog.Error("mine failed", "session", session, "lens", ln.Name, "err", err)
					lm.MineFailed = true
					continue
				}
				lm.Mined = append(lm.Mined, obs...)
			}
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
	for _, lm := range m.Lenses {
		if lm.MineFailed {
			n := w.Store.IncRetry(m.Session, lm.Lens)
			_ = w.Store.SetNextAttempt(m.Session, lm.Lens, time.Now().Add(backoffDelay(n)))
			slog.Warn("distill: lens mining failed; backing off (delta stays pending, never dropped)",
				"session", m.Session, "lens", lm.Lens, "attempt", n, "backoff", backoffDelay(n).String())
			continue
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
		// The model replied (no transport error) but produced no parseable array —
		// a quiet/uneventful session. NOT a failure: yield zero observations so the
		// watermark advances rather than retrying a session that has nothing to mine.
		slog.Debug("mine: no observations parsed; treating as quiet session",
			"session", session, "lens", ln.Name)
		return nil, nil
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

func obsID(session, lens, text string) string {
	h := sha1.Sum([]byte(session + "|" + lens + "|" + text))
	return "obs_" + fmt.Sprintf("%x", h[:6])
}

// mustJSON is a small helper for embedding structured input into prompts.
func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
