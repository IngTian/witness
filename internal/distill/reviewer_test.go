package distill

import (
	"context"
	"errors"
	"strings"
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

// Reviewer.reviewLens must DISPATCH each lens to its per-lens runner (#75 slice 2), not
// just to the default Runner. Guards the runnerFor seam: fails if reviewLens is reverted to
// call r.Runner directly.
func TestReviewerRoutesReviewToPerLensRunner(t *testing.T) {
	s := newStore(t)
	stageObsForReview(t, s, "default")
	stageObsForReview(t, s, "cr")

	reviewedBy := map[string]string{} // review prompt → which runner ran it
	tag := func(runnerName string) MineFunc {
		return func(_ context.Context, _, prompt, _ string) (string, error) {
			reviewedBy[prompt] = runnerName
			return facetReply("thinking", "k", "v"), nil
		}
	}
	r := &Reviewer{
		Store: s,
		Lenses: []*lens.Lens{
			{Name: "default", Review: "REVIEW-default"},
			{Name: "cr", Review: "REVIEW-cr", Runner: "opencode"},
		},
		Config: store.Config{Runner: "claude"},
		Runner: tag("default"),
		RunnerFor: func(ln *lens.Lens) MineFunc {
			if ln != nil && ln.Runner == "opencode" {
				return tag("opencode")
			}
			return nil // fall back to Runner (default)
		},
	}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reviewedBy["REVIEW-default"] != "default" {
		t.Fatalf("default lens must review on the default runner, got %q", reviewedBy["REVIEW-default"])
	}
	if reviewedBy["REVIEW-cr"] != "opencode" {
		t.Fatalf("a lens with runner=opencode must review on the opencode runner, got %q", reviewedBy["REVIEW-cr"])
	}
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

// TestReviewFoldsOnlyObsSinceWatermark is the #16 structural fix: after a lens is
// reviewed, the NEXT review folds ONLY the observations recorded since that lens's
// fold watermark — not the whole corpus. This is what bounds the reviewer input by
// "what's new" instead of archive size (the 10-min / context cliff at scale).
func TestReviewFoldsOnlyObsSinceWatermark(t *testing.T) {
	s := newStore(t)

	// Two observations already in L1 before the first review.
	if err := s.AppendObservations([]store.Observation{
		{ID: "o1", TS: "2026-01-01T00:00:00Z", Session: "s1", Lens: "default", Dimension: "thinking", Observation: "first", Poignancy: 5},
		{ID: "o2", TS: "2026-01-02T00:00:00Z", Session: "s1", Lens: "default", Dimension: "thinking", Observation: "second", Poignancy: 5},
	}); err != nil {
		t.Fatal(err)
	}

	var lastFedIDs []string
	runner := func(_ context.Context, _, _, input string) (string, error) {
		lastFedIDs = nil
		for _, id := range []string{"o1", "o2", "o3"} {
			if strings.Contains(input, id) {
				lastFedIDs = append(lastFedIDs, id)
			}
		}
		return facetReply("thinking", "clarity", "improving"), nil
	}
	r := &Reviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{},
		Runner: runner,
	}

	// First review folds o1+o2 (watermark was 0).
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(lastFedIDs) != 2 {
		t.Fatalf("first review should fold both pre-existing obs, fed %v", lastFedIDs)
	}

	// A new observation arrives; the second review must fold ONLY it.
	if err := s.AppendObservations([]store.Observation{
		{ID: "o3", TS: "2026-01-03T00:00:00Z", Session: "s2", Lens: "default", Dimension: "thinking", Observation: "third", Poignancy: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(lastFedIDs) != 1 || lastFedIDs[0] != "o3" {
		t.Fatalf("second review must fold only the new obs (o3), fed %v", lastFedIDs)
	}
}

// bigObs seeds `n` observations for the default lens with padded text so the delta is
// large enough to force multiple windows under a small ReviewMaxChars.
func bigObs(t *testing.T, s *store.Store, n int) {
	t.Helper()
	pad := strings.Repeat("x", 300) // ~300+ chars/obs serialized → predictable window sizing
	obs := make([]store.Observation, n)
	for i := 0; i < n; i++ {
		obs[i] = store.Observation{
			ID: "o" + string(rune('A'+i)), TS: "2026-01-01T00:00:0" + string(rune('0'+i%10)) + "Z",
			Session: "s", Lens: "default", Dimension: "thinking",
			Observation: "obs " + string(rune('A'+i)) + " " + pad, Poignancy: 5,
		}
	}
	if err := s.AppendObservations(obs); err != nil {
		t.Fatalf("bigObs: %v", err)
	}
}

// TestReviewChunksLargeDeltaByReviewMaxChars (issue #123): a delta whose serialized size
// exceeds ReviewMaxChars must be folded in MORE THAN ONE reviewLens call, each bounded —
// not one giant call that would ride the 10-min timeout.
func TestReviewChunksLargeDeltaByReviewMaxChars(t *testing.T) {
	s := newStore(t)
	bigObs(t, s, 10) // ~3KB+ of serialized obs

	var callSizes []int
	runner := func(_ context.Context, _, _, input string) (string, error) {
		callSizes = append(callSizes, len(input))
		return facetReply("thinking", "clarity", "improving"), nil
	}
	r := &Reviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{ReviewMaxChars: 800}, // small → forces several windows
		Runner: runner,
	}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(callSizes) < 2 {
		t.Fatalf("a large delta must fold in >1 window; got %d call(s)", len(callSizes))
	}
	// Watermark advanced through the whole delta (all 10 obs folded).
	if got := s.UnreviewedDelta("default"); got != 0 {
		t.Fatalf("after windowed fold the whole delta must be reviewed; %d still pending", got)
	}
}

// TestReviewWindowFailureStrandsOnlyCurrentWindow: if window K's fold fails (timeout),
// the windows before it stay committed and the watermark sits at the last GOOD window —
// not rolled back, not advanced past the failure.
func TestReviewWindowFailureStrandsOnlyCurrentWindow(t *testing.T) {
	s := newStore(t)
	bigObs(t, s, 9)

	var calls int
	runner := func(_ context.Context, _, _, _ string) (string, error) {
		calls++
		if calls == 2 { // second window fails
			return "", errors.New("simulated review timeout")
		}
		return facetReply("thinking", "k"+string(rune('0'+calls)), "v"), nil
	}
	r := &Reviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{ReviewMaxChars: 800},
		Runner: runner,
	}
	err := r.Run(context.Background(), time.Now())
	if err == nil {
		t.Fatal("a window failure must surface as an error (not silent)")
	}
	// Watermark advanced past window 1 but NOT to the end (window 2 failed).
	wm := s.ReviewRowid("default")
	if wm == 0 {
		t.Fatal("window 1 committed but watermark stayed 0 — partial progress lost")
	}
	if s.UnreviewedDelta("default") == 0 {
		t.Fatal("window 2 failed so some delta must remain pending, got 0")
	}
	// A facet from window 1 is durably written.
	facets, _ := s.ReadFacets()
	if len(facets) == 0 {
		t.Fatal("window 1's facet must be durably written despite window 2's failure")
	}
}

// TestReviewLaterWindowSeesEarlierWindowStance: window 2 must fold against the facets
// window 1 just wrote (re-read per window), so a later window can reinforce/contradict
// an earlier one. We assert window 2's reviewLens input contains window 1's facet.
func TestReviewLaterWindowSeesEarlierWindowStance(t *testing.T) {
	s := newStore(t)
	bigObs(t, s, 8)

	var call int
	var window2SawPriorFacet bool
	runner := func(_ context.Context, _, _, input string) (string, error) {
		call++
		if call == 1 {
			return facetReply("thinking", "established_in_window_1", "v1"), nil
		}
		if strings.Contains(input, "established_in_window_1") {
			window2SawPriorFacet = true
		}
		return facetReply("thinking", "from_window_2", "v2"), nil
	}
	r := &Reviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{ReviewMaxChars: 800},
		Runner: runner,
	}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if call < 2 {
		t.Fatalf("expected >1 window, got %d", call)
	}
	if !window2SawPriorFacet {
		t.Fatal("a later window must fold against the stance written by an earlier window (re-read per window)")
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
