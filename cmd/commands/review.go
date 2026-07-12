package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
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
	cfg.Runner = st.ResolveRunner(cfg)
	lenses, err := activeLenses(st)
	if err != nil {
		return err
	}
	ctx := context.Background()
	// Same runner lifecycle as the worker: Open before use, Close after. Close runs
	// the OpenCode self-traffic cleanup sweep — which this path previously OMITTED
	// (it deferred only the server's Close), leaking witness-distill sessions back
	// into the pending queue. Routing through the Runner makes that impossible.
	runner, err := platform.RunnerFor(st, cfg)
	if err != nil {
		return err
	}
	if err := runner.Open(ctx); err != nil {
		return err
	}
	defer runner.Close()
	runFn := distill.RunnerMine(runner)
	r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg, Runner: runFn}
	if err := r.Run(ctx, time.Now()); err != nil {
		return err
	}
	regenerateProfile(ctx, st, cfg, runFn)
	fmt.Println("review complete; profile regenerated")
	return nil
}
