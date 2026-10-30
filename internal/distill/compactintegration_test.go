package distill

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// Compaction is only worth anything if it STICKS. These cover the three paths that would
// silently undo it — the review prompt, the review's write-back, and the emergent pass — plus
// the resurrection rule for a facet that was retired and then earned its way back.

// If the review prompt shows an archived facet, the model reinforces it, applyFacet reopens it,
// and the next fold has un-compacted the profile. Nothing would log it.
func TestReviewPromptHidesArchivedFacets(t *testing.T) {
	archived := cf("thinking", "merged_away", "absorbed value", 0.7, 1)
	archived.ArchivedAt = "2026-10-02T00:00:00Z"
	archived.MergedInto = "thinking/survivor"
	retired := cf("habits", "dropped", "retired value", 0.3, 1)
	retired.ArchivedAt = "2026-10-02T00:00:00Z"

	view := slimFacets([]store.Facet{
		cf("thinking", "survivor", "live value", 0.8, 1), archived, retired,
	}, "default")

	blob, _ := json.Marshal(view)
	got := string(blob)
	if strings.Contains(got, "merged_away") || strings.Contains(got, "dropped") {
		t.Errorf("archived facets must not reach the review prompt: %s", got)
	}
	if !strings.Contains(got, "survivor") {
		t.Error("the active facet should still be shown")
	}
	if len(view) != 1 {
		t.Errorf("want exactly 1 facet in the prompt view, got %d", len(view))
	}
}

// The model cannot see an archived facet, so it will eventually re-assert one by name. Left
// alone the facet comes back active on its own history — the merge is undone by the next
// review that happens to mention the trait, and compaction can never hold.
func TestAssertionOnAMergedFacetRedirectsToTheSurvivor(t *testing.T) {
	absorbed := cf("thinking", "absorbed", "old narrow value", 0.6, 1)
	absorbed.ArchivedAt = "2026-10-02T00:00:00Z"
	absorbed.MergedInto = "thinking/survivor"
	survivor := cf("thinking", "survivor", "the combined value", 0.8, 1)

	byKey := indexFacets([]store.Facet{absorbed, survivor})
	r := &Reviewer{}
	ok := r.applyFacet(byKey, "default", reviewedFacet{
		Dimension: "thinking", Key: "absorbed", Value: "new evidence value",
		Confidence: 0.9, Contradicts: true,
	}, "2026-10-03T00:00:00Z")
	if !ok {
		t.Fatal("the assertion should be applied, not dropped")
	}

	if got := byKey["default|thinking|absorbed"]; got.Active() {
		t.Error("the absorbed facet must stay archived — a redirect, not a resurrection")
	}
	// The evidence landed on the survivor: contradicts_prior with a different value is a
	// change arc there.
	surv := byKey["default|thinking|survivor"]
	cur := surv.Current()
	if cur == nil || cur.Value != "new evidence value" {
		t.Fatalf("the survivor should carry the new evidence, got %+v", cur)
	}
	if len(surv.Versions) != 2 || surv.Versions[0].ValidTo == "" {
		t.Errorf("the survivor's prior version should be closed, got %+v", surv.Versions)
	}
}

// A RETIRED facet is the opposite case: the structural judgment was "this is not a trait", and
// new evidence is exactly what overturns it. Redirecting has nowhere to go, and dropping the
// assertion would make a retirement permanent and unfalsifiable.
func TestAssertionOnARetiredFacetResurrectsIt(t *testing.T) {
	retired := cf("habits", "came_back", "old value", 0.3, 1)
	retired.ArchivedAt = "2026-10-02T00:00:00Z" // no MergedInto: retired outright

	byKey := indexFacets([]store.Facet{retired})
	r := &Reviewer{}
	r.applyFacet(byKey, "default", reviewedFacet{
		Dimension: "habits", Key: "came_back", Value: "it is a pattern after all",
		Confidence: 0.8, Contradicts: true,
	}, "2026-10-03T00:00:00Z")

	got := byKey["default|habits|came_back"]
	if !got.Active() {
		t.Fatal("a retired facet with fresh evidence must come back")
	}
	if len(got.Versions) != 2 {
		t.Errorf("it should reopen on its OWN history, not as a blank facet: %+v", got.Versions)
	}
}

