package distill

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
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

// PreviewStore is the read-only store surface PreviewMine needs (issue #73-C1): the
// L0 transcript (ReadRaw) plus the owning-platform lookup distillInputs → ForSession
// performs. It composes two narrow store interfaces so a preview holds no write path
// and can be driven by a fake. *store.Store satisfies it by promotion.
type PreviewStore interface {
	store.RawReader
	store.SessionPlatformReader
}

// Worker processes ONE session's L0 into L1. It is the sole writer of L1 and the
// sole place active + mined observations are combined.
//
// Combine rule (preserves hand-authored quality):
//   - active observations: passed through VERBATIM (authoritative; never re-distilled)
//   - mined observations: kept only if not a near-duplicate of an active one or an
//     already-stored same-lens observation
type Worker struct {
	// Store is the narrow distillation-queue surface (issue #73-C1): the L0/L1 reads,
	// the L1 commit, and the per-(session,lens) watermark/backoff/drift writes the
	// drain drives — NOT the whole *store.Store. This is what lets the worker run
	// against a fake with no real *sql.DB (see worker_fakestore_test.go). store.Queue
	// embeds SessionPlatformReader, so distillInputs → platform.ForSession still
	// resolves each session's owning platform through it.
	Store    store.Queue
	Embedder Embedder
	Lenses   []*lens.Lens // default (always) + any config-enabled lenses; all global, applied to every session regardless of source (CC or OpenCode)
	Config   store.Config
	Run      MineFunc // required; production wires RunnerMine(NewRunner(cfg)), tests inject a fake
	// RunFor, when set, picks the MineFunc for a specific lens — the per-lens RUNNER seam
	// (issue #75 slice 2): a lens declaring its own runtime mines against that runner
	// instead of the default one. nil → every lens uses Run (the single-runner path, and
	// what every test that injects only Run relies on). See runFor.
	RunFor func(ln *lens.Lens) MineFunc
	// NativeFor reports whether this lens's selected runner supports the optional
	// retained OpenCode-fork protocol. Nil keeps injected/test MineFuncs ordinary.
	NativeFor func(ln *lens.Lens) bool
}

