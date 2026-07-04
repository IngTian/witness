package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/IngTian/claude-witness/internal/distill"
	"github.com/IngTian/claude-witness/internal/embed"
	"github.com/IngTian/claude-witness/internal/store"
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
				Runner:          cfg.Runner,
				TriageModel:     cfg.TriageModel,
				DistillModel:    cfg.DistillModel,
				ReviewEvery:     cfg.ReviewEvery,
				ReviewPoignancy: cfg.ReviewPoignancy,
			},
			OpenCodeModels: opencodeModels,
			Archive: doctorArchiveJSON{
				Sessions:      stat.Sessions,
				RawRecords:    stat.RawRecords,
				Observations:  stat.Observations,
				Facets:        stat.Facets,
				Pending:       stat.Pending,
				BackedOff:     stat.BackedOff,
				LastReview:    lastReview,
			},
			Embedder: doctorEmbedderJSON{
				Status:       embedderStatus,
				Dimension:    embedDim,
				ENZHCosine:   enZh,
				ENUnrelated:  enUnrelated,
			},
		}
		return emitJSON(out)
	}

	fmt.Println("claude-witness doctor")
	fmt.Printf("  data root: %s\n", st.Root)
	fmt.Printf("  runner: %s | models: triage=%s distill=%s | review_every=%d poignancy=%d\n",
		cfg.Runner, cfg.TriageModel, cfg.DistillModel, cfg.ReviewEvery, cfg.ReviewPoignancy)
	if strings.EqualFold(cfg.Runner, "opencode") {
		fmt.Printf("  opencode models: %s\n", opencodeModels)
	}
	fmt.Printf("  archive: %d sessions, %d raw messages, %d observations, %d facets\n",
		stat.Sessions, stat.RawRecords, stat.Observations, stat.Facets)
	fmt.Printf("  queue: %d pending, %d backing off | last review: %s\n",
		stat.Pending, stat.BackedOff, lastReview)
	if stat.BackedOff > 0 {
		fmt.Println("  ⚠ sessions are backing off — mining is failing; check witness.log")
	}
	fmt.Println("  profile: collect-only (never injected); read via `witness profile`, MCP get_profile/get_facets, or query witness.db")
	fmt.Printf("  embedder: %s", embedderStatus)
	if embedDim > 0 {
		fmt.Printf(" (dim=%d)", embedDim)
	}
	fmt.Println()
	if embedDim > 0 {
		fmt.Printf("  EN<->ZH cosine: %.4f | EN<->unrelated: %.4f (want first > second)\n", enZh, enUnrelated)
	}
	return deferredErr
}

type doctorJSON struct {
	DataRoot       string             `json:"data_root"`
	Config         doctorConfigJSON   `json:"config"`
	OpenCodeModels string             `json:"opencode_models"`
	Archive        doctorArchiveJSON  `json:"archive"`
	Embedder       doctorEmbedderJSON `json:"embedder"`
}

type doctorConfigJSON struct {
	Runner          string `json:"runner"`
	TriageModel     string `json:"triage_model"`
	DistillModel    string `json:"distill_model"`
	ReviewEvery     int    `json:"review_every"`
	ReviewPoignancy int    `json:"review_poignancy"`
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
