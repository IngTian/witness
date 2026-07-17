package commands

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

// `witness lens try` flags. Package-level so the cobra command's RunE closure can
// bind them; one process per invocation, so shared state is fine.
var (
	trySessions   int
	trySession    string
	tryModel      string
	tryReviewFlag bool
	tryReviewMdl  string
	tryRecent     bool
	tryJSON       bool
)

// newLensTryCmd builds the `try` subcommand. Unlike the other lens verbs (thin thunks
// into cmdLens), `try` carries its own flags, so it is a full command with a closure.
func newLensTryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "try <file|lens-name>",
		Short: "Preview a lens's EXTRACT prompt on real sessions (read-only, writes nothing).",
		Long: "Mine real sessions from your archive through a candidate lens and print the raw " +
			"observations it would produce — WITHOUT registering the lens or writing anything to the " +
			"archive. The argument is a lens FILE path, or the NAME of an already-registered lens (so " +
			"you can re-preview a registered lens's definition without its on-disk path). " +
			"This is the prompt-tuning loop: edit the EXTRACT prompt, run `try`, see what " +
			"changes. Sessions are sampled largest-first by default (deterministic, and the meatiest " +
			"sessions are the ones a prompt is most likely to mishandle); use --recent for the latest.\n\n" +
			"Works on both runners. On an OpenCode runner it briefly holds the worker lock while it runs " +
			"(its shutdown sweep would otherwise disrupt a running worker); on Claude it needs no lock. " +
			"Sessions are previewed in parallel (bounded by mine_concurrency).\n\n" +
			"With --review, it also runs the lens's REVIEW prompt over the observations just mined and " +
			"prints the profile facets it would synthesize — so you can tune both halves of a lens in one " +
			"pass. (The synthesis is over the sampled sessions only, so cross-session change-detection is " +
			"weak; register + backfill the lens for a full-history review.) Still writes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdLensTry(args[0], lensTryOpts{
				nSessions: trySessions, oneSession: trySession, model: tryModel,
				review: tryReviewFlag, reviewModel: tryReviewMdl, recent: tryRecent, asJSON: tryJSON,
			})
		},
	}
	c.Flags().IntVar(&trySessions, "sessions", 3, "number of sessions to sample")
	c.Flags().StringVar(&trySession, "session", "", "preview one specific session id (bypasses sampling)")
	c.Flags().StringVar(&tryModel, "model", "", "override the triage (extract) model for this run (e.g. test above a drift floor without editing config)")
	c.Flags().BoolVar(&tryReviewFlag, "review", false, "also preview the REVIEW prompt: synthesize profile facets from the mined observations")
	c.Flags().StringVar(&tryReviewMdl, "review-model", "", "override the distill (review) model for --review (defaults to distill_model)")
	c.Flags().BoolVar(&tryRecent, "recent", false, "sample the most recent sessions instead of the largest")
	c.Flags().BoolVarP(&tryJSON, "json", "j", false, "output as JSON")
	return c
}

// lensTryOpts groups the try flags — the signature had grown past readability, and a
// struct keeps call sites (and tests) honest about which knob is which.
type lensTryOpts struct {
	nSessions   int
	oneSession  string
	model       string // --model: overrides extract (triage) model
	review      bool   // --review: also run the REVIEW prompt over the mined observations
	reviewModel string // --review-model: overrides review (distill) model
	recent      bool
	asJSON      bool
}

// --- JSON output shape (stable, for diffing v1-vs-v2 prompt runs) -------------

type lensTryObsJSON struct {
	Dimension   string `json:"dimension"`
	Observation string `json:"observation"`
	Evidence    string `json:"evidence"`
	Poignancy   int    `json:"poignancy"`
}

type lensTrySessionJSON struct {
	Session      string           `json:"session"`
	RawTurns     int              `json:"raw_turns"`
	RawChars     int64            `json:"raw_chars"` // SQLite LENGTH() = characters, not bytes
	ChunkCount   int              `json:"chunk_count"`
	Drifted      bool             `json:"drifted"`
	Error        string           `json:"error,omitempty"` // set (and observations empty) if this session's mine failed
	ElapsedMS    int64            `json:"elapsed_ms"`
	Observations []lensTryObsJSON `json:"observations"`
}

