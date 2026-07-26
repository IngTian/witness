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
	if !c.Hidden {
		t.Error("worker group must be hidden (operator escape hatch, not front door)")
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
