package distill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Claims exist for one measured reason: on a real archive 2,039 facets carried 2,411,507 chars
// of value, which was 99.4% of the review call, and none of it was evidence — zero observation
// ids appear inline, they are all in because_of. It was analysis, re-sent every fold. These
// tests pin that the short form reaches the review call, that the full prose is never destroyed,
// and that a bad claim is refused rather than silently shrinking the profile's stance.

func claimer(st *fakeCompactStore, reply string) *Claimer {
	return &Claimer{
		Store:  st,
		Lenses: []*lens.Lens{{Name: "default"}},
		Config: store.Config{},
		Runner: func(context.Context, string, string, string) (string, error) { return reply, nil },
	}
}

// The payoff. If the review call still sees the full prose, the whole split is decoration.
func TestTheReviewCallSeesTheClaimNotTheProse(t *testing.T) {
	prose := strings.Repeat("long analysis with contrasts, advice and falsifiers. ", 40)
	f := cf("thinking", "a", prose, 0.8, 1)
	f.Versions[0].Claim = "He verifies state before acting on it."

	view := slimFacets([]store.Facet{f}, "default", 0)
	blob, _ := json.Marshal(view)
	got := string(blob)

	if !strings.Contains(got, "He verifies state before acting on it.") {
		t.Errorf("the claim must reach the review call:\n%s", got)
	}
	if strings.Contains(got, "long analysis with contrasts") {
		t.Error("the full prose must NOT be re-sent once a claim exists")
	}
	if len(got) > 300 {
		t.Errorf("the payload should collapse to roughly the claim's size, got %d chars", len(got))
	}
}

// An archive that has never been claimed must behave exactly as before — that is what makes
// this a progressive improvement rather than a migration everyone has to run.
func TestAFacetWithNoClaimStillSendsItsValue(t *testing.T) {
	f := cf("thinking", "a", "the full value", 0.8, 1) // no claim
	view := slimFacets([]store.Facet{f}, "default", 0)
	blob, _ := json.Marshal(view)
	if !strings.Contains(string(blob), "the full value") {
		t.Errorf("with no claim the value must still be sent:\n%s", blob)
	}
}

func TestWritingClaimsNeverTouchesTheValue(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "the original full prose, untouched", 0.8, 1),
	}}
	c := claimer(st, `[{"dimension":"thinking","key":"a","claim":"He does the thing."}]`)
	if n := c.Run(context.Background(), c.Lenses[0]); n != 1 {
		t.Fatalf("want 1 claim written, got %d", n)
	}
	got := writtenByRef(t, st, "thinking", "a")
	cur := got.Current()
	if cur.Claim != "He does the thing." {
		t.Errorf("claim not stored, got %q", cur.Claim)
	}
	if cur.Value != "the original full prose, untouched" {
		t.Errorf("the value must be preserved exactly, got %q", cur.Value)
	}
}

// A "claim" that runs to the length of the analysis is the model ignoring the instruction. It
// must be refused, not truncated: the review payload would be unchanged while LOOKING fixed,
// and a half-sentence claim is worse than none (none falls back to the complete value).
func TestAnOverlongClaimIsRefusedNotTruncated(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{cf("thinking", "a", "value", 0.8, 1)}}
	long := strings.Repeat("x", claimMaxChars+1)
	c := claimer(st, `[{"dimension":"thinking","key":"a","claim":"`+long+`"}]`)
	if n := c.Run(context.Background(), c.Lenses[0]); n != 0 {
		t.Fatalf("an overlong claim must be refused, got %d written", n)
	}
	if st.writes != 0 {
		t.Error("nothing should be persisted when every claim was refused")
	}
}

func TestAnEmptyClaimIsRefused(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{cf("thinking", "a", "value", 0.8, 1)}}
	c := claimer(st, `[{"dimension":"thinking","key":"a","claim":"   "}]`)
	if n := c.Run(context.Background(), c.Lenses[0]); n != 0 {
		t.Fatalf("an empty claim must be refused, got %d", n)
	}
}

// A claim for a facet the model was not shown must be ignored, not trusted — the same rule
// compaction's validator applies to groupings.
func TestAClaimForAnUnshownFacetIsIgnored(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{cf("thinking", "a", "value", 0.8, 1)}}
	c := claimer(st, `[{"dimension":"thinking","key":"nonexistent","claim":"invented"},
	                   {"dimension":"thinking","key":"a","claim":"real"}]`)
	if n := c.Run(context.Background(), c.Lenses[0]); n != 1 {
		t.Fatalf("only the real facet should be claimed, got %d", n)
	}
	if got := writtenByRef(t, st, "thinking", "a"); got.Current().Claim != "real" {
		t.Errorf("wrong claim applied: %q", got.Current().Claim)
	}
}