// lensTryFacetJSON is one facet the REVIEW preview synthesized (only with --review).
type lensTryFacetJSON struct {
	Dimension   string   `json:"dimension"`
	Key         string   `json:"key"`
	Value       string   `json:"value"`
	Confidence  float64  `json:"confidence"`
	BecauseOf   []string `json:"because_of"`
	Contradicts bool     `json:"contradicts_prior"`
}

// lensTryReviewJSON is the REVIEW-preview block (present only with --review).
type lensTryReviewJSON struct {
	Model   string             `json:"model"`           // the review (distill) model used
	Drifted bool               `json:"drifted"`         // REVIEW returned no facet array (prose drift)
	Error   string             `json:"error,omitempty"` // set if the review call failed (non-drift)
	Facets  []lensTryFacetJSON `json:"facets"`
}

type lensTryJSON struct {
	Lens       string               `json:"lens"`
	Model      string               `json:"model"`
	Candidate  bool                 `json:"candidate"` // true = shown name is a fallback (file's # name: was reserved)
	Sessions   []lensTrySessionJSON `json:"sessions"`
	TotalObs   int                  `json:"total_observations"`
	DriftedAny bool                 `json:"drifted_any"`
	Review     *lensTryReviewJSON   `json:"review,omitempty"` // present only with --review
}

