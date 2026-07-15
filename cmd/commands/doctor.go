package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Run a health check for the archive and embedder.",
		Long:  "Run a health check for configuration, archive statistics, worker queue state, model availability, and multilingual embedder retrieval quality.",
		Args:  cobra.NoArgs,
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
	runner, rerr := platform.RunnerFor(st, cfg)
	if rerr != nil {
		modelStatus = "INVALID: " + rerr.Error()
		deferredErr = rerr
	} else {
		runnerCmd = runner.InvocationHint()
		if err := runner.ValidateModels(context.Background(), cfg.TriageModel, cfg.DistillModel); err != nil {
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
			},
			ModelCheck: modelStatus,
			Archive: doctorArchiveJSON{
				Sessions:      stat.Sessions,
				RawRecords:    stat.RawRecords,
				Observations:  stat.Observations,
				Facets:        stat.Facets,
				Pending:       stat.Pending,
				BackedOff:     stat.BackedOff,
				DriftEvents:   driftTotal,
				DriftLast:     driftLastTS,
				DriftLastLens: driftLastLens,
				LastReview:    lastReview,
			},
			Embedder: doctorEmbedderJSON{
				Status:      embedderStatus,
				Dimension:   embedDim,
				ENZHCosine:  enZh,
				ENUnrelated: enUnrelated,
			},
		}
		return emitJSON(out)
	}

	// --- human-readable report (decorative; --json is handled above) ----------
	// Overall status glyph: bad if the embedder is down (doctor's core check
	// failed), warn if distillation is stuck (sessions backing off) or an
	// opencode model is misconfigured, else ok.
	overall := okGlyph()
	if embErr != nil {
		overall = badGlyph()
	} else if stat.BackedOff > 0 || driftTotal > 0 || deferredErr != nil {
		overall = warnGlyph()
	}
	fmt.Printf("%s  %s\n", overall, bold("witness doctor"))
	fmt.Printf("   %s %s\n", dim("data root:"), st.Root)

	// Runner block. The runner is GLOBAL: one process distills every session
	// regardless of source (Claude Code and OpenCode both feed the same L0), so
	// make the active backend, its models, and how to change them explicit —
	// otherwise a user who installed both is left guessing which LLM mines their
	// sessions. runnerCmd is the runner's own invocation hint (set above); empty
	// models mean "let the runner pick its environment default".
	fmt.Println()
	fmt.Println("  " + bold("Distillation"))
	fmt.Printf("    %s %s  %s\n", label("runner"), cyan(cfg.Runner),
		dim(fmt.Sprintf("(via `%s`; distills ALL sessions — Claude Code + OpenCode)", runnerCmd)))
	fmt.Printf("    %s triage=%s  distill=%s\n", label("models"),
		modelOrDefault(cfg.TriageModel, runnerCmd), modelOrDefault(cfg.DistillModel, runnerCmd))
	// Show the runner's model check unless it's the trivial "OK" with no models to
	// check (Claude's no-op): a non-OK status, or any status when models are set, is
	// worth surfacing. Runner-neutral — no branch on the runner name.
	if modelStatus != "OK" || strings.TrimSpace(cfg.TriageModel) != "" || strings.TrimSpace(cfg.DistillModel) != "" {
		fmt.Printf("    %s %s\n", label("models ok"), modelStatus)
	}
	fmt.Printf("    %s review_every=%d  poignancy=%d\n", label("review"), cfg.ReviewEvery, cfg.ReviewPoignancy)
	fmt.Printf("    %s enabled=%t  mine_concurrency=%d\n", label("auto"), cfg.AutoDistill, cfg.MineConcurrency)
	// Surface prose_drift: the triage model returned no JSON observation array on some
	// pass, so those sessions distilled to zero observations even though they may not be
	// uneventful. The remedy is a stronger triage model then a re-mine (#57).
	if driftTotal > 0 {
		last := "—"
		if driftLastTS != "" {
			last = driftLastTS
			if driftLastLens != "" {
				last += ", lens=" + driftLastLens
			}
		}
		fmt.Printf("    %s %s\n", warnGlyph(), yellow(fmt.Sprintf(
			"prose drift: %d event(s) (last %s) — triage model may be too weak to emit the observation array; raise triage_model, then `witness lens rebuild <lens>`",
			driftTotal, last)))
	}
	// Model-floor advisory (#57): a lens can declare `# model_floor:` (e.g. "sonnet").
	// Mining uses the single global triage model, so we can't enforce a per-lens floor
	// (that's #69) — but we CAN warn when the configured triage model tier-ranks BELOW a
	// lens's floor, since a below-floor model prose-drifts (silently extracts nothing).
	// Best-effort + non-blocking (respects distill-is-best-effort): an unrankable model
	// (a custom id / Bedrock ARN / the runner default) yields no warning, never an error.
	activeForFloor, _ := activeLenses(st)
	for _, w := range modelFloorWarnings(cfg.TriageModel, activeForFloor) {
		fmt.Printf("    %s %s\n", warnGlyph(), yellow(w))
	}
	fmt.Printf("    %s witness install <claude|opencode>  %s\n", dim("↳ switch runner:"), dim("(re-binds the runner)"))
	fmt.Printf("    %s edit %s  %s\n", dim("↳ set models:  "), st.ConfigPath(), dim("(triage_model, distill_model)"))

	fmt.Println()
	fmt.Println("  " + bold("Archive"))
	fmt.Printf("    %s %d sessions · %d raw messages · %d observations · %d facets\n",
		label("layers"), stat.Sessions, stat.RawRecords, stat.Observations, stat.Facets)
	queueLine := fmt.Sprintf("%d pending · %d backing off · last review: %s", stat.Pending, stat.BackedOff, lastReview)
	if stat.BackedOff > 0 {
		fmt.Printf("    %s %s\n", label("queue"), yellow(queueLine))
		fmt.Printf("    %s %s\n", warnGlyph(), yellow("sessions are backing off — mining is failing; check witness.log"))
	} else {
		fmt.Printf("    %s %s\n", label("queue"), queueLine)
	}
	fmt.Printf("    %s %s\n", label("profile"),
		dim("collect-only (never injected); read via `witness profile`, MCP get_profile/get_facets, or witness.db"))

	fmt.Println()
	fmt.Println("  " + bold("Embedder"))
	if embErr != nil {
		fmt.Printf("    %s %s %s\n", label("status"), badGlyph(), red(embedderStatus))
	} else {
		status := embedderStatus
		if embedDim > 0 {
			status = fmt.Sprintf("%s (dim=%d)", embedderStatus, embedDim)
		}
		fmt.Printf("    %s %s %s\n", label("status"), okGlyph(), green(status))
		if embedDim > 0 {
			retrieval := fmt.Sprintf("EN↔ZH %.4f  >  EN↔unrelated %.4f", enZh, enUnrelated)
			mark := okGlyph()
			if enZh <= enUnrelated {
				mark = warnGlyph()
				retrieval = yellow(retrieval + "  (expected EN↔ZH higher!)")
			}
			fmt.Printf("    %s %s %s\n", label("retrieval"), mark, retrieval)
		}
	}
	return deferredErr
}