// Idempotence: a facet that already has a claim is not a candidate, so repeated runs cost
// nothing and never overwrite.
func TestAnExistingClaimIsNotRewritten(t *testing.T) {
	f := cf("thinking", "a", "value", 0.8, 1)
	f.Versions[0].Claim = "the original claim"
	st := &fakeCompactStore{facets: []store.Facet{f}}
	calls := 0
	c := claimer(st, "[]")
	c.Runner = func(context.Context, string, string, string) (string, error) {
		calls++
		return `[{"dimension":"thinking","key":"a","claim":"a replacement"}]`, nil
	}
	if n := c.Run(context.Background(), c.Lenses[0]); n != 0 {
		t.Errorf("an already-claimed facet is not a candidate, got %d written", n)
	}
	if calls != 0 {
		t.Errorf("no model call should be made when nothing needs a claim, got %d", calls)
	}
}

// Archived facets are history: spending model calls giving them claims is pure waste, since
// nothing ever sends them to a model again.
func TestArchivedFacetsAreNotClaimed(t *testing.T) {
	arch := cf("thinking", "gone", "value", 0.5, 1)
	arch.ArchivedAt = "2026-10-02T00:00:00Z"
	st := &fakeCompactStore{facets: []store.Facet{arch, cf("thinking", "live", "value", 0.8, 1)}}
	var sawInput string
	c := claimer(st, `[{"dimension":"thinking","key":"live","claim":"a claim"}]`)
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		sawInput = input
		return `[{"dimension":"thinking","key":"live","claim":"a claim"}]`, nil
	}
	c.Run(context.Background(), c.Lenses[0])
	if strings.Contains(sawInput, "gone") {
		t.Error("an archived facet must not be sent for claiming")
	}
}

// A model failure must leave the archive exactly as it was — every facet falling back to its
// full value, which is the pre-claim behaviour.
func TestAFailedClaimBatchChangesNothing(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{cf("thinking", "a", "value", 0.8, 1)}}
	c := claimer(st, "")
	c.Runner = func(context.Context, string, string, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	if n := c.Run(context.Background(), c.Lenses[0]); n != 0 {
		t.Errorf("a failed batch writes nothing, got %d", n)
	}
	if st.writes != 0 {
		t.Error("a failed claim pass must not write")
	}
}

// MaxBatches is what lets a caller drain progressively instead of spending an unbounded number
// of calls in one go. 0 means unbounded, which is right for an explicit command.
func TestMaxBatchesBoundsOneRun(t *testing.T) {
	var facets []store.Facet
	big := strings.Repeat("y", claimBatchChars/2) // two facets fill a batch
	for i := 0; i < 6; i++ {
		facets = append(facets, cf("thinking", string(rune('a'+i)), big, 0.8, 1))
	}
	st := &fakeCompactStore{facets: facets}
	calls := 0
	c := claimer(st, "[]")
	c.MaxBatches = 1
	c.Runner = func(context.Context, string, string, string) (string, error) {
		calls++
		return "[]", nil
	}
	c.Run(context.Background(), c.Lenses[0])
	if calls != 1 {
		t.Errorf("MaxBatches=1 should make exactly one call, got %d", calls)
	}
}

func TestSplitClaimParsesBothPartsAndDegradesSafely(t *testing.T) {
	claim, value := splitClaim("CLAIM: the short form\n---\nthe long detail\nover lines")
	if claim != "the short form" || value != "the long detail\nover lines" {
		t.Errorf("split wrong: claim=%q value=%q", claim, value)
	}
	// No delimiter: everything is the value, claim empty, so Gist falls back. Losing a whole
	// merge because the model forgot three dashes would be a far worse trade.
	claim, value = splitClaim("just prose with no delimiter")
	if claim != "" || value != "just prose with no delimiter" {
		t.Errorf("a reply with no delimiter should be value-only, got claim=%q value=%q", claim, value)
	}
	// A delimiter with nothing on one side is not a split.
	if c, v := splitClaim("---\nonly a body"); c != "" || !strings.Contains(v, "only a body") {
		t.Errorf("a headless split should be treated as prose, got claim=%q value=%q", c, v)
	}
}