// cmdLensTry runs the read-only preview. It opens its OWN store (independent of cmdLens)
// and reads only — PreviewMine writes nothing (see distill.PreviewMine). On an OpenCode
// runner it holds the single-flight WorkerLock for the runner's whole lifetime because
// OpenCode's Close() runs a process-global sweep that would delete a concurrent worker's
// in-flight distill session; on Claude (no sweep) it stays lock-free.
func cmdLensTry(file string, opts lensTryOpts) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	// The arg is a lens DIRECTORY path (holding lens.json + extract.md + review.md), or
	// the NAME of an already-registered lens — the convenience so you can `lens try
	// codereview` (re-preview a registered lens's own definition) without knowing its
	// on-disk path. A registered name wins over a same-named dir in cwd: names have no
	// path separators/extension, so the ambiguity is negligible, and "I typed my lens's
	// name" is the overwhelmingly likely intent.
	path := file
	if resolved, ok := registeredLensPath(st, file); ok {
		path = resolved
	}

	// Load the candidate. Strict first; on a reserved-name collision fall back to the
	// lenient loader (preview never writes, so an impersonating name is harmless) and
	// mark it so the display makes the fallback obvious.
	candidate := false
	ln, err := lens.LoadFromDir(path)
	if err != nil {
		var uErr error
		if ln, uErr = lens.LoadFromDirUnchecked(path); uErr != nil {
			return uErr // a genuine load error (missing dir / no extract.md) — surface it
		}
		// LoadFromDirUnchecked succeeded where LoadFromDir failed => reserved name.
		ln.Name = "candidate"
		candidate = true
	}
	// --review needs a REVIEW section to run. Fail early (before any runner work) rather
	// than silently skipping the half the user explicitly asked for.
	if opts.review && strings.TrimSpace(ln.Review) == "" {
		return fmt.Errorf("--review needs a REVIEW section in %q, but the lens has none", path)
	}

	cfg := st.LoadConfig()
	cfg.Runner = st.ResolveRunner(cfg)
	// Model overrides must be applied BEFORE the runner is minted/opened: OpenCode's Open
	// starts `opencode serve` prewarming cfg.TriageModel + cfg.DistillModel, so a later
	// override would silently not reach the server. --model overrides EXTRACT (triage);
	// --review-model overrides REVIEW (distill) — the two engine stages use two models.
	//
	// Apply the override onto BOTH cfg (so OpenCode prewarms the right model) AND the
	// lens's own per-lens model fields (#75): distill.ModelFor prefers a lens's
	// ExtractModel/ReviewModel over the global, so overriding only cfg would let a
	// registered lens's own per-lens model shadow the explicit --model the user asked to
	// preview ON. Explicit --model is the user's stated preview intent and must win.
	if opts.model != "" {
		cfg.TriageModel = opts.model
		ln.ExtractModel = opts.model
	}
	// Only apply --review-model when review actually runs. Otherwise, on an OpenCode
	// runner, Open validates cfg.DistillModel up front and a bad --review-model would
	// abort an extract-only preview on a model that is never used (on Claude it'd be
	// silently ignored) — a confusing runner-dependent side effect for a review-only knob.
	if opts.review && opts.reviewModel != "" {
		cfg.DistillModel = opts.reviewModel
		ln.ReviewModel = opts.reviewModel
	}

	// Resolve the session set (validation happens BEFORE any runner work, so a bad file/
	// session never spawns a server or takes a lock).
	sessions, err := resolveTrySessions(st, opts.nSessions, opts.oneSession, opts.recent)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Mint the runner WITHOUT opening it yet. For a sweeping runner (OpenCode) the
	// WorkerLock MUST be taken between minting and Open: if we Opened first (starting the
	// server) and then bailed on a failed lock, the deferred Close()'s +1s sweep could
	// delete a live worker's in-flight distill session — the exact hazard the lock guards.
	// So the mint→lock→Open sequence is inlined here (a mint+Open helper would leave no
	// seam for the pre-Open lock decision on a sweeping runner).
	runner, err := platform.RunnerFor(st, cfg)
	if err != nil {
		return err
	}
	if platform.RunnerSweepsOnClose(runner) {
		unlock, ok := st.WorkerLock()
		if !ok {
			return fmt.Errorf("`lens try` needs exclusive access on the %q runner (its shutdown sweep "+
				"would disrupt a running worker); a witness worker is draining now — retry once it is idle", cfg.Runner)
		}
		// Registered BEFORE runner.Close below: LIFO runs Close (its sweep) while the lock
		// is still held, THEN unlock — so the sweep never fires outside the lock.
		defer unlock()
	}
	if err := runner.Open(ctx); err != nil {
		return err
	}
	defer runner.Close()
	runFn := distill.RunnerMine(runner)

	// Preview all sessions in parallel (bounded by the engine's concurrency policy), then
	// render in sample order. Concurrency is clamped to the sample size (and to 1 for a
	// single --session, or a hypothetical non-concurrent runner).
	conc := distill.EffectiveConcurrency(cfg.MineConcurrency, runner.ConcurrentRunSafe())
	if conc > len(sessions) {
		conc = len(sessions)
	}
	results := runPreviews(conc, sessions, func(sess string) tryResult {
		start := time.Now()
		obs, chunks, drifted, err := distill.PreviewMine(ctx, runFn, cfg, st, sess, ln)
		return tryResult{obs: obs, chunks: chunks, drifted: drifted, err: err, elapsed: time.Since(start)}
	})

	// --review (L1→L2): synthesize facets from the observations JUST mined above (never
	// registered, never written). This is the chain-from-the-sample design: the review
	// sees only the sampled sessions' observations, so cross-session change-detection is
	// inherently weak (no accumulated `prior` facets to contradict) — documented in help.
	var review *reviewPreview
	if opts.review {
		review = runReviewPreview(ctx, runFn, cfg, ln, results)
	}

	if opts.asJSON {
		return lensTryEmitJSON(st, cfg, ln, candidate, sessions, results, review)
	}
	return lensTryRenderHuman(st, cfg, ln, candidate, sessions, results, review)
}

// reviewPreview is the outcome of the optional --review pass.
type reviewPreview struct {
	model   string
	obsFed  int  // how many mined observations were synthesized (context for the reader)
	drifted bool // the REVIEW reply had no JSON array (prose drift) — likely a too-weak review model
	facets  []distill.PreviewFacet
	err     error
}

