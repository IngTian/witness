package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:     "doctor",
		GroupID: groupSetup,
		Short:   "Run a health check for the archive and embedder.",
		Long:    "Run a health check for configuration, archive statistics, worker queue state, model availability, and multilingual embedder retrieval quality.",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdDoctor(asJSON)
		},
	}
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	return c
}

func cmdDoctor(asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	cfg := st.LoadConfig()
	// Report the EFFECTIVE runner, resolving the WITNESS_RUNNER env fallback so an
	// npm OpenCode user (who never bound a runner via install) sees "opencode",
	// not the misleading template default "claude". Matches what the worker uses.
	cfg.Runner = st.ResolveRunner(cfg)
	stat := st.Stats(activeLensNames(st))
	// prose_drift surfacing (#57): a monotonic count of passes where a lens's model
	// returned no JSON array (likely a below-floor triage model), with the last event's
	// when/lens so the count reads as dated rather than "broken forever".
	driftTotal := st.DriftTotal()
	driftLastTS, driftLastLens := st.DriftLast()

	// Resolve the runner once; doctor asks IT how it runs and whether its models are
	// valid — no branching on the runner name. runnerCmd is the runner's own
	// invocation hint (e.g. "claude -p" / "opencode serve"); modelStatus is the
	// result of the runner's model check (Claude's is a no-op → "OK", nothing to
	// validate and no cost; OpenCode shells to `opencode models`).
	//
	// Don't short-circuit on a bad model: the embedder check below is doctor's core
	// purpose and must run even when distillation is misconfigured. Remember the
	// failure and surface it as the exit code at the very end.
	var deferredErr error
	runnerCmd := "unknown"
	modelStatus := "unknown"
	defaultRunner, rerr := platform.RunnerFor(st, cfg)
	if rerr != nil {
		modelStatus = "INVALID: " + rerr.Error()
		deferredErr = rerr
	} else {
		runnerCmd = defaultRunner.InvocationHint()
		// Validate each RUNTIME's models against ITS OWN runner (#75 slice 2): the default
		// stage models on the default runner, and every per-lens model on the runtime that
		// lens routes to. A per-lens OpenCode model must be checked by the OpenCode runner,
		// not the default (Claude) one — validating it against the wrong runtime would either
		// falsely pass (Claude's no-op) or falsely fail. So we mint one runner per runtime
		// the active lenses use and validate that runtime's model set. A bad per-lens model
		// otherwise only surfaces late as a per-lens mining backoff. (On Claude ValidateModels
		// is a no-op, so a bogus-for-claude model isn't machine-catchable — it shows in
		// `lens show` and surfaces at runtime via backoff.)
		if err := validateRuntimeModels(context.Background(), st, cfg); err != nil {
			modelStatus = "INVALID: " + err.Error()
			deferredErr = err
		} else {
			modelStatus = "OK"
		}
	}

	// Embedder check — load the model and verify EN/ZH retrieval sanity.
	embedderStatus := "OK"
	var enZh, enUnrelated float64
	var embedDim int
	emb, embErr := embed.New()
	if embErr != nil {
		embedderStatus = "UNAVAILABLE: " + embErr.Error()
		deferredErr = embErr
	} else {
		en, err := emb.Embed("I resolve uncertainty by running a cheap experiment.")
		if err != nil {
			return fmt.Errorf("embed EN: %w", err)
		}
		zh, err := emb.Embed("我通过做一个便宜的实验来解决不确定性。")
		if err != nil {
			return fmt.Errorf("embed ZH: %w", err)
		}
		un, _ := emb.Embed("The quarterly revenue report is due Friday.")
		embedDim = len(en)
		enZh = embed.Cosine(en, zh)
		enUnrelated = embed.Cosine(en, un)
	}

	lastReview := stat.LastReview
	if lastReview == "" {
		lastReview = "never"
	}

	if asJSON {
		out := doctorJSON{
			DataRoot: st.Root,
			Config: doctorConfigJSON{
				Runner:          cfg.Runner,
				TriageModel:     cfg.TriageModel,
				DistillModel:    cfg.DistillModel,
				ReviewEvery:     cfg.ReviewEvery,
				ReviewPoignancy: cfg.ReviewPoignancy,
				AutoDistill:     cfg.AutoDistill,
				MineConcurrency: cfg.MineConcurrency,
				ChunkMaxChars:   cfg.ChunkMaxChars,
				ReviewMaxChars:  cfg.ReviewMaxChars,
			},
			ModelCheck: modelStatus,
			Archive: doctorArchiveJSON{
				Sessions:        stat.Sessions,
				RawRecords:      stat.RawRecords,
				Observations:    stat.Observations,
				Facets:          stat.Facets,
				Pending:         stat.Pending,
				BackedOff:       stat.BackedOff,
				DriftedSessions: stat.Drifted,
				DriftEvents:     driftTotal,
				DriftLast:       driftLastTS,
				DriftLastLens:   driftLastLens,
				LastReview:      lastReview,
			},
			Embedder: doctorEmbedderJSON{
				Status:      embedderStatus,
				Dimension:   embedDim,
				ENZHCosine:  enZh,
				ENUnrelated: enUnrelated,
			},
		}
		// Emit the payload, THEN surface the failure. Dropping deferredErr here made
		// `witness doctor --json` exit 0 on an archive that cannot distill at all — a missing
		// embedder model, a typo'd runner, an OpenCode model the provider rejects — while the
		// human path returns it at the end of this function and exits 1. So the machine-readable
		// health check, which is exactly the form a CI step or install-verify script uses
		// (`witness doctor --json || fail`, the shape install.sh and `make doctor` point users
		// at), reported healthy while nothing could ever be mined.
		//
		// Order matters: the report still goes to stdout in full, because that payload is the
		// whole point of --json (a consumer must still see embedder.status / model_check).
		// reportError renders {"error": …} on STDERR for --json callers, so `witness doctor
		// --json | jq .` keeps parsing a clean document either way.
		if err := emitJSON(out); err != nil {
			return err
		}
		return deferredErr
	}

	// --- human-readable report (decorative; --json is handled above) ----------
	// Overall status glyph: bad if the embedder is down (doctor's core check
	// failed), warn if distillation is stuck (sessions backing off) or an
	// opencode model is misconfigured, else ok.
	// An archive that has captured nothing is not healthy, however clean the rest reads: every
	// read command then comes back empty, and doctor is where a new user goes to ask why.
	overall := okGlyph()
	if embErr != nil {
		overall = badGlyph()
	} else if stat.BackedOff > 0 || driftTotal > 0 || deferredErr != nil || stat.Sessions == 0 {
		overall = warnGlyph()
	}
	fmt.Printf("%s  %s\n", overall, title("doctor", tildePath(st.Root), ""))
	row := func(key, value, note string) { fmt.Println("    " + kvRow(key, value, note)) }
	// Warnings are a short colored headline; the why and the fix go on the dim lines under it.
	warn := func(headline string) { fmt.Printf("    %s %s\n", warnGlyph(), yellow(headline)) }
	because := func(why string) { fmt.Println("      " + dim(wrapHanging(why, 6))) }
	fix := func(label, command string) { fmt.Println("  " + hint(label, command)) }
	// Lenses are named by what the user registered them as; the stats key on the mined name.
	shown := func(mined string) string { return lensRegisteredName(st, mined) }

	// Runner block. This is the DEFAULT runner: one process distills every session
	// regardless of source (Claude Code and OpenCode both feed the same L0), and it
	// serves every lens that doesn't override it. So make the active backend, its
	// models, and how to change them explicit —
	// otherwise a user who installed both is left guessing which LLM mines their
	// sessions. runnerCmd is the runner's own invocation hint (set above). The models are
	// the EFFECTIVE ones (an empty review_model runs on mine_model), with their origin.
	fmt.Println()
	fmt.Println("  " + bold("Distillation"))
	eff := effectiveConfig(st, nil)
	row("runner", cyan(cfg.Runner), "via "+runnerCmd+" · "+eff[0].from)
	for _, s := range eff[1:] {
		row(s.key, settingValue(s), s.from)
	}
	// Show the runner's model check unless it's the trivial "OK" with no models to
	// check (Claude's no-op): a non-OK status, or any status when models are set, is
	// worth surfacing. Runner-neutral — no branch on the runner name.
	if cause, bad := strings.CutPrefix(modelStatus, "INVALID: "); bad {
		row("models", badGlyph()+" "+red("invalid"), "")
		because(cause)
	} else if modelStatus != "OK" || strings.TrimSpace(cfg.TriageModel) != "" || strings.TrimSpace(cfg.DistillModel) != "" {
		row("models", okGlyph()+" "+modelStatus, "")
	}
	cadence := countNoun(cfg.ReviewEvery, "session", "sessions")
	if cfg.ReviewPoignancy > 0 {
		cadence += fmt.Sprintf(", or at poignancy %d", cfg.ReviewPoignancy)
	}
	row("review", "every "+cadence, "")
	auto := "off"
	if cfg.AutoDistill {
		auto = "on"
	}
	row("worker", fmt.Sprintf("auto %s · %d parallel · chunking %s", auto, cfg.MineConcurrency, chunkingLabel(cfg.ChunkMaxChars)),
		"review window "+humanCount(cfg.ReviewMaxChars)+" chars")
	// Surface prose_drift: the mine model returned no JSON observation array on some
	// pass, so those sessions distilled to zero observations even though they may not be
	// uneventful. The remedy is a stronger mine model then a re-mine (#57) — of the lens it
	// happened on, by its registered name, never a <lens> placeholder.
	if driftTotal > 0 {
		headline := "prose drift: " + countNoun(driftTotal, "event", "events")
		if driftLastTS != "" {
			headline += ", last " + shortTime(driftLastTS)
		}
		lensFlag, backfill := "", "witness lens backfill <lens> --fresh"
		if driftLastLens != "" {
			name := shown(driftLastLens)
			headline += " (lens " + name + ")"
			lensFlag, backfill = " --lens "+name, "witness lens backfill "+name+" --fresh"
		}
		warn(headline)
		because("the mine model may be too weak to return the observation array")
		fix("raise it", "witness config set mine_model <stronger>"+lensFlag)
		fix("then re-mine", backfill)
	}
	// `witness install` took a <claude|opencode> target before the install/wire split; it is
	// now Args: cobra.NoArgs, so the old hint names a form that errors. Switching the runner
	// is `config set runner` — what wire's own help points at — not a re-wire of the editor.
	fix("set runner", "witness config set runner <claude|opencode>")
	fix("set a model", "witness config set <mine_model|review_model> <model>")

	fmt.Println()
	fmt.Println("  " + bold("Archive"))
	row("layers", fmt.Sprintf("%s · %s raw messages · %s · %s",
		countNoun(stat.Sessions, "session", "sessions"), humanCount(stat.RawRecords),
		countNoun(stat.Observations, "observation", "observations"), countNoun(stat.Facets, "facet", "facets")), "")
	if stat.Sessions == 0 {
		warn("nothing captured yet — no editor is feeding this archive")
		fix("wire editor", wireCommand())
	}
	// "N drifted" is the CURRENTLY-outstanding count (#69-2): distinct sessions whose
	// last mine of some lens drifted and hasn't cleanly re-mined since. It drops as
	// sessions recover, unlike the lifetime "drift events" total surfaced in the
	// Distillation block above. Shown only when > 0 to keep a healthy archive's line clean.
	queueLine := fmt.Sprintf("%s pending · %s backing off", humanCount(stat.Pending), humanCount(stat.BackedOff))
	if stat.Drifted > 0 {
		queueLine += fmt.Sprintf(" · %s drifted", humanCount(stat.Drifted))
	}
	if stat.BackedOff > 0 {
		queueLine = yellow(queueLine)
	}
	row("queue", queueLine, "last review "+shortTime(lastReview))
	if stat.BackedOff > 0 {
		warn(countNoun(stat.BackedOff, "session", "sessions") + " backing off — mining is failing")
		// The cause, on the screen: the backoff itself is logged without it, so this is the most
		// recent error the worker did log.
		if cause := lastLoggedError(st.LogPath()); cause != "" {
			because("last error: " + cause)
		}
		because("full log: " + tildePath(st.LogPath()))
	}
	row("profile", dim("collect-only, never injected; read with witness profile or MCP get_profile"), "")
	// Per-lens unreviewed backlog (#123): observations mined past the lens's review
	// watermark but not yet folded into facets. Normally small (steady-state deltas fold
	// in one window). A LARGE, persistent backlog is the visible signal of a frozen fold —
	// surfaced here so a stall is actionable, not silent. Windowed review (ReviewMaxChars)
	// keeps this draining; a backlog that stays large despite reviews warrants a look at
	// witness.log for a per-lens review error.
	for _, ln := range activeLensNames(st) {
		if d := st.UnreviewedDelta(ln); d > 0 {
			line := fmt.Sprintf("%s: %s not yet folded into facets", shown(ln), countNoun(d, "observation", "observations"))
			if d > cfg.ReviewMaxChars/200 { // ~more than one window's worth of obs pending — nudge
				warn(line)
				because("review is behind; it folds in windows")
				fix("catch up", "witness worker review")
			} else {
				row("unreviewed", dim(line), "")
			}
		}
	}

	fmt.Println()
	fmt.Println("  " + bold("Embedder"))
	if embErr != nil {
		row("status", badGlyph()+" "+red("unavailable"), "")
		because(strings.TrimPrefix(embedderStatus, "UNAVAILABLE: "))
	} else {
		status := embedderStatus
		if embedDim > 0 {
			status = fmt.Sprintf("%s (dim=%d)", embedderStatus, embedDim)
		}
		row("status", okGlyph()+" "+green(status), "")
		if embedDim > 0 {
			pair := glyph("↔", "<->")
			retrieval := fmt.Sprintf("EN%sZH %.4f  >  EN%sunrelated %.4f", pair, enZh, pair, enUnrelated)
			mark := okGlyph()
			if enZh <= enUnrelated {
				mark = warnGlyph()
				retrieval = yellow(retrieval + "  (expected EN" + pair + "ZH higher!)")
			}
			row("retrieval", mark+" "+retrieval, "")
		}
	}
	return deferredErr
}

