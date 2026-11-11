package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newWorkerCmd is the operator group for the distillation worker. Normally the worker
// runs off editor hooks, but some jobs happen ONLY here: distilling at all with
// auto_distill = false, the long-arc pass (`review --full`), stopping a run. It was
// hidden until #199 (F046), while doctor, status, compact, lens and the MCP server all
// printed `witness worker …` hints — so the CLI named a command its own --help said did
// not exist. Listed under Maintenance. Replaces the old top-level `distill` + `review`
// verbs (the pipeline vocabulary left the front door).
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

	var stopAutoOnly bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Ask the running worker to stop.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdDistillStop(stopAutoOnly) },
	}
	stop.Flags().BoolVar(&stopAutoOnly, "auto-only", false, "stop only an automatically-started worker")
	_ = stop.Flags().MarkHidden("auto-only")
	w.AddCommand(stop)

	var reviewFull bool
	reviewCmd := &cobra.Command{
		Use:   "review",
		Short: "Force an L2 review and regenerate profiles from existing observations.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdReviewFull(reviewFull) },
	}
	reviewCmd.Flags().BoolVar(&reviewFull, "full", false,
		"also run the emergent long-arc pass (cluster L1 → verify → merge; #16)")
	w.AddCommand(reviewCmd)

	// candidates: the S3 long-arc dry-run inspector. Prints the emergent-arc clusters the
	// hypothesis engine proposes for a lens WITHOUT verifying (no LLM, no writes) — so
	// cluster quality can be judged on a real archive before any verify budget is spent.
	w.AddCommand(&cobra.Command{
		Use:   "candidates <lens>",
		Short: "Dry-run the emergent-arc clustering for a lens (no LLM, no writes).",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return cmdCandidates(args[0]) },
	})
	return w
}