// runReviewPreview collects the observations mined across all sessions and runs the
// REVIEW prompt over them once (matching production, which reviews the whole L1 corpus
// per lens, not per session). prior is empty: a candidate lens has no accumulated
// facets, so change-detection has nothing to contradict — an inherent preview caveat.
//
// A panic in the review call is caught and turned into rp.err (mirroring the extract
// fan-out's recover barrier): a review failure must degrade to a reported error, never
// crash the command and discard the already-computed extract output.
func runReviewPreview(ctx context.Context, runFn distill.MineFunc, cfg store.Config, ln *lens.Lens, results []tryResult) (rp *reviewPreview) {
	var obs []store.Observation
	for _, r := range results {
		obs = append(obs, r.obs...)
	}
	rp = &reviewPreview{model: modelLabel(store.Config{TriageModel: cfg.DistillModel}), obsFed: len(obs)}
	if len(obs) == 0 {
		return rp // nothing mined → nothing to synthesize; not an error
	}
	defer func() {
		if r := recover(); r != nil {
			rp.err = fmt.Errorf("panic during review: %v", r)
		}
	}()
	facets, err := distill.PreviewReview(ctx, runFn, cfg, ln, obs, nil /* no prior facets for a candidate */)
	rp.facets = facets
	// Classify a no-array reply as DRIFT (a too-weak review model conversed instead of
	// emitting facets) vs a transport failure — so the render can give the same
	// actionable "raise the model" guidance the extract path does, rather than a generic
	// "review failed" that reads like an internal parse bug.
	if errors.Is(err, distill.ErrNoJSONArray) {
		rp.drifted = true
	} else {
		rp.err = err
	}
	return rp
}

// registeredLensPath maps a `lens try` arg to the on-disk DIRECTORY of a registered lens
// when the arg is one of that lens's registered NAMES — the convenience that lets a user
// `lens try codereview` instead of typing the archive path. It matches only a name with
// no path separator or extension (so an actual "./foo" or "dir/lens" always stays a
// directory path the user typed), and only when that name is in RegisteredLenses. Returns
// ok=false to fall through to path handling. `default` isn't in RegisteredLenses (it's the
// built-in), so `lens try default` stays a path arg — previewing the built-in isn't the
// point here.
func registeredLensPath(st *store.Store, arg string) (string, bool) {
	if strings.ContainsRune(arg, filepath.Separator) || strings.Contains(arg, "/") || filepath.Ext(arg) != "" {
		return "", false
	}
	if !slices.Contains(st.RegisteredLenses(), arg) {
		return "", false
	}
	return filepath.Join(st.LensesDir(), arg), true
}

// resolveTrySessions picks the sessions to preview: one specific --session (validated
// to exist), else the largest N (or the most recent N with --recent). Deterministic
// ordering in both sampling modes so v1-vs-v2 prompt runs compare the same sessions.
func resolveTrySessions(st *store.Store, nSessions int, oneSession string, recent bool) ([]string, error) {
	if oneSession != "" {
		if st.RawCount(oneSession) == 0 {
			return nil, fmt.Errorf("session %q has no raw turns (nothing to preview)", oneSession)
		}
		return []string{oneSession}, nil
	}
	if nSessions < 1 {
		nSessions = 1
	}
	var sessions []string
	var err error
	if recent {
		sessions, err = st.SampleRecentSessions(nSessions)
	} else {
		sessions, err = st.SampleSessions(nSessions)
	}
	if err != nil {
		return nil, fmt.Errorf("sample sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("archive has no sessions to preview")
	}
	return sessions, nil
}

// tryResult is one session's preview outcome (an error OR observations, plus metadata).
type tryResult struct {
	obs     []store.Observation
	chunks  int
	drifted bool
	err     error
	elapsed time.Duration
}

// runPreviews previews sessions with a bounded fan-out and returns the results in
// SAMPLE ORDER (results[i] is sessions[i]'s outcome, regardless of completion order) so
// output is deterministic and v1-vs-v2 diffable. Each goroutine writes only its own
// index (no shared-slice race) and is wrapped in a recover barrier: a panic in one
// preview becomes that session's error rather than crashing the process (mirroring the
// engine's "distillation never takes down the process" invariant). preview must be safe
// to call concurrently — distill.PreviewMine is (fresh bare Worker per call, no writes,
// runner.Run concurrency-safe).
func runPreviews(conc int, sessions []string, preview func(sess string) tryResult) []tryResult {
	if conc < 1 {
		conc = 1
	}
	results := make([]tryResult, len(sessions))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, s string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					results[i] = tryResult{err: fmt.Errorf("panic previewing session %s: %v", s, r)}
				}
			}()
			results[i] = preview(s)
		}(i, s)
	}
	wg.Wait()
	return results
}