// modelTier ranks a model string by capability using substring matching against the
// known Claude families, returning (tier, ok). Higher tier = more capable. ok is false
// when the string names no known family (a custom id, a Bedrock ARN, or "" = the
// runner's environment default) — an unrankable model must never trigger a warning,
// only silence, since we genuinely can't judge it. Substrings are matched most-capable
// first so an id containing two family names ranks by the strongest.
func modelTier(model string) (int, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0, false
	}
	switch {
	case strings.Contains(m, "opus"):
		return 3, true
	case strings.Contains(m, "sonnet"):
		return 2, true
	case strings.Contains(m, "haiku"):
		return 1, true
	default:
		return 0, false
	}
}

// modelFloorWarnings returns one advisory string per active lens whose declared
// ModelFloor tier-ranks ABOVE the configured triage model — the case where mining is
// likely to prose-drift (#57). It is purely informational: mining uses the single
// global triage model (a per-lens floor can't be enforced today — #69), so this only
// tells the user to raise triage_model. Returns nothing when either side is unrankable
// (custom id / ARN / runner default) — we don't warn on a model we can't judge.
func modelFloorWarnings(triageModel string, lenses []*lens.Lens) []string {
	triageTier, triageOK := modelTier(triageModel)
	if !triageOK {
		return nil // can't rank the configured model → no basis to warn
	}
	var out []string
	for _, l := range lenses {
		if l == nil || l.ModelFloor == "" {
			continue
		}
		floorTier, floorOK := modelTier(l.ModelFloor)
		if floorOK && triageTier < floorTier {
			out = append(out, fmt.Sprintf(
				"lens %q declares model_floor=%s but triage_model tier-ranks lower — it may prose-drift (extract nothing); raise triage_model to at least %s",
				l.Name, l.ModelFloor, l.ModelFloor))
		}
	}
	return out
}

// modelOrDefault renders an empty model setting as an explicit "(<runner>
// default)" so the user sees WHY a model column is blank (it's not broken —
// the runner picks its environment default) rather than an empty field.
func modelOrDefault(model, runnerCmd string) string {
	if strings.TrimSpace(model) == "" {
		return "(" + runnerCmd + " default)"
	}
	return model
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
}

type doctorArchiveJSON struct {
	Sessions      int    `json:"sessions"`
	RawRecords    int    `json:"raw_records"`
	Observations  int    `json:"observations"`
	Facets        int    `json:"facets"`
	Pending       int    `json:"pending"`
	BackedOff     int    `json:"backed_off"`
	DriftEvents   int    `json:"drift_events"` // not omitempty: 0 is a meaningful "no drift"
	DriftLast     string `json:"drift_last,omitempty"`
	DriftLastLens string `json:"drift_last_lens,omitempty"`
	LastReview    string `json:"last_review"`
}

type doctorEmbedderJSON struct {
	Status      string  `json:"status"`
	Dimension   int     `json:"dimension,omitempty"`
	ENZHCosine  float64 `json:"en_zh_cosine,omitempty"`
	ENUnrelated float64 `json:"en_unrelated_cosine,omitempty"`
}
