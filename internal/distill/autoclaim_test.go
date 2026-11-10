package distill

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// claimCall answers a claim batch with "CLAIM-<key>" for every facet it was shown, so a test can
// see which facets got claims and whether the review then sent them instead of the full values.
var claimKeyRE = regexp.MustCompile(`(?m)^--- dimension: (.+)\nkey: (.+)$`)

func claimReplyFor(input string) string {
	var out []claimedFacet
	for _, m := range claimKeyRE.FindAllStringSubmatch(input, -1) {
		out = append(out, claimedFacet{Dimension: m[1], Key: m[2], Claim: "CLAIM-" + m[2]})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func seedUnclaimedFacets(t *testing.T, s *store.Store, lensName string, n, valueChars int) {
	t.Helper()
	var fs []store.Facet
	for i := 0; i < n; i++ {
		key := "k" + string(rune('a'+i))
		fs = append(fs, store.Facet{Lens: lensName, Dimension: "thinking", Key: key,
			Versions: []store.FacetVersion{{Value: "FULLVALUE-" + key + "-" + strings.Repeat("x", valueChars), Confidence: 0.8}}})
	}
	if err := s.WriteFacets(fs); err != nil {
		t.Fatal(err)
	}
}

type recordedCall struct{ prompt, input string }

func autoClaimReviewer(s *store.Store, maxBatches int, run MineFunc) *Reviewer {
	return &Reviewer{
		Store:  s,
		Lenses: []*lens.Lens{{Name: "math", Review: "REVIEW-math"}},
		Runner: run,
		Claims: &Claimer{Store: s, Runner: run, MaxBatches: maxBatches},
	}
}

func claimsWritten(t *testing.T, s *store.Store, lensName string) int {
	t.Helper()
	fs, err := s.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range fs {
		if f.Lens == lensName && f.Current() != nil && f.Current().Claim != "" {
			n++
		}
	}
	return n
}

// The automatic claim pass runs BEFORE the fold, so the very review that would have sent every
// facet in full sends the short claims instead. After the fold would be too late: math's
// claim-less windows (1.14M chars each) timed out, and a pass gated on a successful fold could
// never unblock them.
func TestAutoClaimsRunBeforeTheFoldAndShrinkIt(t *testing.T) {
	s := newStore(t)
	seedUnclaimedFacets(t, s, "math", 3, 200)
	stageObsForReview(t, s, "math")

	var calls []recordedCall
	run := func(_ context.Context, _, prompt, input string) (string, error) {
		calls = append(calls, recordedCall{prompt, input})
		if prompt == claimPrompt {
			return claimReplyFor(input), nil
		}
		return "[]", nil
	}
	if err := autoClaimReviewer(s, AutoClaimBatches, run).Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(calls) < 2 || calls[0].prompt != claimPrompt {
		t.Fatalf("the claim pass must run before the fold; calls = %v", prompts(calls))
	}
	review := calls[len(calls)-1]
	if review.prompt != "REVIEW-math" {
		t.Fatalf("last call should be the review, got %q", review.prompt)
	}
	if !strings.Contains(review.input, "CLAIM-ka") || strings.Contains(review.input, "FULLVALUE-ka") {
		t.Errorf("the review must send the new claims instead of the full values:\n%s", review.input)
	}
	if got := claimsWritten(t, s, "math"); got != 3 {
		t.Errorf("claims written = %d, want 3", got)
	}
}

// Bounded per run: a lens with hundreds of claim-less facets catches up over several reviews
// instead of turning one review into an hours-long claim drain.
func TestAutoClaimsRespectTheBatchCap(t *testing.T) {
	s := newStore(t)
	seedUnclaimedFacets(t, s, "math", 5, claimBatchChars) // each facet fills a batch on its own
	stageObsForReview(t, s, "math")

	claimCalls := 0
	run := func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == claimPrompt {
			claimCalls++
			return claimReplyFor(input), nil
		}
		return "[]", nil
	}
	if err := autoClaimReviewer(s, 2, run).Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if claimCalls != 2 {
		t.Errorf("claim calls = %d, want the cap of 2", claimCalls)
	}
	if got := claimsWritten(t, s, "math"); got != 2 {
		t.Errorf("claims written = %d, want 2 (the rest wait for the next review)", got)
	}
}

// Best-effort: a failing claim batch leaves the facets on their full values (the pre-claim
// behaviour) and the review still folds.
func TestAutoClaimFailureDoesNotFailTheReview(t *testing.T) {
	s := newStore(t)
	seedUnclaimedFacets(t, s, "math", 2, 200)
	stageObsForReview(t, s, "math")

	reviewed := false
	run := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == claimPrompt {
			return "", errors.New("provider down")
		}
		reviewed = true
		return facetReply("thinking", "new", "v"), nil
	}
	if err := autoClaimReviewer(s, AutoClaimBatches, run).Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("a failed claim batch must not fail the review: %v", err)
	}
	if !reviewed {
		t.Fatal("the review call never ran")
	}
	fs, _ := s.ReadFacets()
	found := false
	for _, f := range fs {
		if f.Lens == "math" && f.Key == "new" {
			found = true
		}
	}
	if !found {
		t.Error("the review's facet was not written after the claim pass failed")
	}
}

// No delta, no claim calls: claims exist to shrink a fold, and a lens with nothing to fold
// spends nothing.
func TestAutoClaimsSkipALensWithNothingToFold(t *testing.T) {
	s := newStore(t)
	seedUnclaimedFacets(t, s, "math", 3, 200)

	calls := 0
	run := func(context.Context, string, string, string) (string, error) { calls++; return "[]", nil }
	if err := autoClaimReviewer(s, AutoClaimBatches, run).Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if calls != 0 {
		t.Errorf("model calls = %d, want 0 for a lens with no unreviewed observations", calls)
	}
}

func prompts(calls []recordedCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.prompt
		if c.prompt == claimPrompt {
			out[i] = "<claimPrompt>"
		}
	}
	return out
}