// lastLoggedError is the err field of the newest witness.log line that has one — the cause behind
// "sessions backing off", which the backoff line itself does not carry. Only the log's tail is
// read; "" when there is no log or no error in it.
func lastLoggedError(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 256 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		_, _ = f.Seek(fi.Size()-tail, io.SeekStart)
	}
	data, _ := io.ReadAll(f)
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Err string `json:"err"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) == nil && rec.Err != "" {
			return rec.Err
		}
	}
	return ""
}

// chunkingLabel renders the ChunkMaxChars knob for the doctor report: the default
// (<=0) reads "whole" (each session mined in one call — the best-quality path per
// #57), a positive budget shows the char cap a session is split at.
func chunkingLabel(maxChars int) string {
	if maxChars <= 0 {
		return "whole"
	}
	return fmt.Sprintf("%d chars", maxChars)
}

type doctorJSON struct {
	DataRoot   string             `json:"data_root"`
	Config     doctorConfigJSON   `json:"config"`
	ModelCheck string             `json:"model_check"` // runner's model validation: "OK" / "INVALID: …" (runner-neutral)
	Archive    doctorArchiveJSON  `json:"archive"`
	Embedder   doctorEmbedderJSON `json:"embedder"`
}

type doctorConfigJSON struct {
	Runner          string `json:"runner"`
	TriageModel     string `json:"triage_model"`
	DistillModel    string `json:"distill_model"`
	ReviewEvery     int    `json:"review_every"`
	ReviewPoignancy int    `json:"review_poignancy"`
	AutoDistill     bool   `json:"auto_distill"`
	MineConcurrency int    `json:"mine_concurrency"`
	ChunkMaxChars   int    `json:"chunk_max_chars"`
	ReviewMaxChars  int    `json:"review_max_chars"`
}

type doctorArchiveJSON struct {
	Sessions        int    `json:"sessions"`
	RawRecords      int    `json:"raw_records"`
	Observations    int    `json:"observations"`
	Facets          int    `json:"facets"`
	Pending         int    `json:"pending"`
	BackedOff       int    `json:"backed_off"`
	DriftedSessions int    `json:"drifted_sessions"` // #69-2: sessions CURRENTLY at a drift; not omitempty (0 = none now)
	DriftEvents     int    `json:"drift_events"`     // not omitempty: 0 is a meaningful "no drift"
	DriftLast       string `json:"drift_last,omitempty"`
	DriftLastLens   string `json:"drift_last_lens,omitempty"`
	LastReview      string `json:"last_review"`
}

type doctorEmbedderJSON struct {
	Status      string  `json:"status"`
	Dimension   int     `json:"dimension,omitempty"`
	ENZHCosine  float64 `json:"en_zh_cosine,omitempty"`
	ENUnrelated float64 `json:"en_unrelated_cosine,omitempty"`
}

// validateRuntimeModels validates every runtime's models against ITS OWN runner (#75
// slice 2). It partitions the models by the runtime that will actually use them — the
// default stage models on the default runner, each per-lens model on the runtime its lens
// routes to — then mints each runtime's runner and calls its ValidateModels with that
// runtime's set. This is why a per-lens OpenCode model gets checked by `opencode models`
// and not the default (Claude) runner. Returns the first runtime's validation error (with
// the runtime named) so doctor's single model line stays meaningful.
func validateRuntimeModels(ctx context.Context, st *store.Store, cfg store.Config) error {
	// runtime name → deduped non-empty models to validate on it.
	perRuntime := map[string]map[string]bool{}
	add := func(runtime, m string) {
		m = strings.TrimSpace(m)
		if m == "" {
			return
		}
		if perRuntime[runtime] == nil {
			perRuntime[runtime] = map[string]bool{}
		}
		perRuntime[runtime][m] = true
	}
	defaultRunner := strings.TrimSpace(cfg.Runner)
	// Default stage models belong to the default runner.
	add(defaultRunner, cfg.TriageModel)
	add(defaultRunner, cfg.DistillModel)
	// Each lens's per-lens models belong to the runtime the lens routes to.
	if lenses, err := activeLenses(st); err == nil {
		for _, l := range lenses {
			rt := distill.RunnerFor(cfg, l)
			add(rt, l.ExtractModel)
			add(rt, l.ReviewModel)
		}
	}
	// Ensure the default runner is validated even with no models (it mints + is the report's
	// InvocationHint source; an unknown default-runner name is a real config error).
	if _, ok := perRuntime[defaultRunner]; !ok {
		perRuntime[defaultRunner] = map[string]bool{}
	}
	for runtime, set := range perRuntime {
		runner, err := platform.RunnerForName(runtime, cfg)
		if err != nil {
			return fmt.Errorf("runner %q: %w", runtime, err)
		}
		models := make([]string, 0, len(set))
		for m := range set {
			models = append(models, m)
		}
		if err := runner.ValidateModels(ctx, models...); err != nil {
			return fmt.Errorf("runner %q: %w", runtime, err)
		}
	}
	return nil
}
