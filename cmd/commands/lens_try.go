package commands

import (
	"context"
	"fmt"
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
	trySessions int
	trySession  string
	tryModel    string
	tryRecent   bool
	tryJSON     bool
)

// newLensTryCmd builds the `try` subcommand. Unlike the other lens verbs (thin thunks
// into cmdLens), `try` carries its own flags, so it is a full command with a closure.
func newLensTryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "try <file>",
		Short: "Preview a lens's EXTRACT prompt on real sessions (read-only, writes nothing).",
		Long: "Mine real sessions from your archive through a CANDIDATE lens file and print the raw " +
			"observations it would produce — WITHOUT registering the lens or writing anything to the " +
			"archive. This is the prompt-tuning loop: edit the EXTRACT prompt, run `try`, see what " +
			"changes. Sessions are sampled largest-first by default (deterministic, and the meatiest " +
			"sessions are the ones a prompt is most likely to mishandle); use --recent for the latest.\n\n" +
			"Works on both runners. On an OpenCode runner it briefly holds the worker lock while it runs " +
			"(its shutdown sweep would otherwise disrupt a running worker); on Claude it needs no lock. " +
			"Sessions are previewed in parallel (bounded by mine_concurrency).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdLensTry(args[0], trySessions, trySession, tryModel, tryRecent, tryJSON)
		},
	}
	c.Flags().IntVar(&trySessions, "sessions", 3, "number of sessions to sample")
	c.Flags().StringVar(&trySession, "session", "", "preview one specific session id (bypasses sampling)")
	c.Flags().StringVar(&tryModel, "model", "", "override the triage model for this run (e.g. test above a drift floor without editing config)")
	c.Flags().BoolVar(&tryRecent, "recent", false, "sample the most recent sessions instead of the largest")
	c.Flags().BoolVarP(&tryJSON, "json", "j", false, "output as JSON")
	return c
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

type lensTryJSON struct {
	Lens       string               `json:"lens"`
	Model      string               `json:"model"`
	Candidate  bool                 `json:"candidate"` // true = shown name is a fallback (file's # name: was reserved)
	Sessions   []lensTrySessionJSON `json:"sessions"`
	TotalObs   int                  `json:"total_observations"`
	DriftedAny bool                 `json:"drifted_any"`
}

// cmdLensTry runs the read-only preview. It opens its OWN store (independent of cmdLens)
// and reads only — PreviewMine writes nothing (see distill.PreviewMine). On an OpenCode
// runner it holds the single-flight WorkerLock for the runner's whole lifetime because
// OpenCode's Close() runs a process-global sweep that would delete a concurrent worker's
// in-flight distill session; on Claude (no sweep) it stays lock-free.
func cmdLensTry(file string, nSessions int, oneSession, model string, recent, asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	// Load the candidate. Strict first; on a reserved-name collision fall back to the
	// lenient loader (preview never writes, so an impersonating name is harmless) and
	// mark it so the display makes the fallback obvious.
	candidate := false
	ln, err := lens.LoadFromFile(file)
	if err != nil {
		var uErr error
		if ln, uErr = lens.LoadFromFileUnchecked(file); uErr != nil {
			return uErr // a genuine load error (missing file / no EXTRACT) — surface it
		}
		// LoadFromFileUnchecked succeeded where LoadFromFile failed => reserved name.
		ln.Name = "candidate"
		candidate = true
	}

	cfg := st.LoadConfig()
	cfg.Runner = st.ResolveRunner(cfg)
	// --model overrides the triage model for THIS run. Must be applied BEFORE the runner
	// is minted/opened: OpenCode's Open starts `opencode serve` bound to cfg.TriageModel,
	// so a later override would silently not reach the server. (Claude passes it per Run,
	// so timing is looser there, but set it here uniformly.)
	if model != "" {
		cfg.TriageModel = model
	}

	// Resolve the session set (validation happens BEFORE any runner work, so a bad file/
	// session never spawns a server or takes a lock).
	sessions, err := resolveTrySessions(st, nSessions, oneSession, recent)
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

	if asJSON {
		return lensTryEmitJSON(st, cfg, ln, candidate, sessions, results)
	}
	return lensTryRenderHuman(st, cfg, ln, candidate, sessions, results)
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
func lensTryRenderHuman(st *store.Store, cfg store.Config, ln *lens.Lens, candidate bool, sessions []string, results []tryResult) error {
	name := ln.Name
	if candidate {
		name += dim(" (candidate — file's name was reserved)")
	}
	fmt.Printf("%s %s   %s %s\n", label("lens"), bold(name), dim("model:"), modelLabel(cfg))
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
	return nil
}

func lensTryEmitJSON(st *store.Store, cfg store.Config, ln *lens.Lens, candidate bool, sessions []string, results []tryResult) error {
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
	return emitJSON(out)
}