// A survivor can itself be merged by a later pass, so the redirect has to walk the chain.
func TestRedirectFollowsAMergeChain(t *testing.T) {
	a := cf("thinking", "a", "va", 0.6, 1)
	a.ArchivedAt, a.MergedInto = "t", "thinking/b"
	b := cf("thinking", "b", "vb", 0.6, 1)
	b.ArchivedAt, b.MergedInto = "t", "thinking/c"
	c := cf("thinking", "c", "vc", 0.8, 1)

	byKey := indexFacets([]store.Facet{a, b, c})
	got, id := resolveArchived(byKey, "default", byKey["default|thinking|a"], "default|thinking|a")
	if id != "default|thinking|c" || got.Key != "c" {
		t.Fatalf("the chain should resolve to the live survivor c, got %s", id)
	}
	for _, k := range []string{"a", "b"} {
		if byKey["default|thinking|"+k].Active() {
			t.Errorf("walking the chain must not reactivate intermediate facet %s", k)
		}
	}
}

// A cycle (only reachable via a hand-edited archive) must not spin forever, and the evidence
// must not be lost. Resurrecting is the safe landing.
func TestRedirectSurvivesACycle(t *testing.T) {
	a := cf("thinking", "a", "va", 0.6, 1)
	a.ArchivedAt, a.MergedInto = "t", "thinking/b"
	b := cf("thinking", "b", "vb", 0.6, 1)
	b.ArchivedAt, b.MergedInto = "t", "thinking/a"

	byKey := indexFacets([]store.Facet{a, b})
	got, _ := resolveArchived(byKey, "default", byKey["default|thinking|a"], "default|thinking|a")
	if got == nil {
		t.Fatal("a cycle must still resolve to something")
	}
	if !got.Active() {
		t.Error("with no live survivor the facet should be resurrected rather than dropped")
	}
}

// A survivor named by merged_into but absent from the table (hand-edited archive, partial
// restore) must not strand the assertion.
func TestRedirectToAMissingSurvivorResurrects(t *testing.T) {
	a := cf("thinking", "a", "va", 0.6, 1)
	a.ArchivedAt, a.MergedInto = "t", "thinking/nonexistent"
	byKey := indexFacets([]store.Facet{a})
	got, _ := resolveArchived(byKey, "default", byKey["default|thinking|a"], "default|thinking|a")
	if !got.Active() || got.MergedInto != "" {
		t.Errorf("a dangling survivor link should resurrect and clear itself, got %+v", got)
	}
}

// An archived facet's citations must not suppress a new candidate arc, or a retirement becomes
// permanent: the arc could never be re-proposed however much evidence accumulated.
func TestArchivedFacetsDoNotCoverCandidateArcs(t *testing.T) {
	members := []store.Observation{{ID: "o1", Lens: "default"}, {ID: "o2", Lens: "default"}}
	covering := store.Facet{
		Lens: "default", Dimension: "thinking", Key: "covers",
		Versions: []store.FacetVersion{{Value: "v", BecauseOf: []string{"o1", "o2"}}},
	}

	cov, key := bestFacetCoverage(members, []store.Facet{covering}, "default")
	if cov != 1.0 || key == "" {
		t.Fatalf("an ACTIVE facet citing both members should fully cover them, got %.2f %q", cov, key)
	}

	covering.ArchivedAt = "2026-10-02T00:00:00Z"
	cov, _ = bestFacetCoverage(members, []store.Facet{covering}, "default")
	if cov != 0 {
		t.Errorf("an archived facet must not cover an arc, got %.2f", cov)
	}
}
