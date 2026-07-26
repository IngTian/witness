package commands

import "github.com/spf13/cobra"

// newStatusCmd is the top-level "witness status": the single read for "is it
// working — what's captured, what's distilling, is it fresh". It renders the same
// data the worker tracks (moved off the old `distill status` so a user never has to
// know the word "distill"). --json emits the machine schema unchanged.
func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:     "status",
		GroupID: groupRead,
		Short:   "Show what witness has captured and whether it's up to date.",
		Long:    "Show archive stats, the background worker's state, how many sessions are pending, and how fresh the distilled data is. --json emits the same fields for scripts.",
		Args:    cobra.NoArgs,
		RunE:    func(_ *cobra.Command, _ []string) error { return cmdDistillStatus(asJSON) },
	}
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	return c
}
