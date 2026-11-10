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

// The long-arc pass reports clustering (ranking each observation's neighbours, minutes of CPU on a
// real lens) and then its verify calls, each with a total known before the first step.
func TestEmergentProgressCountsClusteringAndVerifies(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})
	seedArc(t, s, "b", 6, []float32{0, 1, 0})
	seedArc(t, s, "c", 6, []float32{0, 0, 1})
	obs, _ := s.ReadObservations("math")
	ranked := len(obs) // every seeded observation is embedded, so each one is ranked

	run := func(failOn int) (evs []Progress, calls int) {
		r := emergentReviewer(s, func(context.Context, string, string, string) (string, error) {
			calls++
			if calls == failOn {
				return "", errors.New("simulated verify timeout")
			}
			return `[]`, nil // a rejection: marks the arc seen
		})
		// A lens with no observations has nothing to cluster and must say nothing.
		r.Lenses = append(r.Lenses, &lens.Lens{Name: "quiet", Emerge: "EMERGE", Review: "REVIEW"})
		r.Progress = func(p Progress) { evs = append(evs, p) }
		if err := r.RunFull(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Where != "math" {
				t.Fatalf("every event must name the lens it is about, and only a lens with work: %+v", e)
			}
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

	// First pass, first verify fails: all three arcs are tried, the total is fixed at 3 from the
	// first event, and the failed call still counts as done.
	evs, calls := run(1)
	cl, vf := split(evs)
	if len(cl) != ranked+1 {
		t.Fatalf("want one clustering event per ranked observation plus the start (%d), got %d", ranked+1, len(cl))
	}
	for i, e := range cl {
		c, v := stage(t, e, "clustering"), stage(t, e, "verifying arcs")
		if c.Done != i || c.Total != ranked || !v.Open || v.Total != 0 {
			t.Fatalf("clustering event %d: %+v / %+v", i, c, v)
		}
	}
	if calls != 3 || len(vf) == 0 {
		t.Fatalf("want all 3 arcs verified on the first pass, got %d calls, %d events", calls, len(vf))
	}
	for _, e := range vf {
		if v := stage(t, e, "verifying arcs"); v.Total != 3 || v.Open {
			t.Fatalf("verify total must be exact and fixed from the first event: %+v", e)
		}
	}
	if l := stage(t, vf[len(vf)-1], "verifying arcs"); l.Done != 3 {
		t.Errorf("verify progress ended at %d/3; a failed call is still a call done", l.Done)
	}

	// The failed arc was not marked seen, so the next pass verifies exactly that one.
	if _, calls = run(0); calls != 1 {
		t.Fatalf("want the 1 failed arc re-verified, got %d calls", calls)
	}

	// Everything judged: no verify calls, so no verify progress.
	if evs, calls = run(0); calls != 0 {
		t.Fatalf("every arc is seen; want 0 calls, got %d", calls)
	}
	if _, vf = split(evs); len(vf) != 0 {
		t.Errorf("nothing to verify must say nothing about verifying: %+v", vf)
	}
}
