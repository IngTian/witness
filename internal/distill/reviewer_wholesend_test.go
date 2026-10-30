package distill

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Clipping is the OVERFLOW behaviour, not the normal path. The policy is to give the model
// everything it can take, and a healthy stance is cheap: measured on a real archive the mean
// current value is 1,182 chars, so 200 facets is ~236k chars (~59k tokens) — comfortable even
// on a 200k-context model. These tests pin that a stance which fits is never cut, that an
// oversized one still is, and that the gate measures the right thing.

// wholeSendReviewer captures the input the model actually received.
func wholeSendReviewer(got *string) *Reviewer {
	return &Reviewer{
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{ReviewFacetClip: 160, ReviewFacetFull: 1},
		Runner: func(_ context.Context, _, _, input string) (string, error) {
			*got = input
			return "[]", nil
		},
	}
}

// The regression this gate exists for, and it bites a FRESH archive hardest. Every facet in a
// new archive has exactly one version, so the "changed before" full-prose tier selects NONE of
// them — without the gate a 40-facet, 36k-char stance arrives with every single value truncated
// to 160 chars and not one shown whole, discarding real signal to save nothing.
func TestFreshArchiveValuesReachTheModelComplete(t *testing.T) {
	var got string
	r := wholeSendReviewer(&got)
	value := strings.Repeat("a", 900)
	var prior []store.Facet
	for i := 0; i < 40; i++ {
		prior = append(prior, facet("default", "habits", string(rune('a'+i)), 0.7, "2026-10-01T00:00:00Z", value))
	}

	if _, err := r.reviewLens(context.Background(), r.Lenses[0], nil, prior); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, clipMarker) {
		t.Error("a stance well under the whole-send budget must not be clipped at all")
	}
	if n := strings.Count(got, value); n != 40 {
		t.Errorf("want all 40 values in full, got %d", n)
	}
	// The header must not claim truncation that did not happen: the model would distrust
	// complete values and assert contradictions expecting a re-ask it never needs.
	if !strings.Contains(got, "Values are complete.") && !strings.Contains(got, "full values") {
		t.Error("header should state the values are complete")
	}
}

// Configured clip values still govern HOW an oversized stance is cut — the gate decides
// whether to cut, not what the budget is.
func TestStanceUnderBudgetIgnoresTheConfiguredClip(t *testing.T) {
	r := wholeSendReviewer(new(string))
	prior := []store.Facet{facet("default", "habits", "small", 0.7, "2026-10-01T00:00:00Z", strings.Repeat("x", 5000))}
	clip, full := r.facetBudget(prior, "default")
	if clip != 0 {
		t.Errorf("a 5k stance should be sent whole, got clip=%d", clip)
	}
	if full != 1 {
		t.Errorf("topFull should survive the gate, got %d", full)
	}
}

func TestOversizedStanceStillClips(t *testing.T) {
	r := wholeSendReviewer(new(string))
	prior := []store.Facet{facet("default", "habits", "huge", 0.7, "2026-10-01T00:00:00Z",
		strings.Repeat("x", reviewFacetWholeBudget+1))}
	clip, _ := r.facetBudget(prior, "default")
	if clip != 160 {
		t.Fatalf("a stance over the budget must clip at the configured value, got clip=%d", clip)
	}
}

// priorChars filtering by lens is load-bearing: prior carries EVERY lens's facets, so totalling
// all of them lets a large sibling lens force this one to clip a stance that fits fine. The
// archive this was measured on has exactly that shape — a 2.4M-char `default` beside a 920k-char
// `math`.
func TestLargeSiblingLensDoesNotForceClipping(t *testing.T) {
	r := wholeSendReviewer(new(string))
	prior := []store.Facet{
		facet("default", "habits", "small", 0.7, "2026-10-01T00:00:00Z", strings.Repeat("x", 1000)),
		facet("math", "habits", "huge", 0.7, "2026-10-01T00:00:00Z", strings.Repeat("y", reviewFacetWholeBudget+1)),
	}
	if clip, _ := r.facetBudget(prior, "default"); clip != 0 {
		t.Fatalf("a sibling lens's size must not clip this one, got clip=%d", clip)
	}
	// And the oversized lens itself must still clip.
	if clip, _ := r.facetBudget(prior, "math"); clip != 160 {
		t.Errorf("the oversized lens should still clip, got clip=%d", clip)
	}
}

// Closed versions are history, not current state, so they must not count toward the budget —
// otherwise a facet that has merely changed often pushes an archive into clipping it never
// needed.
func TestOnlyCurrentValuesCountTowardTheBudget(t *testing.T) {
	big := strings.Repeat("x", reviewFacetWholeBudget)
	prior := []store.Facet{facet("default", "habits", "churned", 0.7, "2026-10-01T00:00:00Z", big, big, "now small")}
	if n := priorChars(prior, "default"); n != len("now small") {
		t.Fatalf("priorChars should count only the open version, got %d", n)
	}
}
