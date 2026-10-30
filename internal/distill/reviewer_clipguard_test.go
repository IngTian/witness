package distill

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Clipping INTRODUCES a risk that does not exist today. With every value sent in full the
// model cannot close a version it has not read; once values are clipped it can, and
// applyFacet's change branch (`rf.Contradicts && cur.Value != rf.Value`) would stamp ValidTo
// on good full prose and make a 160-char stub the current value. These tests pin the guard
// that prevents it. Note the asymmetry the guard relies on: the reinforcement branch touches
// only Confidence and BecauseOf, never Value, so a clipped value echoed as a REINFORCEMENT is
// harmless — only contradictions are dangerous.

func clipGuardReviewer(run MineFunc) *Reviewer {
	return &Reviewer{
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{ReviewFacetClip: 160, ReviewFacetFull: 1},
		Runner: run,
	}
}

func priorOne(value string, versions int) []store.Facet {
	vals := make([]string, versions)
	for i := range vals {
		vals[i] = value
	}
	return []store.Facet{facet("default", "habits", "stored", 0.7, "2026-10-01T00:00:00Z", vals...)}
}

// A stub echoed back as a value must never be stored. Unguarded, with contradicts_prior set,
// it replaces real prose with a truncated fragment and the history records a change that
// never happened.
func TestEchoedClipMarkerIsDropped(t *testing.T) {
	calls := 0
	r := clipGuardReviewer(func(context.Context, string, string, string) (string, error) {
		calls++
		return "[]", nil
	})
	prior := priorOne(strings.Repeat("a", 900), 1)
	reviewed := []reviewedFacet{
		{Dimension: "habits", Key: "stored", Value: "aaaa" + clipMarker, Contradicts: true},
		{Dimension: "habits", Key: "other", Value: "a real new value", Contradicts: false},
	}
	out := r.guardClippedVerdicts(context.Background(), r.Lenses[0], "{}", prior, reviewed, map[string]bool{})
	if len(out) != 1 || out[0].Key != "other" {
		t.Fatalf("the echoed stub should be dropped and the real assertion kept, got %+v", out)
	}
	if calls != 0 {
		t.Errorf("dropping an echo must not trigger a re-ask, got %d calls", calls)
	}
}

// The core guard: a contradiction against a facet shown only truncated is held back and
// re-asked with the real text, and the re-asked verdict is what gets applied.
func TestContradictionAgainstClippedFacetIsReAsked(t *testing.T) {
	full := strings.Repeat("b", 900) + "DISTINCTIVE_TAIL"
	var sawPrompt string
	r := clipGuardReviewer(func(_ context.Context, _, _, input string) (string, error) {
		sawPrompt = input
		return `[{"dimension":"habits","key":"stored","value":"an informed new value","contradicts_prior":true}]`, nil
	})
	prior := priorOne(full, 1)
	reviewed := []reviewedFacet{{Dimension: "habits", Key: "stored", Value: "guessed from the head", Contradicts: true}}

	out := r.guardClippedVerdicts(context.Background(), r.Lenses[0], "{}", prior, reviewed, map[string]bool{})

	if !strings.Contains(sawPrompt, "DISTINCTIVE_TAIL") {
		t.Error("the re-ask must carry the facet's FULL value — that is the whole point")
	}
	if strings.Contains(sawPrompt, clipMarker) {
		t.Error("the re-ask must not itself be clipped")
	}
	if len(out) != 1 || out[0].Value != "an informed new value" {
		t.Fatalf("the re-asked verdict should replace the blind one, got %+v", out)
	}
	if !out[0].Contradicts {
		t.Error("an informed contradiction should still contradict")
	}
}

// If the re-ask cannot run, degrade to a REINFORCEMENT rather than closing blind or dropping
// the evidence. applyFacet's default branch merges because_of and raises confidence while
// leaving the stored value intact — the arc is not recorded, but nothing is corrupted.
func TestReAskFailureDegradesToReinforcement(t *testing.T) {
	r := clipGuardReviewer(func(context.Context, string, string, string) (string, error) {
		return "", errors.New("runner down")
	})
	prior := priorOne(strings.Repeat("c", 900), 1)
	reviewed := []reviewedFacet{{Dimension: "habits", Key: "stored", Value: "blind guess", Contradicts: true}}

	out := r.guardClippedVerdicts(context.Background(), r.Lenses[0], "{}", prior, reviewed, map[string]bool{})

	if len(out) != 1 {
		t.Fatalf("the evidence should be kept, not dropped, got %d", len(out))
	}
	if out[0].Contradicts {
		t.Error("a contradiction that could not be verified must NOT stay a contradiction — it would close a version blind")
	}
}

