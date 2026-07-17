package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	_ "github.com/IngTian/witness/internal/platform/claude" // register default platform for ForSession
	opencodeplatform "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

// previewLens is a minimal candidate lens for the preview tests.
func previewLens() *lens.Lens {
	return &lens.Lens{Name: "cand", Default: false, Extract: "extract-cand", Review: "r", Dimensions: []string{"thinking"}}
}

// obsReply is a helper: a JSON array of one observation echoing the input.
func obsReply(input string) string {
	arr := []minedObs{{Dimension: "thinking", Observation: "obs:" + input, Evidence: "e", Poignancy: 5}}
	b, _ := json.Marshal(arr)
	return string(b)
}

// TestPreviewMineIsReadOnly: a preview must write NOTHING — no observations, no
// watermark advance, no staged changes — even for a busy session. This is the whole
// safety contract of `lens try`.
func TestPreviewMineIsReadOnly(t *testing.T) {
	s := newStore(t)
	session := "sess-ro"
	capture(t, s, session, "user", "hello")
	capture(t, s, session, "assistant", "hi there")

	obsBefore := countObs(t, s)
	wmBefore := s.DistilledCount(session, "cand")

	miner := func(_ context.Context, _, _, input string) (string, error) { return obsReply(input), nil }
	obs, chunks, drifted, err := PreviewMine(context.Background(), miner, store.Config{}, s, session, previewLens())
	if err != nil {
		t.Fatalf("PreviewMine: %v", err)
	}
	if len(obs) == 0 {
		t.Fatalf("expected the preview to return observations")
	}
	if chunks < 1 {
		t.Fatalf("expected chunkCount >= 1, got %d", chunks)
	}
	if drifted {
		t.Fatalf("a normal reply must not be flagged as drift")
	}

	if got := countObs(t, s); got != obsBefore {
		t.Fatalf("preview WROTE observations: before=%d after=%d (must write nothing)", obsBefore, got)
	}
	if got := s.DistilledCount(session, "cand"); got != wmBefore {
		t.Fatalf("preview advanced the watermark: before=%d after=%d (must not)", wmBefore, got)
	}
}

// TestPreviewMineMinesWholeSessionIgnoringWatermark: even when a lens has ALREADY
// mined the whole session (watermark == raw count), a preview must re-mine the ENTIRE
// session, not the (empty) un-mined delta. Reusing MineSession would preview nothing
// here — the core reason PreviewMine exists.
func TestPreviewMineMinesWholeSessionIgnoringWatermark(t *testing.T) {
	s := newStore(t)
	session := "sess-caught-up"
	capture(t, s, session, "user", "u1")
	capture(t, s, session, "assistant", "a1")
	capture(t, s, session, "user", "u2")

	total := s.RawCount(session)
	// Simulate the lens being fully caught up: watermark == total.
	if err := s.MarkDistilled(session, "cand", total); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	if s.DistilledCount(session, "cand") != total {
		t.Fatalf("precondition: watermark should equal total (%d)", total)
	}

	// The miner echoes its transcript into the observation text, and it captures the
	// exact transcript(s) it was handed — so we can assert WHAT was mined, not just that
	// SOMETHING was.
	var minedInputs []string
	miner := func(_ context.Context, _, _, input string) (string, error) {
		minedInputs = append(minedInputs, input)
		return obsReply(input), nil
	}
	obs, _, _, err := PreviewMine(context.Background(), miner, store.Config{}, s, session, previewLens())
	if err != nil {
		t.Fatalf("PreviewMine: %v", err)
	}
	if len(minedInputs) == 0 {
		t.Fatalf("preview did not mine an already-caught-up session (it must ignore the watermark)")
	}
	if len(obs) == 0 {
		t.Fatalf("preview of a caught-up session returned no observations (previewed the empty delta)")
	}
	// The decisive assertion: the transcript handed to the miner must contain the LAST
	// turn ("u2"). If PreviewMine had regressed to mining raw[done:] (done==total), the
	// delta would be empty and the rendered transcript would NOT contain "u2" — so this
	// fails under the exact regression the test guards, unlike a bare mined>0 check
	// (an empty delta still renders one empty input on Claude, yielding a spurious obs).
	joined := strings.Join(minedInputs, "\n")
	if !strings.Contains(joined, "u2") {
		t.Fatalf("preview did not mine the WHOLE session — last turn %q missing from mined transcript:\n%s", "u2", joined)
	}
}

