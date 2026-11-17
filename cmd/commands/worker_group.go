package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newWorkerCmd is the operator group for the distillation worker. Normally the worker
// runs off editor hooks, but some jobs happen ONLY here: distilling at all with
// auto_distill = false, stopping a run. It was hidden until #199 (F046), while doctor,
// status, compact, lens and the MCP server all printed `witness worker …` hints — so the
// CLI named a command its own --help said did not exist. Listed under Maintenance.
// Replaces the old top-level `distill` + `review` verbs (the pipeline vocabulary left the
// front door).
func newWorkerCmd() *cobra.Command {
	w := &cobra.Command{
		Use:     "worker",
		Short:   "Operator controls for the background distillation worker.",
		GroupID: groupAdmin,
		RunE:    groupRunE, // `witness worker stpo` is an unknown command, not its help page and exit 0
	}

	var quiet, detach, waitBackoffs bool
	var since, until string
	run := &cobra.Command{
		Use:   "run",
		Short: "Distill the pending backlog now (foreground by default; --detach to background).",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if detach {
				if waitBackoffs {
					return fmt.Errorf("--wait-backoffs applies only to the foreground drain; drop --detach")
				}
				return cmdDistillStart(quiet, since, until)
			}
			return cmdDistillBackfill(quiet, since, until, waitBackoffs)
		},
	}
	run.Flags().BoolVar(&detach, "detach", false, "run in the background instead of the foreground")
	run.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable status output")
	run.Flags().StringVar(&since, "since", "", "only sessions updated at/after this time (e.g. 7d or 2026-07-01)")
	run.Flags().StringVar(&until, "until", "", "only sessions updated at/before this time")
	run.Flags().BoolVar(&waitBackoffs, "wait-backoffs", false, "wait out transient mining backoffs and retry (foreground only)")
	w.AddCommand(run)

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Ask the running worker to stop.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdDistillStop() },
	}
	w.AddCommand(stop)

	w.AddCommand(&cobra.Command{
		Use:   "review",
		Short: "Force an L2 review and regenerate profiles from existing observations.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdReview() },
	})
	return w
}
