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
	Mined           []store.Observation
	MineFailed      bool // a transport error hit at least one lens — commit must back off, not write
	NothingToDo     bool // no new turns and nothing staged — commit just advances the watermark
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
	done := w.Store.DistilledCount(session) // records already distilled

	// Capture the raw "generation" this mine reads — the highest raw.id. CommitMining
	// advances the watermark only if this id still exists, so a replace-import/cleanup
	// that deletes raw mid-mine can't have a stale count blind-written over it (#49 C2).
	rawHighID := w.Store.MaxRawID(session)

	// Only the turns past the watermark are new.
	var newRecs []store.RawRecord
	if total > done {
		newRecs = raw[done:]
	}

	m := &SessionMining{Session: session, Total: total, RawHighID: rawHighID, StagedThroughID: 0}
	if total > 0 {
		m.SessionTS = raw[total-1].TS
	}

	// 1. Active observations (staged in-session via MCP) — authoritative. We drain
	// them and remember how far (StagedThroughID) so CommitMining can delete exactly
	// those rows after a successful commit; on failure they stay for retry.
	active, stagedThroughID, _ := w.Store.DrainStaged(session)
	for i := range active {
		active[i].Source = "active"
	}
	m.Active = active
	m.StagedThroughID = stagedThroughID

	// Nothing new to mine and nothing staged → commit just advances the watermark.
	if len(newRecs) == 0 && len(active) == 0 {
		m.NothingToDo = true
		return m, nil
	}

	// 2. Mine ONLY the delta. Each lens runs its extract prompt over the new turns.
	//
	// ALL-OR-NOTHING per delta (deliberate tradeoff): if ANY lens hits a transport
	// error, we abandon the whole pass (MineFailed) and re-mine every lens on the
	// next attempt. A healthy sibling lens's observations from THIS round are
	// discarded — but never lost, since the delta stays pending and is re-mined in
	// full once the failure clears. We accept the wasted work because transport
	// errors almost always hit all lenses at once (same `claude -p` / auth), so a
	// single lens failing in isolation is rare; the alternative (per-lens
	// watermarking) is more state for a case that seldom happens. The cost of a
	// genuinely poison session is bounded: it retries on backoff (capped 6h, never
	// dropped) and shows up in `witness doctor` as BackedOff. If single-lens
	// failures ever become common, move to a per-(session,lens) watermark here.
	if len(newRecs) > 0 {
		for _, transcript := range distillInputs(w.Store, session, newRecs) {
			for _, ln := range w.Lenses {
				obs, err := w.mine(ctx, ln, session, transcript)
				if err != nil {
					// A claude -p transport failure (rate limit, network). Treat the whole
					// delta as not-yet-distilled so it's retried, rather than silently
					// advancing past it (which would lose those turns). (A mere parse-miss
					// is NOT an error — mine() returns it as a quiet session.)
					slog.Error("mine failed", "session", session, "lens", ln.Name, "err", err)
					m.MineFailed = true
					continue
				}
				m.Mined = append(m.Mined, obs...)
			}
		}
	}

	// 3. Embed active so dedup + later recall work. Done in the map phase so the
	// serial commit stays cheap; the embedder's own mutex makes this concurrency-safe.
	for i := range m.Active {
		if len(m.Active[i].Embedding) == 0 {
			if v, err := w.Embedder.Embed(m.Active[i].Observation); err == nil {
				m.Active[i].Embedding = v
			}
		}
	}
	// Embed mined too, here in the map phase (dedup in commit needs the vectors).
	// A mined obs whose embedding fails is dropped now — same as before, where the
	// combine loop skipped it on Embed error.
	kept := m.Mined[:0]
	for i := range m.Mined {
		v, err := w.Embedder.Embed(m.Mined[i].Observation)
		if err != nil {
			continue
		}
		m.Mined[i].Embedding = v
		kept = append(kept, m.Mined[i])
	}
	m.Mined = kept
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
		// Guarded like the main path: if raw was replaced under this mine (an OpenCode
		// history rewrite / cleanup deleting the rows), don't blind-advance the
		// watermark past a generation we didn't see (#49 C2). Not advancing leaves the
		// session pending so it re-checks the new generation.
		if _, err := w.Store.MarkDistilledIfCurrent(m.Session, m.Total, m.RawHighID); err != nil {
			return err
		}
		return nil
	}

	// Failure handling (S1: at-least-once, NEVER-drop). A transport failure leaves
	// the delta unwritten and the watermark unadvanced, so the raw turns stay
	// pending. We back the session off so the consumer doesn't hammer it; when the
	// failure clears, the delta self-heals. (A parse-miss is handled in mine() as a
	// quiet session, not a failure — so an uneventful session still advances.)
	if m.MineFailed {
		n := w.Store.IncRetry(m.Session)
		_ = w.Store.SetNextAttempt(m.Session, time.Now().Add(backoffDelay(n)))
		slog.Warn("distill: mining failed; backing off (delta stays pending, never dropped)",
			"session", m.Session, "attempt", n, "backoff", backoffDelay(n).String())
		return nil // active obs persist in staging, delta re-mines later
	}
	w.Store.ResetRetry(m.Session)

	// Combine: active verbatim; mined minus near-duplicates (of active or of the
	// running corpus, which includes earlier sessions' writes in THIS drain).
	combined := append([]store.Observation{}, m.Active...)
	for i := range m.Mined {
		if vector.NearestScore(m.Active, m.Mined[i].Embedding, m.Mined[i].Lens) >= dedupThreshold {
			continue
		}
		if vector.NearestScore(*existing, m.Mined[i].Embedding, m.Mined[i].Lens) >= dedupThreshold {
			continue
		}
		combined = append(combined, m.Mined[i])
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

	// Write L1, then advance the watermark (so a crash mid-write re-runs the delta;
	// obsID dedup keeps that re-run from duplicating), then clear the staged rows we
	// drained — LAST, so a crash before it just re-drains harmlessly.
	if err := w.Store.AppendObservations(toWrite); err != nil {
		return fmt.Errorf("append L1: %w", err)
	}
	// Advance the watermark ONLY if the raw generation we mined is still current.
	// If a replace-import / cleanup deleted raw mid-mine (#49 C2), the guard fails,
	// the watermark is NOT written (progress stays as the reset the import wrote, or
	// absent), and the pending query re-offers the session so it re-mines the new
	// generation from scratch. The L1 obs already appended are harmless: append-only,
	// obsID-idempotent, and the reviewer reconciles stale/superseded obs at L2 — the
	// only correctness-critical thing is that the watermark not skip un-mined turns.
	advanced, err := w.Store.MarkDistilledIfCurrent(m.Session, m.Total, m.RawHighID)
	if err != nil {
		return err
	}
	if !advanced {
		slog.Warn("distill: raw changed under mine; watermark held, session will re-mine",
			"session", m.Session, "mined_to", m.Total)
		return nil // leave staged rows for the re-mine; don't clear on a stale commit
	}
	w.Store.ClearStagedThrough(m.Session, m.StagedThroughID)
	// Feed the just-written observations into the running snapshot so the next
	// session's dedup sees them (the cross-session dedup guarantee).
	*existing = append(*existing, toWrite...)
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
