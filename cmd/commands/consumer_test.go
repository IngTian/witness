package commands

import (
	"sort"
	"testing"
)

// drainQueue must: process every pending job exactly once per run, pick up jobs
// that ARRIVE during the run, and terminate even if a job stays pending (a
// dead-lettering/failing session must not spin the loop forever).
func TestDrainQueueProcessesArrivalsOnceAndTerminates(t *testing.T) {
	pendingSet := map[string]bool{"A": true, "B": true, "stuck": true}
	var order []string

	process := func(s string) {
		order = append(order, s)
		if s == "A" {
			pendingSet["C"] = true // a new job arrives mid-run
		}
		if s != "stuck" {
			delete(pendingSet, s) // normal jobs clear; "stuck" never does
		}
	}
	pending := func() []string {
		out := []string{}
		for k := range pendingSet {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	drainQueue(pending, process) // must terminate

	counts := map[string]int{}
	for _, s := range order {
		counts[s]++
	}
	for _, s := range []string{"A", "B", "C", "stuck"} {
		if counts[s] != 1 {
			t.Errorf("%s processed %d times, want exactly 1", s, counts[s])
		}
	}
}

func TestDrainQueueLimitStopsAfterBudget(t *testing.T) {
	pendingSet := map[string]bool{"A": true, "B": true, "C": true}
	var order []string
	pending := func() []string {
		out := []string{}
		for k := range pendingSet {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	processed := drainQueueLimit(pending, func(s string) {
		order = append(order, s)
		delete(pendingSet, s)
	}, 1)

	if processed != 1 || len(order) != 1 || order[0] != "A" {
		t.Fatalf("processed=%d order=%v, want one first job", processed, order)
	}
	if !pendingSet["B"] || !pendingSet["C"] {
		t.Fatalf("budgeted drain should leave remaining jobs queued: %#v", pendingSet)
	}
}
