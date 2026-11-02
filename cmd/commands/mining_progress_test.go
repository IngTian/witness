package commands

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// A second Drain call in one pass (a late arrival) continues the count instead of restarting it.
func TestContinuedDrainOffsetsAndNames(t *testing.T) {
	in := distill.Progress{Stages: []distill.Stage{{Name: "mining sessions", Done: 0, Total: 1, Open: true}}}
	got := continuedDrain(in, "carney-market", 312)
	if s := got.Stages[0]; got.Where != "carney-market" || s.Done != 312 || s.Total != 313 || !s.Open || s.Name != "mining sessions" {
		t.Fatalf("got %+v", got)
	}
	if in.Stages[0].Done != 0 || in.Stages[0].Total != 1 {
		t.Fatalf("the engine's snapshot must not be mutated: %+v", in)
	}
}

// tryRunner is a fake runtime for `lens try`: it counts extract and review calls and panics on
// the session whose transcript says "boom". Never spawns anything.
type tryRunner struct {
	extracts, reviews atomic.Int32
	obs               string // what an extract call returns
}

func (*tryRunner) Open(context.Context) error { return nil }
func (*tryRunner) Close() error               { return nil }
func (r *tryRunner) Run(_ context.Context, _, prompt, input string) (string, error) {
	if strings.Contains(prompt, "REVIEW-MARK") {
		r.reviews.Add(1)
		return "[]", nil
	}
	r.extracts.Add(1)
	if strings.Contains(input, "boom") {
		panic("runner blew up")
	}
	return r.obs, nil
}
func (*tryRunner) ValidateModels(context.Context, ...string) error { return nil }
func (*tryRunner) InvocationHint() string                          { return "fake" }
func (*tryRunner) ConcurrentRunSafe() bool                         { return true }

// lensTryProgress runs cmdLensTry over 3 sessions (one panics) and returns every snapshot.
func lensTryProgress(t *testing.T, r *tryRunner, review bool) (snaps []distill.Progress, finished int) {
	t.Helper()
	s := seedTryStore(t, store.RunnerClaude) // s1
	for _, id := range []string{"s2", "s3"} {
		text := "hello"
		if id == "s2" {
			text = "boom"
		}
		if err := s.AppendRaw(store.RawRecord{Session: id, Seq: 0, Role: "user", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	prevRunner, prevProgress := runnerForStore, tryProgress
	t.Cleanup(func() { runnerForStore, tryProgress = prevRunner, prevProgress })
	runnerForStore = func(store.RunnerResolver, store.Config) (platform.Runner, error) { return r, nil }
	var mu sync.Mutex
	tryProgress = func() (distill.ProgressFunc, func()) {
		return func(p distill.Progress) { mu.Lock(); snaps = append(snaps, p); mu.Unlock() },
			func() { finished++ }
	}
	out := captureStdout(t, func() {
		if err := cmdLensTry(writeLensDir(t, "cand", "EXTRACT-MARK", "REVIEW-MARK"), lensTryOpts{nSessions: 3, review: review, asJSON: true}); err != nil {
			t.Errorf("cmdLensTry: %v", err)
		}
	})
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("stdout must stay the JSON document alone: %q", out)
	}
	return snaps, finished
}

func previewStage(t *testing.T, p distill.Progress) distill.Stage {
	t.Helper()
	if len(p.Stages) == 0 || p.Stages[0].Name != "previewing sessions" {
		t.Fatalf("no previewing stage first: %+v", p)
	}
	return p.Stages[0]
}

// Every previewed session counts — the one whose runner panicked too — and the count is the
// extract calls the runner got. The review stage appears only once its one call is made.
func TestLensTryProgressCountsEverySession(t *testing.T) {
	r := &tryRunner{obs: `[{"dimension":"growth","observation":"o","evidence":"e","poignancy":3}]`}
	snaps, finished := lensTryProgress(t, r, true)

	if first := previewStage(t, snaps[0]); first.Done != 0 || first.Total != 3 || snaps[0].Where != "cand" {
		t.Fatalf("the first snapshot must carry the whole sample before any call: %+v", snaps[0])
	}
	var last distill.Stage
	reviewAt := -1
	for i, p := range snaps {
		s := previewStage(t, p)
		if s.Done < last.Done || s.Total != 3 {
			t.Fatalf("snapshot %d went backwards or its total moved: %+v -> %+v", i, last, s)
		}
		last = s
		if len(p.Stages) > 1 {
			if reviewAt >= 0 || p.Stages[1].Name != "reviewing" || p.Stages[1].Total != 1 || s.Done != 3 {
				t.Fatalf("the review stage must appear once, after every preview: %+v", p)
			}
			reviewAt = i
		}
	}
	if last.Done != 3 || r.extracts.Load() != 3 {
		t.Fatalf("ended at %d/3 after %d extract calls (one panicked), want 3/3", last.Done, r.extracts.Load())
	}
	if reviewAt < 0 || r.reviews.Load() != 1 {
		t.Fatalf("review stage shown=%v, review calls=%d; want both", reviewAt >= 0, r.reviews.Load())
	}
	if finished != 1 {
		t.Fatalf("the line must be finished exactly once before printing, got %d", finished)
	}
}

// Nothing mined means no review call, so no review stage: a total must not count a call that
// will never happen.
func TestLensTryProgressNoReviewStageWithoutACall(t *testing.T) {
	r := &tryRunner{obs: "[]"}
	snaps, _ := lensTryProgress(t, r, true)
	for _, p := range snaps {
		if len(p.Stages) > 1 {
			t.Fatalf("a review stage was shown but nothing was mined: %+v", p)
		}
	}
	if r.reviews.Load() != 0 {
		t.Fatalf("no observations, yet %d review calls", r.reviews.Load())
	}
}
