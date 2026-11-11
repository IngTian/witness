package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestWorkerGroupShape(t *testing.T) {
	c := newWorkerCmd()
	if c.Use != "worker" {
		t.Fatalf("Use = %q, want worker", c.Use)
	}
	// Listed, not hidden (#199 F046): witness's own hints send people to `witness worker …`,
	// and with auto_distill = false it is the only way to distill.
	if c.Hidden || c.GroupID != groupAdmin {
		t.Errorf("worker group must be listed under Maintenance; Hidden=%v GroupID=%q", c.Hidden, c.GroupID)
	}
	subs := map[string]bool{}
	for _, s := range c.Commands() {
		subs[s.Name()] = true
	}
	for _, want := range []string{"run", "stop", "review"} {
		if !subs[want] {
			t.Errorf("worker missing subcommand %q", want)
		}
	}
	// `run` carries the drain flags.
	var run *cobra.Command
	for _, s := range c.Commands() {
		if s.Name() == "run" {
			run = s
		}
	}
	for _, f := range []string{"detach", "wait-backoffs", "since", "until"} {
		if run.Flags().Lookup(f) == nil {
			t.Errorf("worker run missing --%s", f)
		}
	}
}