// A facet that WAS shown in full needs no re-ask: the model read the real text, so its verdict
// is already informed. Re-asking would double every call for nothing.
func TestContradictionAgainstFullyShownFacetSkipsTheReAsk(t *testing.T) {
	calls := 0
	r := clipGuardReviewer(func(context.Context, string, string, string) (string, error) {
		calls++
		return "[]", nil
	})
	prior := priorOne(strings.Repeat("d", 900), 2) // 2 versions => in the full tier
	reviewed := []reviewedFacet{{Dimension: "habits", Key: "stored", Value: "informed", Contradicts: true}}

	out := r.guardClippedVerdicts(context.Background(), r.Lenses[0], "{}", prior,
		reviewed, map[string]bool{"habits|stored": true})

	if calls != 0 {
		t.Errorf("no re-ask should happen for a fully-shown facet, got %d calls", calls)
	}
	if len(out) != 1 || !out[0].Contradicts {
		t.Fatalf("the verdict should pass through untouched, got %+v", out)
	}
}

// A contradiction against a facet that does not exist yet cannot close anything — there is no
// stored version to close — so it must pass straight through rather than burning a re-ask.
func TestContradictionOnABrandNewFacetSkipsTheReAsk(t *testing.T) {
	calls := 0
	r := clipGuardReviewer(func(context.Context, string, string, string) (string, error) {
		calls++
		return "[]", nil
	})
	reviewed := []reviewedFacet{{Dimension: "habits", Key: "never_seen", Value: "brand new", Contradicts: true}}

	out := r.guardClippedVerdicts(context.Background(), r.Lenses[0], "{}", nil, reviewed, map[string]bool{})

	if calls != 0 {
		t.Errorf("a new facet needs no re-ask, got %d calls", calls)
	}
	if len(out) != 1 || !out[0].Contradicts {
		t.Fatalf("a new facet's assertion should pass through, got %+v", out)
	}
}

// The re-ask reply is filtered to what was actually asked about, so a model that volunteers
// extra facets in the second pass cannot smuggle in verdicts that never went through the
// first-pass window logic.
func TestReAskReplyIsFilteredToTheDeferredSet(t *testing.T) {
	r := clipGuardReviewer(func(context.Context, string, string, string) (string, error) {
		return `[{"dimension":"habits","key":"stored","value":"ok","contradicts_prior":true},
		          {"dimension":"habits","key":"smuggled","value":"not asked about","contradicts_prior":true}]`, nil
	})
	prior := priorOne(strings.Repeat("e", 900), 1)
	reviewed := []reviewedFacet{{Dimension: "habits", Key: "stored", Value: "blind", Contradicts: true}}

	out := r.guardClippedVerdicts(context.Background(), r.Lenses[0], "{}", prior, reviewed, map[string]bool{})

	if len(out) != 1 || out[0].Key != "stored" {
		t.Fatalf("only the deferred facet should come back, got %+v", out)
	}
}

// facetBudget must never let a zero-valued Config mean "unbounded" for a stance that does NOT
// fit — unbounded is exactly the payload that refused a real 2,039-facet lens for 24 days. (A
// stance that fits is sent whole by design; that is the whole-send gate, tested separately.)
func TestZeroConfigFallsBackToMeasuredDefaultsNotUnbounded(t *testing.T) {
	r := &Reviewer{Config: store.Config{}}
	oversized := []store.Facet{facet("default", "habits", "huge", 0.7, "2026-10-01T00:00:00Z",
		strings.Repeat("x", reviewFacetWholeBudget+1))}
	clip, full := r.facetBudget(oversized, "default")
	if clip != store.DefaultReviewFacetClip || full != store.DefaultReviewFacetFull {
		t.Fatalf("zero Config gave clip=%d full=%d, want the measured defaults", clip, full)
	}
	if clip <= 0 {
		t.Fatal("a zero Config must not resolve to unbounded")
	}
}
