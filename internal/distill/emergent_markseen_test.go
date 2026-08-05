package distill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// failingWriteFacets is a real store with WriteFacets forced to fail — the crash-window
// stand-in. It has to be an embedded *store.Store so signature state, observations, and
// facet reads all behave exactly as in production.
type failingWriteFacets struct {
	*store.Store
	err error
}

func (f failingWriteFacets) WriteFacets([]store.Facet) error { return f.err }

// An ACCEPTED arc whose L2 write fails must NOT be recorded as already-verified.
//
// markSeen used to run for both outcomes immediately after verify, but the arc facets are
// only written at the END of RunFull. So a failing WriteFacets — or a crash anywhere in the
// remaining lenses — left the cluster stamped verified-and-accepted with its facet never
// stored. seenUnchanged then skips it forever: the signature is a pure function of the
// member observation ids, and a re-mine reproduces identical ids, so nothing ever perturbs
// it back into the queue. The arc is lost silently and permanently.
func TestRunFullDoesNotMarkAnAcceptedArcSeenWhenTheL2WriteFails(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})

	verify := func(_ context.Context, _, _, _ string) (string, error) {
		return `[{"dimension":"thinking","key":"abstracts_to_structure","value":"reflexively abstracts","confidence":0.8,"contradicts_prior":false}]`, nil
	}

	// Pass 1: the write fails, so the run reports the failure and stamps nothing.
	boom := errors.New("disk full")
	failing := emergentReviewer(s, verify)
	failing.Store = failingWriteFacets{Store: s, err: boom}
	if err := failing.RunFull(context.Background(), time.Now()); err == nil {
		t.Fatal("RunFull must surface a failed L2 write")
	}
	if facets, _ := s.ReadFacets(); len(facets) != 0 {
		t.Fatalf("nothing should have been written: %d facets", len(facets))
	}

	// Pass 2: same store, working writes. The arc must be re-verified and land in L2.
	calls := 0
	counting := func(ctx context.Context, a, b, c string) (string, error) {
		calls++
		return verify(ctx, a, b, c)
	}
	if err := emergentReviewer(s, counting).RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if calls == 0 {
		t.Fatal("the arc was recorded as already-verified despite never being written — " +
			"it is now skipped on every future pass and lost permanently")
	}
	var found bool
	for _, f := range facets(t, s) {
		if f.Lens == "math" && f.Key == "abstracts_to_structure" {
			found = true
		}
	}
	if !found {
		t.Fatal("the retried arc did not reach L2")
	}
}

// A REJECTED arc is stamped immediately and stays stamped: there is nothing to persist for
// it, so it carries no ordering hazard, and re-running an expensive no-op verify on every
// future pass is exactly what the signature state exists to prevent.
func TestRunFullStillSuppressesAReVerifiedRejection(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})

	calls := 0
	reject := func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		return `[]`, nil // empty array = the judge rejected the arc
	}
	r := emergentReviewer(s, reject)
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := calls
	if first == 0 {
		t.Fatal("no candidate was verified at all — the fixture is not exercising the path")
	}
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if calls != first {
		t.Errorf("a rejected cluster was re-verified: %d calls then %d", first, calls)
	}
}

// An accepted arc that DID write is stamped, so it is not re-verified next pass.
func TestRunFullSuppressesAnAcceptedArcOnceItIsDurable(t *testing.T) {
	s := newStore(t)
	seedArc(t, s, "a", 6, []float32{1, 0, 0})

	calls := 0
	accept := func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		return `[{"dimension":"thinking","key":"k","value":"v","confidence":0.8}]`, nil
	}
	r := emergentReviewer(s, accept)
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := calls
	if err := r.RunFull(context.Background(), time.Now()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if calls != first {
		t.Errorf("a durable accepted arc was re-verified: %d calls then %d", first, calls)
	}
}

func facets(t *testing.T, s *store.Store) []store.Facet {
	t.Helper()
	f, err := s.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	return f
}
