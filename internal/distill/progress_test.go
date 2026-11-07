package distill

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// A progress bar that stalls short of its total, or whose total moves for a stage known up front,
// lies — worse than none. These pin the counts to the calls the runner actually received.

func stage(t *testing.T, p Progress, name string) Stage {
	t.Helper()
	for _, s := range p.Stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q stage in %+v", name, p)
	return Stage{}
}

func TestCompactProgressCountsEveryCall(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "value a", 0.7, 1), cf("thinking", "b", "value b", 0.7, 1),
		cf("thinking", "c", "value c", 0.7, 1), cf("thinking", "d", "value d", 0.7, 1),
	}}
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"a","value":"merged","absorb":["b","c"],"confidence":0.8,"why":"same"}]`)
	c.BatchChars = 21 // a, b, c (7 chars each) fill one batch; d is a second
	inner := c.Runner
	groups, writes := 0, 0
	c.Runner = func(ctx context.Context, model, system, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			writes++
		} else {
			groups++
		}
		return inner(ctx, model, system, input)
	}
	var evs []Progress
	c.Progress = func(p Progress) { evs = append(evs, p) }
	if _, err := c.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(evs) == 0 {
		t.Fatal("Plan reported no progress")
	}
	if g := stage(t, evs[0], "grouping"); g.Total != 2 || g.Done != 0 {
		t.Errorf("the first event must carry the full grouping total before any call: %+v", g)
	}
	for i := 1; i < len(evs); i++ {
		a, b := evs[i-1], evs[i]
		ga, gb := stage(t, a, "grouping"), stage(t, b, "grouping")
		ma, mb := stage(t, a, "writing merges"), stage(t, b, "writing merges")
		if gb.Total != ga.Total || gb.Done < ga.Done || mb.Done < ma.Done || mb.Total < ma.Total {
			t.Fatalf("progress went backwards or the grouping total moved: %+v -> %+v", a, b)
		}
		if mb.Open != (gb.Done < gb.Total) {
			t.Fatalf("merges must be Open exactly while grouping is unfinished: %+v", b)
		}
	}
	last := evs[len(evs)-1]
	g, m := stage(t, last, "grouping"), stage(t, last, "writing merges")
	if g.Done != g.Total || g.Done != groups {
		t.Errorf("grouping ended at %d/%d, but %d calls were made", g.Done, g.Total, groups)
	}
	if m.Done != m.Total || m.Done != writes || writes != 1 {
		t.Errorf("merges ended at %d/%d, but %d write calls were made (want 1)", m.Done, m.Total, writes)
	}
	if last.Where != "default/thinking" {
		t.Errorf("progress must say where it is: %q", last.Where)
	}
}

func TestClaimProgressCountsEveryBatch(t *testing.T) {
	var facets []store.Facet
	big := strings.Repeat("y", claimBatchChars/2) // two facets fill a batch: 6 facets, 3 batches
	for i := 0; i < 6; i++ {
		facets = append(facets, cf("thinking", string(rune('a'+i)), big, 0.8, 1))
	}
	for _, max := range []int{0, 1} {
		st := &fakeCompactStore{facets: append([]store.Facet(nil), facets...)}
		c := claimer(st, "[]")
		c.MaxBatches = max
		calls := 0
		c.Runner = func(context.Context, string, string, string) (string, error) { calls++; return "[]", nil }
		var evs []Progress
		c.Progress = func(p Progress) { evs = append(evs, p) }
		c.Run(context.Background(), defaultLens()[0])

		want := 3
		if max == 1 {
			want = 1 // the bound is part of the total, or the bar would stop at 1/3
		}
		if len(evs) == 0 {
			t.Fatalf("MaxBatches=%d: no progress reported", max)
		}
		if s := stage(t, evs[0], "writing claims"); s.Total != want || s.Done != 0 {
			t.Fatalf("MaxBatches=%d: the first event must carry the total %d: %+v", max, want, s)
		}
		last := evs[len(evs)-1]
		if s := stage(t, last, "writing claims"); s.Done != want || calls != want || last.Where != "default" {
			t.Errorf("MaxBatches=%d: ended at %d/%d after %d calls: %+v", max, s.Done, s.Total, calls, last)
		}
	}
}
