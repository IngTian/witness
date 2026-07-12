package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/embed"
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
	stat := st.Stats()

	// Don't short-circuit on a bad OpenCode model: the embedder check below is
	// doctor's core purpose (verify the model loads and EN/ZH retrieval works),
	// and it must run even when distillation is misconfigured. Remember the
	// failure and surface it as the exit code at the very end.
	var deferredErr error
	opencodeModels := "skipped"
	if strings.EqualFold(cfg.Runner, "opencode") {
		if err := distill.ValidateOpenCodeModels(context.Background(), cfg.TriageModel, cfg.DistillModel); err != nil {
			opencodeModels = "INVALID: " + err.Error()
			deferredErr = err
		} else {
			opencodeModels = "OK"
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
				Runner:                     cfg.Runner,
				TriageModel:                cfg.TriageModel,
				DistillModel:               cfg.DistillModel,
				ReviewEvery:                cfg.ReviewEvery,
				ReviewPoignancy:            cfg.ReviewPoignancy,
				AutoDistill:                cfg.AutoDistill,
				AutoDistillIntervalMinutes: cfg.AutoDistillIntervalMinutes,
				AutoDistillSessionBudget:   cfg.AutoDistillSessionBudget,
			},
			OpenCodeModels: opencodeModels,
			Archive: doctorArchiveJSON{
				Sessions:     stat.Sessions,
				RawRecords:   stat.RawRecords,
				Observations: stat.Observations,
				Facets:       stat.Facets,
				Pending:      stat.Pending,
				BackedOff:    stat.BackedOff,
				LastReview:   lastReview,
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
	} else if stat.BackedOff > 0 || deferredErr != nil {
		overall = warnGlyph()
	}
	fmt.Printf("%s  %s\n", overall, bold("witness doctor"))
	fmt.Printf("   %s %s\n", dim("data root:"), st.Root)

	// Runner block. The runner is GLOBAL: one process distills every session
	// regardless of source (Claude Code and OpenCode both feed the same L0), so
	// make the active backend, its models, and how to change them explicit —
	// otherwise a user who installed both is left guessing which LLM mines their
	// sessions. Empty models mean "let the runner pick its environment default".
	runnerCmd := "claude -p"
	if strings.EqualFold(strings.TrimSpace(cfg.Runner), "opencode") {
		runnerCmd = "opencode serve"
	}
	fmt.Println()
	fmt.Println("  " + bold("Distillation"))
	fmt.Printf("    %s %s  %s\n", label("runner"), cyan(cfg.Runner),
		dim(fmt.Sprintf("(via `%s`; distills ALL sessions — Claude Code + OpenCode)", runnerCmd)))
	fmt.Printf("    %s triage=%s  distill=%s\n", label("models"),
		modelOrDefault(cfg.TriageModel, runnerCmd), modelOrDefault(cfg.DistillModel, runnerCmd))
	if strings.EqualFold(strings.TrimSpace(cfg.Runner), "opencode") {
		fmt.Printf("    %s %s\n", label("oc models"), opencodeModels)
	}
	fmt.Printf("    %s review_every=%d  poignancy=%d\n", label("review"), cfg.ReviewEvery, cfg.ReviewPoignancy)
	fmt.Printf("    %s enabled=%t  interval=%dm  session_budget=%d\n", label("auto"), cfg.AutoDistill, cfg.AutoDistillIntervalMinutes, cfg.AutoDistillSessionBudget)
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
	DataRoot       string             `json:"data_root"`
	Config         doctorConfigJSON   `json:"config"`
	OpenCodeModels string             `json:"opencode_models"`
	Archive        doctorArchiveJSON  `json:"archive"`
	Embedder       doctorEmbedderJSON `json:"embedder"`
}

type doctorConfigJSON struct {
	Runner                     string `json:"runner"`
	TriageModel                string `json:"triage_model"`
	DistillModel               string `json:"distill_model"`
	ReviewEvery                int    `json:"review_every"`
	ReviewPoignancy            int    `json:"review_poignancy"`
	AutoDistill                bool   `json:"auto_distill"`
	AutoDistillIntervalMinutes int    `json:"auto_distill_interval_minutes"`
	AutoDistillSessionBudget   int    `json:"auto_distill_session_budget"`
}

type doctorArchiveJSON struct {
	Sessions     int    `json:"sessions"`
	RawRecords   int    `json:"raw_records"`
	Observations int    `json:"observations"`
	Facets       int    `json:"facets"`
	Pending      int    `json:"pending"`
	BackedOff    int    `json:"backed_off"`
	LastReview   string `json:"last_review"`
}

type doctorEmbedderJSON struct {
	Status      string  `json:"status"`
	Dimension   int     `json:"dimension,omitempty"`
	ENZHCosine  float64 `json:"en_zh_cosine,omitempty"`
	ENUnrelated float64 `json:"en_unrelated_cosine,omitempty"`
}