// runFor returns the MineFunc for a lens: the per-lens runner via RunFor when wired,
// else the single default Run. Centralizes the fallback so mine() (and any future call
// site) never has to special-case a nil RunFor.
func (w *Worker) runFor(ln *lens.Lens) MineFunc {
	if w.RunFor != nil {
		if fn := w.RunFor(ln); fn != nil {
			return fn
		}
	}
	return w.Run
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
	// MineTimedOut narrows MineFailed to "the request hit a deadline" (errors.Is of
	// context.DeadlineExceeded). It is the backpressure signal: unlike a 4xx or a network
	// error, a deadline usually means WE asked for more concurrency than the provider
	// grants, so the drain reduces its window rather than retrying the same burst.
	//
	// It deliberately CONFLATES two causes, which the runner can distinguish and does not:
	// a request that never began generating (queue starvation — narrowing is the correct
	// response) and one that began and generated too slowly (a slow model — narrowing does
	// nothing for it). Both set this flag.
	//
	// Conflating them is the right trade here, for two reasons. Narrowing is per-batch and
	// re-derived from Conc on the next batch (see drainWindow), so a spurious narrow costs
	// some parallelism for one batch, not a persistent throttle. And the asymmetry of being
	// wrong is lopsided: failing to narrow under real starvation is the measured Windows
	// failure — four requests dead at exactly 600s having never been served, repeating
	// every drain — while narrowing on a genuinely slow model just serializes work that
	// was going to be slow anyway.
	//
	// If it ever needs splitting, the runner already has the fact: OpenCode tracks
	// generationStarted and reports the two phases in distinct messages ("never began
	// generating after ..." vs "generation exceeded ..."). The clean shape is a typed
	// sentinel from the platform port, not string matching here. Not done because there is
	// no evidence yet that the slow-model case actually fires — the flag would need to be
	// observed narrowing a healthy drain first.
	MineTimedOut bool
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
	// Native holds retained OpenCode fork finalizers for this lens's rendered
	// inputs. They run only after the generation-gated L1 commit succeeds.
	Native []*platform.NativeSession
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
	// Read the delta AND its raw "generation" (highest raw.id) as ONE atomic snapshot.
	// CommitMining advances each lens's watermark only if this id still exists, so a
	// replace-import/cleanup that deletes raw mid-mine can't have a stale count blind-
	// written over it (#49 C2). The generation is session-level (a property of raw),
	// shared by all lenses.
	//
	// This MUST be a single read, not ReadRaw + a separate MaxRawID: MaxOpenConns(1)
	// serializes each statement but releases the connection between them, so a replace-
	// import committing in that gap would pair an OLD-gen count/content with the NEW
	// gen's high id — the CAS then sees the new id live and blind-advances over never-
	// mined turns (issue #67-1). One query makes rawHighID provably the max id of
	// exactly `raw`, so the pair can never straddle a generation boundary.
	raw, rawHighID, err := w.Store.ReadRawSnapshot(session)
	if err != nil {
		return nil, fmt.Errorf("read L0: %w", err)
	}
	total := len(raw)

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
			// Ask the platform whether its sessions can host a retained scratch context, rather
			// than comparing its NAME. Both halves of this decision are now capabilities: the
			// session owner via SupportsNativeSessionSource, and the runner via w.NativeFor
			// (which asserts platform.NativeSessionSupport). The name compare that used to be
			// here was the engine's only piece of hardcoded platform knowledge, and the
			// acceptance guard that forbids exactly that could not see it.
			isNative := platform.SupportsNativeSessionSource(platform.ForSession(w.Store, session)) &&
				w.NativeFor != nil && w.NativeFor(ln)
			var nativeDigest string
			if isNative {
				entries := make([]platform.TranscriptEntry, len(raw))
				for i, record := range raw {
					entries[i] = platform.TranscriptEntry{Role: record.Role, Text: record.Text}
				}
				nativeDigest = platform.TranscriptDigest(entries)
			}
			for chunkIndex, transcript := range distillInputs(w.Store, w.Config, session, raw[done:]) {
				callCtx := ctx
				var native *platform.NativeSession
				if isNative {
					h := sha256.Sum256([]byte(transcript))
					native = &platform.NativeSession{Session: session, RawHigh: rawHighID, Total: total, Lens: ln.Name, Input: fmt.Sprintf("%d:%x", chunkIndex, h[:]), Digest: nativeDigest}
					callCtx = platform.WithNativeSession(callCtx, native)
				}
				// Log the SIZE of what we are about to send, and how long it took.
				//
				// This was a blind spot with real cost: a mine that times out reported only
				// "context deadline exceeded", which names the deadline and says nothing about the
				// request. Three separate wrong diagnoses on a real Windows failure (rate limits,
				// then the reply window, then the session fork) all survived longer than they
				// should have because nobody could see that the input was, or was not, enormous.
				// Chunking defaults to OFF (ChunkMaxChars=0 => the whole session as ONE
				// transcript), so a session with only a handful of MESSAGES can still carry a
				// gigantic prompt — a few tool results that each dumped a file, for instance. Turns
				// and characters are different units, and only one of them predicts a model stall.
				started := time.Now()
				slog.Info("mine: sending", "session", session, "lens", ln.Name,
					"chunk", chunkIndex, "input_chars", utf8.RuneCountInString(transcript),
					"input_bytes", len(transcript), "records", len(raw[done:]))
				obs, err := w.mine(callCtx, ln, session, transcript)
				slog.Info("mine: returned", "session", session, "lens", ln.Name,
					"chunk", chunkIndex, "input_chars", utf8.RuneCountInString(transcript),
					"took", time.Since(started).String(), "observations", len(obs),
					"err", errText(err))
				if native != nil {
					lm.Native = append(lm.Native, native)
				}
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
					// A DEADLINE is a distinct signal from any other transport failure: it means
					// the request was accepted and never served. When several fire at once against
					// a rate-limiting provider, the excess sit in a provider queue and every one of
					// them burns its full budget having never been generated — measured on a real
					// Windows run: four simultaneous prompts at 21:11:01, all four dead at exactly
					// 21:21:01 (600s), then ONE solo retry three seconds later completing in 41s.
					// The drain uses this to narrow its own concurrency (see DrainOpts.Conc), so
					// witness stops asking for more parallelism than the provider will serve.
					if errors.Is(err, context.DeadlineExceeded) {
						lm.MineTimedOut = true
					}
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

	// 3. Embed active + each lens's mined for later recall (MCP/CLI vector search).
	// Done in the map phase so the serial commit stays cheap; the embedder's own mutex
	// makes this concurrency-safe. A mined obs whose embedding FAILS is KEPT with an
	// empty embedding — the embedding is now consulted only at read-time recall (the
	// write-path dedup that used to require it is gone), so an un-embeddable obs is
	// still a fully valid L1 event (recall-only degradation until it is re-embedded).
	// This matches the active-obs loop just above (keep-on-failure) and honors "keep
	// full L1 to preserve expensive mining work."
	for i := range m.Active {
		if len(m.Active[i].Embedding) == 0 {
			if v, err := w.Embedder.Embed(m.Active[i].Observation); err == nil {
				m.Active[i].Embedding = v
			}
		}
	}
	for li := range m.Lenses {
		for i := range m.Lenses[li].Mined {
			if v, err := w.Embedder.Embed(m.Lenses[li].Mined[i].Observation); err == nil {
				m.Lenses[li].Mined[i].Embedding = v
			}
		}
	}
	return m, nil
}

// CommitMining is the REDUCE half: given one session's mining result and a pointer
// to the RUNNING corpus snapshot, write L1 (append-only), advance the watermark, and
// clear staged rows. It is the SOLE L1 writer and MUST run serially. `existing` is
// threaded by pointer and APPENDED with each session's newly-written observations so
// the exact-obsID idempotency check below sees an earlier session's writes within the
// same drain — the ONLY remaining use of `existing` now that the embedding dedup gate
// is gone (L1 is an append-only event log). It cannot suppress a genuine recurrence:
// obsID hashes the session, so two different sessions with identical text get distinct
// obsIDs and both survive.
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

	// Combine what to write: active verbatim + each SUCCESSFUL lens's mined verbatim.
	// L1 is an append-only EVENT LOG — we keep every occurrence, including a pattern
	// that recurs across sessions. The old code dropped a mined obs whose embedding was
	// a near-duplicate (cosine >= 0.93) of any resident same-lens obs; that was
	// confirmation bias — it silently destroyed re-emergence (a trait the person moved
	// away from and returned to could never be recorded, since the original was still
	// resident) AND reinforcement-frequency (the strongest evidence a trait is real).
	// That multiplicity IS the signal; the reviewer, which holds the current stance,
	// is the right place to fold repeats into confidence/change — not a stance-blind
	// content filter here. The only write-time filter that remains is exact-obsID
	// idempotency below (crash/re-mine + within-session repeats). A lens that hit a
	// transport failure contributes nothing this round; its delta stays pending and
	// re-mines when the failure clears (#55 per-lens watermark, never-drop).
	combined := append([]store.Observation{}, m.Active...)
	for li := range m.Lenses {
		if m.Lenses[li].MineFailed {
			continue
		}
		combined = append(combined, m.Lenses[li].Mined...)
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

	// Split what we write by provenance (issue #67-2). ACTIVE obs (MCP-staged) are
	// authoritative and generation-INDEPENDENT — write them unconditionally so a mining
	// outage or a raw replace never delays them. MINED obs are derived from the raw
	// generation the mine READ, so they must commit ATOMICALLY with the watermark
	// advance under the generation guard: on an edit-style replace landing mid-mine,
	// the mine's generation is gone and BOTH the mined obs and the advance are skipped
	// — no orphan obs referencing since-replaced text (the gap AppendObservations-then-
	// CAS used to leave). obsID dedup still keeps a crash-then-re-run idempotent.
	var activeToWrite, minedToWrite []store.Observation
	for _, o := range toWrite {
		if o.Source == "active" {
			activeToWrite = append(activeToWrite, o)
		} else {
			minedToWrite = append(minedToWrite, o)
		}
	}
	if err := w.Store.AppendObservations(activeToWrite); err != nil {
		return fmt.Errorf("append active L1: %w", err)
	}

	// Fail/succeed bookkeeping per lens: a failed lens backs off (its delta stays
	// pending, never dropped); a non-failed lens is a candidate to advance. ResetRetry
	// runs here for every non-failed lens (it also blanks any stale drift_at — a clean
	// re-mine forgets old drift; #69 Part 2), and we collect the lenses to advance +
	// tally drift for the stamp/counter below.
	driftCount := 0
	lastDriftLens := ""
	var okLenses, driftedLenses []string
	for _, lm := range m.Lenses {
		if lm.MineFailed {
			n := w.Store.IncRetry(m.Session, lm.Lens)
			_ = w.Store.SetNextAttempt(m.Session, lm.Lens, time.Now().Add(backoffDelay(n)))
			slog.Warn("distill: lens mining failed; backing off (delta stays pending, never dropped)",
				"session", m.Session, "lens", lm.Lens, "attempt", n, "backoff", backoffDelay(n).String())
			continue
		}
		// A drifted lens advances just like a successful one (its data outcome — zero obs
		// for this delta — is identical to the pre-#57 silent behavior). Drift is NOT a
		// transport failure, so it must NOT back off (that would re-hammer a deterministically-
		// below-floor model forever and wedge the backfill queue).
		w.Store.ResetRetry(m.Session, lm.Lens)
		okLenses = append(okLenses, lm.Lens)
		if lm.Drifted {
			driftCount++
			lastDriftLens = lm.Lens
			driftedLenses = append(driftedLenses, lm.Lens)
		}
	}

	// ATOMIC (issue #67-2): write the mined obs and advance the successful lenses'
	// watermarks in ONE generation-gated transaction. `advanced` is the shared currency
	// verdict — the guard is session-level (same rawHighID for every lens), so all
	// successful lenses advance together or not at all. It gates the drift stamp/counter,
	// the running-snapshot feed, and staged-clearing below (the old generationCurrent).
	generationCurrent, err := w.Store.CommitLensDistillation(minedToWrite, m.Session, m.Total, m.RawHighID, okLenses)
	if err != nil {
		return fmt.Errorf("commit lens distillation: %w", err)
	}
	if !generationCurrent && len(okLenses) > 0 {
		slog.Warn("distill: raw changed under mine; lens watermarks held, will re-mine",
			"session", m.Session, "mined_to", m.Total)
	}

	// Per-lens drift stamp — only when the generation advanced. ResetRetry (above) blanked
	// drift_at; re-stamping here makes the persisted drift_at reflect THIS pass's outcome,
	// so Stats.Drifted/doctor show a currently-drifting lens and drop one that recovered. A
	// drift over a since-replaced generation didn't really happen for the archive (the
	// session re-mines the new one). Best-effort — a stamp hiccup must never fail a commit
	// whose L1/watermark writes already landed.
	if generationCurrent {
		// L1 is durable before a native fork is finalized. A finalizer failure is
		// deliberately non-fatal: its manifest has already been marked committed and
		// OpenCode retries that cleanup on the next runner Open.
		for _, lm := range m.Lenses {
			if lm.MineFailed {
				continue
			}
			for _, native := range lm.Native {
				if err := native.Finalize(); err != nil {
					slog.Warn("distill: retain native cleanup for retry", "session", m.Session, "lens", lm.Lens, "err", err)
				}
			}
		}
		for _, ln := range driftedLenses {
			if err := w.Store.SetDrift(m.Session, ln); err != nil {
				slog.Warn("distill: could not stamp per-lens drift", "session", m.Session, "lens", ln, "err", err)
			}
		}
	}
	// Record drift AFTER the advance, and only when the generation was still current
	// (a drift over a since-replaced generation didn't really "happen" for the archive —
	// the session will re-mine the new generation, so counting it would be misleading).
	if driftCount > 0 && generationCurrent {
		if err := w.Store.RecordDrift(driftCount, m.Session, lastDriftLens); err != nil {
			// Surfacing is best-effort bookkeeping; a meta-write hiccup must never fail a
			// commit whose L1/watermark writes already succeeded.
			slog.Warn("distill: could not record drift counter", "session", m.Session, "err", err)
		}
	}

	// Feed the just-written observations into the running snapshot so a later session in
	// this drain sees them in the exact-obsID idempotency check (a re-mine of the SAME
	// (session,lens,text) within the drain won't double-write). Active obs always landed;
	// mined obs only landed if the generation was current, so feed them conditionally —
	// else the seen-map would reference phantom rows not actually in L1.
	*existing = append(*existing, activeToWrite...)
	if generationCurrent {
		*existing = append(*existing, minedToWrite...)
	}

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
	reply, err := w.runFor(ln)(ctx, ModelFor(w.Config, ln, PhaseExtract), ln.Extract, transcript)
	if err != nil {
		return nil, err
	}
	raw, perr := ParseJSONArray[minedObs](reply)
	if errors.Is(perr, ErrTruncatedJSONArray) {
		// The model DID extract; the reply was cut off mid-array (output-token cap, killed
		// child, dropped stream). Returning this as drift would advance the watermark over
		// turns whose observations were sitting in the truncated text — permanent loss,
		// since the watermark counts raw records and they are never offered again. Surface
		// it as an ordinary retryable failure instead: the lens backs off (5m/10m/20m…,
		// capped at 6h) and re-mines the SAME delta, and raw is never dropped.
		return nil, fmt.Errorf("%w (lens=%s reply_len=%d)", perr, ln.Name, len(strings.TrimSpace(reply)))
	}
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
// owns no runner lifecycle. cfg supplies the triage model. st is the narrow
// PreviewStore (issue #73-C1): read the raw transcript and resolve the session's
// owning platform — no write path, so a preview can never touch the archive.
func PreviewMine(ctx context.Context, run MineFunc, cfg store.Config, st PreviewStore, session string, ln *lens.Lens) (obs []store.Observation, chunkCount int, drifted bool, err error) {
	raw, err := st.ReadRaw(session)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read L0: %w", err)
	}
	// A bare Worker is enough for mine(): it reads only Run, Config, and the lens arg —
	// never Store, Embedder, or Lenses. Embedder stays nil (mine never embeds), Store is
	// nil to make it structurally impossible for this preview to reach a write path.
	w := &Worker{Config: cfg, Run: run}
	inputs := distillInputs(st, cfg, session, raw) // whole session — no watermark slice
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

// errText renders an error for a structured log field without the caller needing a branch: slog
// prints a nil error as "<nil>", which reads as a value rather than as "no failure".
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
