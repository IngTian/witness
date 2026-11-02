package distill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// The review fold's progress is pinned to the calls the fake runner actually received: the window
// plan is fixed before the first call, so the total must be exact from the first event and never
// move — including when a window fails and the lens stops there.
func TestReviewProgressCountsEveryWindow(t *testing.T) {
	for _, failOn := range []int{0, 2} {
		s := newStore(t)
		bigObs(t, s, 9)
		calls := 0
		r := &Reviewer{
			Store:  s,
			Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}, {Name: "quiet", Review: "REVIEW"}},
			Config: store.Config{ReviewMaxChars: 1000}, // several windows of 2 obs, so windows != obs
			Runner: func(context.Context, string, string, string) (string, error) {
				calls++
				if calls == failOn {
					return "", errors.New("simulated review timeout")
				}
				return facetReply("thinking", "k", "v"), nil
			},
		}
		var evs []Progress
		r.Progress = func(p Progress) { evs = append(evs, p) }
		err := r.Run(context.Background(), time.Now())
		if (err != nil) != (failOn > 0) {
			t.Fatalf("failOn=%d: Run err = %v", failOn, err)
		}

		if len(evs) == 0 {
			t.Fatalf("failOn=%d: no progress reported", failOn)
		}
		total := stage(t, evs[0], "folding windows").Total
		if first := stage(t, evs[0], "folding windows"); first.Done != 0 || total < 3 {
			t.Fatalf("failOn=%d: the first event must carry the whole plan before any call: %+v", failOn, first)
		}
		for i, e := range evs {
			if e.Where != "default" {
				t.Fatalf("failOn=%d: a lens with nothing to fold must say nothing, got %q", failOn, e.Where)
			}
			w := stage(t, e, "folding windows")
			if w.Total != total || w.Open || (i > 0 && w.Done < stage(t, evs[i-1], "folding windows").Done) {
				t.Fatalf("failOn=%d: the total moved or progress went backwards at event %d: %+v", failOn, i, w)
			}
		}
		last := stage(t, evs[len(evs)-1], "folding windows")
		if last.Done != calls {
			t.Errorf("failOn=%d: progress ended at %d/%d, but the runner received %d calls", failOn, last.Done, last.Total, calls)
		}
		if failOn == 0 && last.Done != total {
			t.Errorf("a clean fold must end at its total: %d/%d", last.Done, total)
		}
		if failOn > 0 && last.Done != failOn {
			t.Errorf("the failed window is finished work and must be counted: ended at %d, failed call %d", last.Done, failOn)
		}
	}
}

// The long-arc pass reports clustering (the band of k values, minutes of CPU each on a real lens)
// and then its verify calls, each with a total known before the first step.
func TestEmergentProgressCountsClusteringAndVerifies(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})
	seedArc(t, s, "b", 6, []float32{0, 1, 0})
	obs, _ := s.ReadObservations("math")
	band := len(kBand(len(obs)))

	run := func(maxVerify, failOn int) (evs []Progress, calls int) {
		r := emergentReviewer(s, func(context.Context, string, string, string) (string, error) {
			calls++
			if calls == failOn {
				return "", errors.New("simulated verify timeout")
			}
			return `[]`, nil // a rejection: marks the arc seen
		})
		r.MaxVerify = maxVerify
		r.Progress = func(p Progress) { evs = append(evs, p) }
		if err := r.RunFull(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		return evs, calls
	}
	split := func(evs []Progress) (clustering, verifying []Progress) {
		for _, e := range evs {
			if len(e.Stages) == 2 {
				clustering = append(clustering, e)
			} else {
				verifying = append(verifying, e)
			}
		}
		return
	}

	// Budget 1, first verify fails: a failure does not spend the budget, so exactly one more
	// call follows, and the total must grow to meet it rather than stall at 1/1 or end at 2/1.
	evs, calls := run(1, 1)
	cl, vf := split(evs)
	if len(cl) != band+1 {
		t.Fatalf("want one clustering event per k plus the start (%d), got %d", band+1, len(cl))
	}
	for i, e := range cl {
		c, v := stage(t, e, "clustering"), stage(t, e, "verifying arcs")
		if c.Done != i || c.Total != band || !v.Open || v.Total != 0 {
			t.Fatalf("clustering event %d: %+v / %+v", i, c, v)
		}
	}
	if len(vf) == 0 || calls != 2 {
		t.Fatalf("budget 1 with one failure: want 2 calls and verify progress, got %d calls, %d events", calls, len(vf))
	}
	if f := stage(t, vf[0], "verifying arcs"); f.Done != 0 || f.Total != 1 || !f.Open {
		t.Errorf("under a budget the first total is the budget, Open while unseen arcs remain: %+v", f)
	}
	if l := stage(t, vf[len(vf)-1], "verifying arcs"); l.Done != calls || l.Total != calls {
		t.Errorf("verify progress ended at %d/%d after %d calls", l.Done, l.Total, calls)
	}

	// The other arc is still unseen (the failed one was not marked): an unbudgeted run verifies
	// exactly what is left, with that total fixed from the first event.
	evs, calls = run(0, 0)
	_, vf = split(evs)
	if calls == 0 || len(vf) == 0 {
		t.Fatalf("want the remaining arcs verified, got %d calls", calls)
	}
	for _, e := range vf {
		if v := stage(t, e, "verifying arcs"); v.Total != calls || v.Open || e.Where != "math" {
			t.Fatalf("unbudgeted verify total must be exact and fixed: %+v after %d calls", e, calls)
		}
	}

	// Everything judged: no verify calls, so no verify progress.
	if evs, calls = run(0, 0); calls != 0 {
		t.Fatalf("every arc is seen; want 0 calls, got %d", calls)
	}
	if _, vf = split(evs); len(vf) != 0 {
		t.Errorf("nothing to verify must say nothing about verifying: %+v", vf)
	}
}