// modelLabel renders the effective triage model for display ("(runner default)" when
// unset, so the reader knows which model produced the preview).
func modelLabel(cfg store.Config) string {
	if cfg.TriageModel == "" {
		return "runner default"
	}
	return cfg.TriageModel
}

// lensTryRenderHuman prints the pre-computed results in SAMPLE ORDER (results[i] pairs
// with sessions[i]). Rendering after the fan-out barrier — not during — is what keeps
// output deterministic regardless of which session's mine finished first.
func lensTryRenderHuman(st *store.Store, cfg store.Config, ln *lens.Lens, candidate bool, sessions []string, results []tryResult, review *reviewPreview) error {
	name := ln.Name
	if candidate {
		name += dim(" (candidate — file's name was reserved)")
	}
	fmt.Printf("%s %s   %s %s\n", label("lens"), bold(name), dim("extract model:"), modelLabel(cfg))
	fmt.Printf("%s previewing %d session(s), read-only — nothing is written\n\n", label("try"), len(sessions))

	total, driftedAny := 0, false
	for i, sess := range sessions {
		r := results[i]
		turns := st.RawCount(sess)
		chars := st.RawChars(sess)
		fmt.Printf("%s %s  %s\n", cyan("── session"), sess,
			dim(fmt.Sprintf("(%d turns, %d chars) ──", turns, chars)))

		if r.err != nil {
			// A transport error (or panic) on one session doesn't sink the others — report
			// it and move on, so a rate-limit on session 2 still lets 1 and 3 render.
			fmt.Printf("   %s %s\n\n", badGlyph(), red("mine failed: "+r.err.Error()))
			continue
		}
		if r.chunks > 1 {
			fmt.Printf("   %s %s\n", warnGlyph(), yellow(fmt.Sprintf(
				"session rendered to %d chunks — arc-spanning lenses may fragment across chunk boundaries", r.chunks)))
		}
		if r.drifted {
			driftedAny = true
			fmt.Printf("   %s %s\n", warnGlyph(), yellow(
				"model returned no observation array (prose drift) — the triage model may be too weak for this prompt; "+
					"raise it with `witness config set triage_model <stronger>` or pass --model"))
		}
		if len(r.obs) == 0 && !r.drifted {
			fmt.Printf("   %s\n", dim("(no observations — a quiet session for this lens, or the prompt found nothing)"))
		}
		for _, o := range r.obs {
			total++
			fmt.Printf("   %s %s\n", dim(fmt.Sprintf("[%s p%d]", o.Dimension, o.Poignancy)), o.Observation)
			if o.Evidence != "" {
				fmt.Printf("       %s\n", dim("↳ "+o.Evidence))
			}
		}
		fmt.Printf("   %s\n\n", dim(fmt.Sprintf("%d obs in %.1fs", len(r.obs), r.elapsed.Seconds())))
	}

	summary := fmt.Sprintf("%d observation(s) across %d session(s)", total, len(sessions))
	if driftedAny {
		summary += " — some sessions drifted (see above)"
	}
	fmt.Printf("%s %s\n", label("total"), summary)

	if review != nil {
		lensTryRenderReviewHuman(review)
	}
	return nil
}

