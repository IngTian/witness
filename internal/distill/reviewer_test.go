package distill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// stageObsForReview drops one observation into L1 for a lens so the reviewer has
// something to review (an empty obs set is skipped, never counted as a failure).
func stageObsForReview(t *testing.T, s *store.Store, lensName string) {
	t.Helper()
	if err := s.AppendObservations([]store.Observation{{
		ID:          obsID("sess", lensName, "obs-"+lensName),
		TS:          time.Now().UTC().Format(time.RFC3339),
		Session:     "sess",
		Lens:        lensName,
		Dimension:   "thinking",
		Observation: "did a thing",
		Poignancy:   5,
	}}); err != nil {
		t.Fatalf("AppendObservations(%s): %v", lensName, err)
	}
}

// A single facet reply the reviewer can parse+apply, tagged so we can assert which
// lens produced the written facet.
func facetReply(dimension, key, value string) string {
	return `[{"dimension":"` + dimension + `","key":"` + key +
		`","value":"` + value + `","confidence":0.9,"because_of":["x"],"contradicts_prior":false}]`
}

// #16 C1: a lens whose review CALL fails must NOT advance the review watermark and
// must surface as an error, even though other lenses reviewed cleanly. The old code
// `continue`d past the error then stamped + returned nil — silently reporting
// "review complete" with the failed lens unreviewed.
func TestReviewerFailedLensDoesNotStamp(t *testing.T) {
	s := newStore(t)
	stageObsForReview(t, s, "default")
	stageObsForReview(t, s, "codereview")

	// default reviews fine; codereview's runner errors (a timeout / model failure).
	runner := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "REVIEW-codereview" {
			return "", errors.New("simulated review timeout")
		}
		return facetReply("thinking", "clarity", "improving"), nil
	}
	r := &Reviewer{
		Store: s,
		Lenses: []*lens.Lens{
			{Name: "default", Review: "REVIEW-default"},
			{Name: "codereview", Review: "REVIEW-codereview"},
		},
		Config: store.Config{},
		Runner: runner,
	}

	err := r.Run(context.Background(), time.Now())
	if err == nil {
		t.Fatal("expected an error when a lens review fails; got nil (silent success — the C1 bug)")
	}

	// The review must NOT be stamped: a fresh review is still due (never ran).
	if got := s.MetaString("review_ts"); got != "" {
		t.Fatalf("review_ts should be empty (review not stamped after a failure); got %q", got)
	}

	// The lens that DID succeed must still have its facet written — no data loss.
	facets, ferr := s.ReadFacets()
	if ferr != nil {
		t.Fatalf("ReadFacets: %v", ferr)
	}
	var sawDefault, sawCodereview bool
	for _, f := range facets {
		switch f.Lens {
		case "default":
			sawDefault = true
		case "codereview":
			sawCodereview = true
		}
	}
	if !sawDefault {
		t.Error("the successfully-reviewed lens (default) should have a facet written")
	}
	if sawCodereview {
		t.Error("the failed lens (codereview) should not have produced a facet")
	}
}

// The happy path still stamps the review and returns nil.
func TestReviewerAllLensesSucceedStamps(t *testing.T) {
	s := newStore(t)
	stageObsForReview(t, s, "default")

	runner := func(_ context.Context, _, _, _ string) (string, error) {
		return facetReply("thinking", "clarity", "improving"), nil
	}
	r := &Reviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW-default"}},
		Config: store.Config{},
		Runner: runner,
	}

	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("Run: unexpected error on all-success review: %v", err)
	}
	if got := s.MetaString("review_ts"); got == "" {
		t.Fatal("review_ts should be stamped after a fully-successful review")
	}
}