// TestPreviewMineDriftRule: a reply with NO JSON array (prose drift) and ZERO
// observations flags drifted=true; a reply with an explicit empty array is a legit
// quiet session (drifted=false, no obs).
func TestPreviewMineDriftRule(t *testing.T) {
	s := newStore(t)
	session := "sess-drift"
	capture(t, s, session, "user", "u1")

	// Prose reply → no array → drift.
	prose := func(_ context.Context, _, _, _ string) (string, error) {
		return "Sure! Here's a summary of the session instead of JSON.", nil
	}
	obs, _, drifted, err := PreviewMine(context.Background(), prose, store.Config{}, s, session, previewLens())
	if err != nil {
		t.Fatalf("PreviewMine(prose): %v", err)
	}
	if !drifted {
		t.Fatalf("a no-array prose reply with zero obs must be flagged as drift")
	}
	if len(obs) != 0 {
		t.Fatalf("drift must yield zero observations, got %d", len(obs))
	}

	// Explicit empty array → legit quiet, NOT drift.
	empty := func(_ context.Context, _, _, _ string) (string, error) { return "[]", nil }
	obs, _, drifted, err = PreviewMine(context.Background(), empty, store.Config{}, s, session, previewLens())
	if err != nil {
		t.Fatalf("PreviewMine(empty): %v", err)
	}
	if drifted {
		t.Fatalf("an explicit empty array is a legit quiet session, not drift")
	}
	if len(obs) != 0 {
		t.Fatalf("empty array must yield zero obs, got %d", len(obs))
	}
}

// TestPreviewMineMultiChunkAggregates: a session that renders to MULTIPLE chunks (an
// OpenCode session under a small budget) must aggregate observations across ALL chunks
// and report chunkCount>1. This exercises the multi-input loop that a single-chunk
// Claude session never reaches.
func TestPreviewMineMultiChunkAggregates(t *testing.T) {
	defer opencodeplatform.SetChunkMaxCharsForTest(18)() // force several chunks
	s := newStore(t)
	session := "opencode:multi"
	capture(t, s, session, "user", "alpha alpha alpha")
	capture(t, s, session, "assistant", "beta beta beta")
	capture(t, s, session, "user", "gamma gamma gamma")

	miner := func(_ context.Context, _, _, input string) (string, error) { return obsReply(input), nil }
	obs, chunks, drifted, err := PreviewMine(context.Background(), miner, store.Config{}, s, session, previewLens())
	if err != nil {
		t.Fatalf("PreviewMine: %v", err)
	}
	if chunks <= 1 {
		t.Fatalf("expected the session to render to >1 chunk under an 18-char budget, got %d", chunks)
	}
	if len(obs) != chunks {
		t.Fatalf("expected one obs per chunk (aggregated across all chunks): chunks=%d obs=%d", chunks, len(obs))
	}
	if drifted {
		t.Fatalf("all chunks produced obs, so the lens must not be flagged as drifted")
	}
}

// TestPreviewMineMultiChunkDriftRule: with several chunks where SOME drift but at least
// one produces observations, the lens must NOT be flagged as drifted (the all-inputs
// rule: drift only when zero obs across ALL chunks). This is the exact case a
// single-chunk test can't reach.
func TestPreviewMineMultiChunkDriftRule(t *testing.T) {
	defer opencodeplatform.SetChunkMaxCharsForTest(18)()
	s := newStore(t)
	session := "opencode:mixed"
	capture(t, s, session, "user", "alpha alpha alpha")
	capture(t, s, session, "assistant", "beta beta beta")
	capture(t, s, session, "user", "gamma gamma gamma")

	// First chunk drifts (prose, no array); the rest extract fine.
	var call int
	miner := func(_ context.Context, _, _, input string) (string, error) {
		call++
		if call == 1 {
			return "Here is some prose instead of JSON.", nil
		}
		return obsReply(input), nil
	}
	obs, chunks, drifted, err := PreviewMine(context.Background(), miner, store.Config{}, s, session, previewLens())
	if err != nil {
		t.Fatalf("PreviewMine: %v", err)
	}
	if chunks <= 1 {
		t.Fatalf("expected >1 chunk, got %d", chunks)
	}
	if drifted {
		t.Fatalf("a session where one chunk drifts but others produce obs must NOT be flagged drifted")
	}
	if len(obs) == 0 {
		t.Fatalf("expected observations from the non-drifting chunks")
	}
}