// lensTryRenderReviewHuman prints the REVIEW-preview block: the facets the lens's
// REVIEW prompt would synthesize from the observations just mined. Kept visually
// separate from the per-session extract output so the two stages read distinctly.
func lensTryRenderReviewHuman(review *reviewPreview) {
	fmt.Printf("\n%s %s   %s %s\n", label("review"),
		dim(fmt.Sprintf("synthesizing %d mined observation(s)", review.obsFed)), dim("review model:"), review.model)
	fmt.Printf("   %s\n", dim("(over the sampled sessions only — register + backfill for full-history change detection)"))
	if review.drifted {
		// Mirror the extract-drift message: distinct from a transport failure, with the
		// actionable fix (a stronger review model), not a generic "review failed".
		fmt.Printf("   %s %s\n", warnGlyph(), yellow(
			"REVIEW returned no facet array (prose drift) — the review model may be too weak for this prompt; "+
				"raise it with `witness config set distill_model <stronger>` or pass --review-model"))
		return
	}
	if review.err != nil {
		fmt.Printf("   %s %s\n", badGlyph(), red("review failed: "+review.err.Error()))
		return
	}
	if review.obsFed == 0 {
		fmt.Printf("   %s\n", dim("(no observations were mined, so there is nothing to synthesize)"))
		return
	}
	if len(review.facets) == 0 {
		fmt.Printf("   %s\n", dim("(the REVIEW prompt asserted no facets from these observations)"))
		return
	}
	malformed := 0
	for _, f := range review.facets {
		// A facet missing its key or value parsed as JSON but doesn't carry the substance a
		// facet needs (key = the named attribute, value = its assertion) — usually the
		// REVIEW prompt didn't specify the output schema, so the model emitted objects with
		// the wrong field names. Flag it as a prompt problem rather than rendering a blank
		// "[dim/ c0.00]  = " line that looks like a tool bug (the exact signal a lens author
		// needs while tuning a REVIEW prompt).
		if strings.TrimSpace(f.Key) == "" || strings.TrimSpace(f.Value) == "" {
			malformed++
			continue
		}
		change := ""
		if f.Contradicts {
			change = yellow(" [contradicts prior]")
		}
		fmt.Printf("   %s %s = %s%s\n",
			dim(fmt.Sprintf("[%s/%s c%.2f]", f.Dimension, f.Key, f.Confidence)), bold(f.Key), f.Value, change)
		if len(f.BecauseOf) > 0 {
			fmt.Printf("       %s\n", dim(fmt.Sprintf("↳ because_of %d obs", len(f.BecauseOf))))
		}
	}
	if malformed > 0 {
		fmt.Printf("   %s %s\n", warnGlyph(), yellow(fmt.Sprintf(
			"%d facet(s) had no dimension/key/value — the REVIEW prompt likely doesn't specify the facet "+
				"output schema (each element needs dimension, key, value, confidence, because_of)", malformed)))
	}
}

func lensTryEmitJSON(st *store.Store, cfg store.Config, ln *lens.Lens, candidate bool, sessions []string, results []tryResult, review *reviewPreview) error {
	out := lensTryJSON{Lens: ln.Name, Model: modelLabel(cfg), Candidate: candidate}
	for i, sess := range sessions {
		r := results[i]
		sj := lensTrySessionJSON{
			Session:      sess,
			RawTurns:     st.RawCount(sess),
			RawChars:     st.RawChars(sess),
			ChunkCount:   r.chunks,
			Drifted:      r.drifted,
			ElapsedMS:    r.elapsed.Milliseconds(),
			Observations: []lensTryObsJSON{},
		}
		if r.err != nil {
			// Report-and-continue, matching human mode: record this session's error and
			// still emit a well-formed array so the other previews aren't discarded (a
			// --json run stays diffable even when one session errored).
			sj.Error = r.err.Error()
			out.Sessions = append(out.Sessions, sj)
			continue
		}
		for _, o := range r.obs {
			sj.Observations = append(sj.Observations, lensTryObsJSON{
				Dimension:   o.Dimension,
				Observation: o.Observation,
				Evidence:    o.Evidence,
				Poignancy:   o.Poignancy,
			})
			out.TotalObs++
		}
		if r.drifted {
			out.DriftedAny = true
		}
		out.Sessions = append(out.Sessions, sj)
	}
	if review != nil {
		rj := &lensTryReviewJSON{Model: review.model, Drifted: review.drifted, Facets: []lensTryFacetJSON{}}
		if review.err != nil {
			rj.Error = review.err.Error()
		}
		for _, f := range review.facets {
			bo := f.BecauseOf
			if bo == nil {
				bo = []string{}
			}
			rj.Facets = append(rj.Facets, lensTryFacetJSON{
				Dimension: f.Dimension, Key: f.Key, Value: f.Value,
				Confidence: f.Confidence, BecauseOf: bo, Contradicts: f.Contradicts,
			})
		}
		out.Review = rj
	}
	return emitJSON(out)
}
