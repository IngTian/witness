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
