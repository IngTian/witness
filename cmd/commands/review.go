package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/IngTian/claude-witness/internal/distill"
	"github.com/IngTian/claude-witness/internal/store"
	"github.com/spf13/cobra"
)

func newReviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "review",
		Short: "Force an L2 review and regenerate profiles.",
		Long:  "Force an L2 review from existing observations, update facets, and regenerate the derived L4 markdown profiles. This writes derived data but does not capture new raw turns.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdReview()
		},
	}
}

func cmdReview() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()
	cfg := st.LoadConfig()
	lenses, err := activeLenses(st)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var runFn distill.MineFunc
	if strings.EqualFold(strings.TrimSpace(cfg.Runner), "opencode") {
		opencodeServer, err := distill.StartOpenCodeServer(ctx, cfg.TriageModel, cfg.DistillModel)
		if err != nil {
			return err
		}
		defer opencodeServer.Close()
		runFn = opencodeServer.Run
	}
	r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn}
	if err := r.Run(ctx, time.Now()); err != nil {
		return err
	}
	regenerateProfile(ctx, st, cfg, runFn)
	fmt.Println("review complete; profile regenerated")
	return nil
}