// TestPreviewMineTransportErrorSurfaces: a transport error (not a parse miss) is
// returned as-is — it is a real failure, distinct from drift.
func TestPreviewMineTransportErrorSurfaces(t *testing.T) {
	s := newStore(t)
	session := "sess-transport"
	capture(t, s, session, "user", "u1")

	boom := func(_ context.Context, _, _, _ string) (string, error) {
		return "", fmt.Errorf("rate limited")
	}
	_, _, drifted, err := PreviewMine(context.Background(), boom, store.Config{}, s, session, previewLens())
	if err == nil {
		t.Fatalf("a transport error must be surfaced, not swallowed")
	}
	if drifted {
		t.Fatalf("a transport error must NOT be reported as drift")
	}
}

// TestPreviewReviewSynthesizesWithoutWriting: PreviewReview runs the REVIEW prompt over
// a set of observations and returns the facets it would assert — writing NO facets.
func TestPreviewReviewSynthesizesWithoutWriting(t *testing.T) {
	s := newStore(t)
	// Seed a couple of real facet rows so we can prove PreviewReview doesn't touch them.
	facetsBefore := countFacets(t, s)

	obs := []store.Observation{
		{ID: "obs_1", Lens: "cand", Dimension: "thinking", Observation: "reasons by analogy", Poignancy: 5},
		{ID: "obs_2", Lens: "cand", Dimension: "thinking", Observation: "reasons by analogy again", Poignancy: 4},
	}
	// A fake reviewer that returns one facet citing the fed observations.
	reviewer := func(_ context.Context, _, _, _ string) (string, error) {
		arr := []reviewedFacet{{Dimension: "thinking", Key: "analogical", Value: "leans analogical", Confidence: 0.7, BecauseOf: []string{"obs_1", "obs_2"}}}
		b, _ := json.Marshal(arr)
		return string(b), nil
	}
	facets, err := PreviewReview(context.Background(), reviewer, store.Config{}, previewLens(), obs, nil)
	if err != nil {
		t.Fatalf("PreviewReview: %v", err)
	}
	if len(facets) != 1 || facets[0].Key != "analogical" || len(facets[0].BecauseOf) != 2 {
		t.Fatalf("expected one facet citing 2 obs, got %+v", facets)
	}
	if got := countFacets(t, s); got != facetsBefore {
		t.Fatalf("PreviewReview WROTE facets: before=%d after=%d (must write nothing)", facetsBefore, got)
	}
}

// TestPreviewReviewDriftSurfaces: a REVIEW reply with no JSON array surfaces the
// ErrNoJSONArray sentinel (prose drift on the review model), so a caller can classify
// it as drift (and give actionable guidance) rather than a silent empty or a generic
// parse error.
func TestPreviewReviewDriftSurfaces(t *testing.T) {
	prose := func(_ context.Context, _, _, _ string) (string, error) {
		return "The person seems thoughtful. (no JSON here)", nil
	}
	obs := []store.Observation{{ID: "o", Lens: "cand", Dimension: "thinking", Observation: "x", Poignancy: 3}}
	_, err := PreviewReview(context.Background(), prose, store.Config{}, previewLens(), obs, nil)
	if err == nil {
		t.Fatalf("a no-array REVIEW reply should surface an error, not a silent empty facet set")
	}
	if !errors.Is(err, ErrNoJSONArray) {
		t.Fatalf("a no-array reply must surface as ErrNoJSONArray (so callers can classify drift), got: %v", err)
	}
}

func countFacets(t *testing.T, s *store.Store) int {
	t.Helper()
	f, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets: %v", err)
	}
	return len(f)
}

// countObs returns the total observation rows — the read-only assertion's ground truth.
// ReadObservations("") reads across all lenses (the same all-lens read existing drain
// tests use).
func countObs(t *testing.T, s *store.Store) int {
	t.Helper()
	obs, err := s.ReadObservations("")
	if err != nil {
		t.Fatalf("ReadObservations: %v", err)
	}
	return len(obs)
}
