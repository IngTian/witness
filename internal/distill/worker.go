package distill

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/IngTian/claude-witness/internal/lens"
	"github.com/IngTian/claude-witness/internal/store"
	"github.com/IngTian/claude-witness/internal/vector"
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

// MineFunc runs one extraction pass (the `claude -p` call). Injectable so tests
// can drive the worker without spawning a real model. Nil => the package Run.
type MineFunc func(ctx context.Context, model, prompt, input string) (string, error)

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
	Run      MineFunc // nil => the package Run (real `claude -p`)
}

// Process runs the fast-path pass for a session, distilling only the records that
// arrived since the last run (the watermark). This makes it safe on RESUMED
// sessions: Claude Code reuses the session id and appends new turns to the same
// log, so a binary "done" flag would skip them — the delta watermark mines them.
func (w *Worker) Process(ctx context.Context, session string) error {
	raw, err := w.Store.ReadRaw(session)
	if err != nil {
		return fmt.Errorf("read L0: %w", err)
	}
	total := len(raw)
	done := w.Store.DistilledCount(session) // records already distilled

	// Only the turns past the watermark are new.
	var newRecs []store.RawRecord
	if total > done {
		newRecs = raw[done:]
	}

	// 1. Active observations (staged in-session via MCP) — authoritative. We drain
	// them and remember how far (stagedThroughID) so we can delete exactly those
	// rows after a successful commit (step 7); on failure they stay for retry.
	active, stagedThroughID, _ := w.Store.DrainStaged(session)
	for i := range active {
		active[i].Source = "active"
	}

	// Nothing new to mine and nothing staged → just advance the watermark.
	if len(newRecs) == 0 && len(active) == 0 {
		return w.Store.MarkDistilled(session, total)
	}

	// 2. Mine ONLY the delta. Each lens runs its extract prompt over the new turns.
	//
	// ALL-OR-NOTHING per delta (deliberate tradeoff): if ANY lens hits a transport
	// error, we abandon the whole pass below (mineFailed) and re-mine every lens on
	// the next attempt. A healthy sibling lens's observations from THIS round are
	// discarded — but never lost, since the delta stays pending and is re-mined in
	// full once the failure clears. We accept the wasted work because transport
	// errors almost always hit all lenses at once (same `claude -p` / auth), so a
	// single lens failing in isolation is rare; the alternative (per-lens
	// watermarking) is more state for a case that seldom happens. The cost of a
	// genuinely poison session is bounded: it retries on backoff (capped 6h, never
	// dropped) and shows up in `witness doctor` as BackedOff. If single-lens
	// failures ever become common, move to a per-(session,lens) watermark here.
	var mined []store.Observation
	mineFailed := false
	if len(newRecs) > 0 {
		for _, transcript := range distillInputs(session, newRecs) {
			for _, ln := range w.Lenses {
				obs, err := w.mine(ctx, ln, session, transcript)
				if err != nil {
					// A claude -p transport failure (rate limit, network). Treat the whole
					// delta as not-yet-distilled so it's retried, rather than silently
					// advancing past it (which would lose those turns). (A mere parse-miss
					// is NOT an error — mine() returns it as a quiet session.)
					slog.Error("mine failed", "session", session, "lens", ln.Name, "err", err)
					mineFailed = true
					continue
				}
				mined = append(mined, obs...)
			}
		}
	}

	// 3. Embed active so dedup + later recall work.
	for i := range active {
		if len(active[i].Embedding) == 0 {
			if v, err := w.Embedder.Embed(active[i].Observation); err == nil {
				active[i].Embedding = v
			}
		}
	}
	existing, _ := w.Store.ReadObservations("")

	// 4. Combine: active verbatim; mined minus near-duplicates (of active or stored).
	combined := append([]store.Observation{}, active...)
	for i := range mined {
		v, err := w.Embedder.Embed(mined[i].Observation)
		if err != nil {
			continue
		}
		mined[i].Embedding = v
		if vector.NearestScore(active, v, mined[i].Lens) >= dedupThreshold {
			continue
		}
		if vector.NearestScore(existing, v, mined[i].Lens) >= dedupThreshold {
			continue
		}
		combined = append(combined, mined[i])
	}

	// 5. Drop anything whose exact obsID is already in L1. This makes the whole pass
	// idempotent on re-run: re-drained active obs and identical re-mines (after a
	// crash) are skipped rather than duplicated. `seen` also dedups within the batch.
	seen := make(map[string]bool, len(existing))
	for _, o := range existing {
		seen[o.ID] = true
	}
	sessionTS := ""
	if total > 0 {
		sessionTS = raw[total-1].TS // recency anchor for obs lacking their own ts
	}
	var toWrite []store.Observation
	for _, o := range combined {
		if seen[o.ID] {
			continue
		}
		seen[o.ID] = true
		if o.TS == "" {
			o.TS = sessionTS
		}
		toWrite = append(toWrite, o)
	}

	// 6. Failure handling (S1: at-least-once, NEVER-drop). A transport failure
	// (claude -p hiccup / rate limit) leaves the delta unwritten and the watermark
	// unadvanced, so the raw turns stay pending. We back the session off so the
	// consumer doesn't hammer it; when the failure clears, the delta self-heals.
	// (A mere parse-miss is handled in mine() as a quiet session, not a failure —
	// so a genuinely uneventful session still advances instead of looping forever.)
	if mineFailed {
		n := w.Store.IncRetry(session)
		_ = w.Store.SetNextAttempt(session, time.Now().Add(backoffDelay(n)))
		slog.Warn("distill: mining failed; backing off (delta stays pending, never dropped)",
			"session", session, "attempt", n, "backoff", backoffDelay(n).String())
		return nil // discard toWrite; active obs persist in staging, delta re-mines later
	}
	w.Store.ResetRetry(session)

	// 7. Write L1, then advance the watermark (so a crash mid-write re-runs the
	// delta; obsID dedup keeps that re-run from duplicating), then clear the staged
	// rows we drained — LAST, so a crash before it just re-drains harmlessly.
	if err := w.Store.AppendObservations(toWrite); err != nil {
		return fmt.Errorf("append L1: %w", err)
	}
	if err := w.Store.MarkDistilled(session, total); err != nil {
		return err
	}
	w.Store.ClearStagedThrough(session, stagedThroughID)
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
	runFn := w.Run
	if runFn == nil {
		runFn = func(ctx context.Context, model, prompt, input string) (string, error) {
			return RunWith(ctx, w.Config.Runner, model, prompt, input)
		}
	}
	reply, err := runFn(ctx, w.Config.TriageModel, ln.Extract, transcript)
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

func renderTranscript(raw []store.RawRecord) string {
	var b strings.Builder
	for _, r := range raw {
		b.WriteString(strings.ToUpper(r.Role))
		b.WriteString(": ")
		b.WriteString(r.Text)
		b.WriteString("\n\n")
	}
	return b.String()
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
