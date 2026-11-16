package distill

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Decay only means anything if it reaches the model and survives a confirmation. These pin the
// two ways it would silently do nothing.

// The payoff: the review call must see the DECAYED confidence. Sending the stored value is what
// made confidence a ratchet from the model's point of view — it would be told 0.9 for a facet
// that has not been confirmed in thousands of observations, and would have no way to know.
func TestTheReviewCallSeesDecayedConfidence(t *testing.T) {
	f := cf("thinking", "stale", "a value", 0.9, 1)
	f.Versions[0].ConfidenceSeq = 100
	f.Versions[0].Claim = "a short claim"

	// One half-life of observations later.
	at := int64(100 + store.FacetDecayHalfLifeObs)
	view := slimFacets([]store.Facet{f}, "default", store.ContiguousClock(at))
	blob, _ := json.Marshal(view)

	if strings.Contains(string(blob), "0.9") {
		t.Errorf("the stored confidence must not reach the model:\n%s", blob)
	}
	if !strings.Contains(string(blob), "0.45") {
		t.Errorf("want the decayed 0.45 in the prompt view:\n%s", blob)
	}
}

// Each review window is shown confidences decayed on its own lens's observations up to what
// that window has read: never another lens's, and never the ones later windows have yet to fold.
// The math facet at seq 1 is 1,500 math observations old when window 1 (the lone giant at seq
// 4501) folds, and 3,000 when window 2 (seqs 4502-6001) does.
func TestTheReviewDecaysEachWindowOnItsOwnClock(t *testing.T) {
	s := newStore(t)
	a := mathArchive()
	appendLensObs(t, s, "a", a[:len(a)-1]...) // seqs 1-4500
	if err := s.AppendObservations([]store.Observation{{ID: "big", Lens: "math", Observation: strings.Repeat("a", 500_000)}}); err != nil {
		t.Fatal(err) // seq 4501, a window to itself
	}
	appendLensObs(t, s, "b", slices.Repeat([]string{"math"}, 1500)...) // seqs 4502-6001
	f := openFacet("d", "k", "v", 0.9, 1)
	f.Lens = "math"
	writeFacets(t, s, f)
	if err := s.StampReviewLens("math", 4500); err != nil {
		t.Fatal(err)
	}
	var inputs []string
	r := &Reviewer{Store: s, Lenses: []*lens.Lens{{Name: "math", Review: "REVIEW"}}, Config: store.Config{ReviewMaxChars: 400_000},
		Runner: func(_ context.Context, _, _, input string) (string, error) {
			inputs = append(inputs, input)
			return "[]", nil
		}}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 {
		t.Fatalf("want two review windows, got %d", len(inputs))
	}
	if !strings.Contains(inputs[0], `"current_confidence": 0.64`) {
		t.Errorf("window 1 must read 0.64, decayed on math through its own seq; the prompt ends:\n%s", inputs[0][len(inputs[0])-600:])
	}
	if !strings.Contains(inputs[1], `"current_confidence": 0.45`) {
		t.Errorf("window 2 must read 0.45, decayed on math through its own seq; the prompt ends:\n%s", inputs[1][len(inputs[1])-600:])
	}
}

// The subtle one. applyFacet must step from what the model SAW, not from the stored value —
// otherwise every confirmation silently undoes the decay: the model is shown 0.45, agrees with
// 0.45, and the facet snaps back toward 0.9 because that is what the row still held.
func TestConfirmingADecayedFacetDoesNotUndoTheDecay(t *testing.T) {
	f := cf("thinking", "stale", "a value", 0.9, 1)
	f.Versions[0].ConfidenceSeq = 100
	byKey := indexFacets([]store.Facet{f})

	at := int64(100 + store.FacetDecayHalfLifeObs) // decayed to 0.45
	(&Reviewer{}).applyFacet(byKey, "default", reviewedFacet{
		Dimension: "thinking", Key: "stale", Value: "a value", Confidence: 0.45,
	}, applyNow, at, store.ContiguousClock(at))

	cur := byKey["default|thinking|stale"].Current()
	if cur.Confidence > 0.5 {
		t.Errorf("agreeing with the decayed value must not restore the old one, got %v", cur.Confidence)
	}
	// And the clock restarts, so it stops decaying from the old stamp.
	if cur.ConfidenceSeq != at {
		t.Errorf("a confirmation should restamp the clock to %d, got %d", at, cur.ConfidenceSeq)
	}
	if cur.DecayedConfidence(store.ContiguousClock(at)) != cur.Confidence {
		t.Error("a just-confirmed facet should read exactly as stored")
	}
}

// Fresh evidence still raises a decayed facet — decay is not a one-way street, it just has to
// be re-earned a step at a time.
func TestFreshEvidenceLiftsADecayedFacet(t *testing.T) {
	f := cf("thinking", "revived", "a value", 0.9, 1)
	f.Versions[0].ConfidenceSeq = 100
	byKey := indexFacets([]store.Facet{f})

	at := int64(100 + store.FacetDecayHalfLifeObs) // 0.45
	(&Reviewer{}).applyFacet(byKey, "default", reviewedFacet{
		Dimension: "thinking", Key: "revived", Value: "a value", Confidence: 0.95,
	}, applyNow, at, store.ContiguousClock(at))

	cur := byKey["default|thinking|revived"].Current()
	if cur.Confidence <= 0.45 {
		t.Errorf("strong fresh evidence should lift a decayed facet, got %v", cur.Confidence)
	}
	if cur.Confidence > 0.56 {
		t.Errorf("but by at most one step from the decayed value, got %v", cur.Confidence)
	}
}

// A brand-new facet is stamped with the current clock, or it would read as never-confirmed and
// never decay.
func TestANewFacetIsStampedWithTheClock(t *testing.T) {
	byKey := indexFacets(nil)
	(&Reviewer{}).applyFacet(byKey, "default", reviewedFacet{
		Dimension: "thinking", Key: "brand_new", Value: "v", Confidence: 0.6,
	}, applyNow, 4242, store.ContiguousClock(4242))
	cur := byKey["default|thinking|brand_new"].Current()
	if cur.ConfidenceSeq != 4242 {
		t.Errorf("a new facet must carry the clock, got %d", cur.ConfidenceSeq)
	}
}

// A change arc opens a new version, which is a fresh judgment and so gets a fresh clock.
func TestAChangeArcStampsTheClock(t *testing.T) {
	f := cf("thinking", "moved", "old value", 0.8, 1)
	f.Versions[0].ConfidenceSeq = 100
	byKey := indexFacets([]store.Facet{f})

	(&Reviewer{}).applyFacet(byKey, "default", reviewedFacet{
		Dimension: "thinking", Key: "moved", Value: "a genuinely new value",
		Confidence: 0.7, Contradicts: true,
	}, applyNow, 9000, store.ContiguousClock(9000))

	fc := byKey["default|thinking|moved"]
	if len(fc.Versions) != 2 {
		t.Fatalf("want a new version, got %d", len(fc.Versions))
	}
	if fc.Current().ConfidenceSeq != 9000 {
		t.Errorf("the new version needs the current clock, got %d", fc.Current().ConfidenceSeq)
	}
}
