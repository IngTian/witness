package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newWorkerCmd is the HIDDEN operator group for the distillation worker. A normal
// user never touches it — the worker runs off editor hooks. It exists as an escape
// hatch: force a full re-distill, stop a run, or force a review. Hidden (not in
// `witness --help`) but `witness worker --help` works. Replaces the old top-level
// `distill` + `review` verbs (the pipeline vocabulary left the front door).
func newWorkerCmd() *cobra.Command {
	w := &cobra.Command{
		Use:    "worker",
		Short:  "Operator controls for the background distillation worker.",
		Hidden: true,
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

	w.AddCommand(&cobra.Command{
		Use:   "review",
		Short: "Force an L2 review and regenerate profiles from existing observations.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdReview() },
	})

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
