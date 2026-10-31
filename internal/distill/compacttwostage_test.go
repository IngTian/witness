package distill

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// The two-stage split and the two guards that make it trustworthy.
//
// Why it is two stages at all is empirical: one call asked to group AND write every value
// emitted ~40,000 chars and was killed at the runner's 10-minute wall on a real 105-facet
// dimension, for three different prompts. Grouping alone returned 8KB in 3m17s. These tests pin
// the structure that came out of that, and the two ways stage 2 could quietly do damage.

// Stage 1 must never be asked for values, and stage 2 must be one call per group. If these drift
// back together the 10-minute wall returns and deep merges silently stop happening.
func TestGroupingAndWritingAreSeparateCalls(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1),
		cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1),
		cf("thinking", "d", "vd", 0.7, 1),
		cf("thinking", "e", "ve", 0.7, 1),
	}}
	var group, write int
	c := compactor(st, "")
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			write++
			// Stage 2 must receive the full text of its members, not just names.
			if !strings.Contains(input, "FACETS TO COMBINE") {
				t.Error("stage 2 must be given the facets in full")
			}
			return mergedValue, nil
		}
		group++
		// Stage 1 must be told not to write values — a long reply here is what hit the wall.
		return `[{"action":"merge","dimension":"thinking","key":"g1","absorb":["a","b"],"why":"x"},
		         {"action":"merge","dimension":"thinking","key":"g2","absorb":["c","d"],"why":"y"}]`, nil
	}

	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if group != 1 {
		t.Errorf("one grouping call per dimension, got %d", group)
	}
	if write != 2 {
		t.Errorf("one write call per merge group, got %d", write)
	}
	if len(plans) != 1 || len(plans[0].Actions) != 2 {
		t.Fatalf("want 2 actions, got %+v", plans)
	}
	for _, a := range plans[0].Actions {
		if a.Value != mergedValue {
			t.Errorf("%s should carry the stage-2 value, got %q", a.Key, a.Value)
		}
	}
}

// The gap the maintainer caught: a stage-2 call that sees only its own nine facets writes blind
// to what the dimension's other survivors cover, so it bleeds into their territory and
// re-creates the overlap compaction exists to remove — one level coarser.
func TestStageTwoIsToldWhatItsSiblingsCover(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1), cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1), cf("thinking", "d", "vd", 0.7, 1),
	}}
	var sawSiblings []string
	c := compactor(st, "")
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			sawSiblings = append(sawSiblings, input)
			return mergedValue, nil
		}
		return `[{"action":"merge","dimension":"thinking","key":"first_trait","absorb":["a","b"],"why":"shared mechanism one"},
		         {"action":"merge","dimension":"thinking","key":"second_trait","absorb":["c","d"],"why":"shared mechanism two"}]`, nil
	}
	if _, err := c.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sawSiblings) != 2 {
		t.Fatalf("want 2 write calls, got %d", len(sawSiblings))
	}
	// Each call must name the OTHER trait and its reason, and must not list itself as a sibling.
	for _, in := range sawSiblings {
		self := "first_trait"
		other, otherWhy := "second_trait", "shared mechanism two"
		if strings.Contains(in, "THE TRAIT YOU ARE WRITING: second_trait") {
			self = "second_trait"
			other, otherWhy = "first_trait", "shared mechanism one"
		}
		if !strings.Contains(in, "OTHER TRAITS IN THIS DIMENSION") {
			t.Errorf("%s was written with no sibling boundaries at all", self)
		}
		if !strings.Contains(in, other) || !strings.Contains(in, otherWhy) {
			t.Errorf("%s should be told that %s covers %q", self, other, otherWhy)
		}
	}
}

// The central risk of the whole feature: a merge that READS well while dropping nine facets'
// worth of specifics turns "90%% fewer facets" into data loss dressed as a win. A value shorter
// than the longest thing it absorbs is the cheap, prose-blind tell.
func TestAMergedValueShorterThanItsLargestInputIsRefused(t *testing.T) {
	long := strings.Repeat("x", 2000)
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "big", long, 0.7, 1),
		cf("thinking", "small1", "s1", 0.7, 1),
		cf("thinking", "small2", "s2", 0.7, 1),
	}}
	c := compactor(st, "")
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			return "a tidy little summary", nil // 21 chars replacing a 2,000-char facet
		}
		return mergeReply("big", []string{"small1", "small2"}), nil
	}
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) == 0 {
		t.Fatal("the refusal should still produce a plan carrying the tally")
	}
	if len(plans[0].Actions) != 0 {
		t.Errorf("a lossy merge must not be applied, got %+v", plans[0].Actions)
	}
	if plans[0].Skipped["merged_value_shorter_than_its_largest_input"] != 1 {
		t.Errorf("the refusal should be tallied, got %v", plans[0].Skipped)
	}
}

// The empty check has to be tested in ISOLATION, which needs members whose own values are
// empty so `longest` is 0 and the length guard CANNOT fire. Without that, writeOne trims a
// whitespace-only reply to "" and the length guard catches it instead — so deleting the empty
// check entirely still passed, and the test proved nothing about the guard it named. (Found by
// mutation testing; the end-to-end case below stays, since it covers the realistic path.)
func TestTheEmptyValueGuardIsWhatRefusesABlankValue(t *testing.T) {
	members := []store.Facet{
		cf("thinking", "a", "", 0.7, 1),
		cf("thinking", "b", "", 0.7, 1),
	}
	got := validateMergedValue(compactAction{Action: "merge", Key: "a", Value: "   \n "}, members)
	if got != "empty_merged_value" {
		t.Errorf("a blank value must be refused by the empty guard, got %q", got)
	}
	// And a real value with no length floor to clear is accepted.
	if got := validateMergedValue(compactAction{Action: "merge", Key: "a", Value: "a real claim"}, members); got != "" {
		t.Errorf("a non-empty value should pass when there is no length floor, got %q", got)
	}
}

func TestAnEmptyMergedValueIsRefused(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1), cf("thinking", "b", "vb", 0.7, 1), cf("thinking", "c", "vc", 0.7, 1),
	}}
	c := compactor(st, "")
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			return "   \n  ", nil
		}
		return mergeReply("a", []string{"b", "c"}), nil
	}
	plans, _ := c.Plan(context.Background())
	if len(plans) > 0 && len(plans[0].Actions) != 0 {
		t.Error("an empty value would blank the survivor's stance — it must be refused")
	}
}

// A group whose write call fails must be dropped, not applied with no value, and must not take
// the other groups with it.
func TestAFailedWriteDropsOnlyItsOwnGroup(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1), cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1), cf("thinking", "d", "vd", 0.7, 1),
	}}
	c := compactor(st, "")
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING: doomed") {
			return "", context.DeadlineExceeded
		}
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			return mergedValue, nil
		}
		return `[{"action":"merge","dimension":"thinking","key":"doomed","absorb":["a","b"],"why":"x"},
		         {"action":"merge","dimension":"thinking","key":"survives","absorb":["c","d"],"why":"y"}]`, nil
	}
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Actions) != 1 || plans[0].Actions[0].Key != "survives" {
		t.Fatalf("only the failed group should be dropped, got %+v", plans)
	}
	if plans[0].Failed != 1 {
		t.Errorf("the failure must be reported, got Failed=%d", plans[0].Failed)
	}
	// Archived is counted from the FINAL actions, so the dropped group's facets must not be
	// reported as about to be archived.
	if plans[0].Archived != 2 {
		t.Errorf("want 2 archived (only the surviving group), got %d", plans[0].Archived)
	}
}

// Compaction reshapes a profile on a model's judgment, so the reason has to outlive the log
// line. Without it you can see THAT nine facets became one but never why they were judged one
// trait.
func TestTheMergeReasonIsPersistedOnEveryTouchedFacet(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1), cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1), cf("habits", "junk", "one-off", 0.2, 1),
	}}
	c := compactor(st, "")
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			return mergedValue, nil
		}
		if strings.Contains(input, "DIMENSION: habits") {
			return `[{"action":"retire","dimension":"habits","key":"junk","why":"a single incident, not a pattern"}]`, nil
		}
		return `[{"action":"merge","dimension":"thinking","key":"a","absorb":["b","c"],"why":"one shared mechanism"}]`, nil
	}
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(context.Background(), plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"b", "c"} {
		got := writtenByRef(t, st, "thinking", k)
		if got.ArchiveReason != "one shared mechanism" {
			t.Errorf("%s should record why it was merged, got %q", k, got.ArchiveReason)
		}
	}
	if got := writtenByRef(t, st, "habits", "junk"); got.ArchiveReason != "a single incident, not a pattern" {
		t.Errorf("a retirement should record its reason, got %q", got.ArchiveReason)
	}
}

func TestStripFenceLeavesPlainProseAlone(t *testing.T) {
	cases := map[string]string{
		"plain prose":                 "plain prose",
		"```\nfenced value\n```":      "fenced value",
		"```markdown\nvalue\n```":     "value",
		"has ``` inside it somewhere": "has ``` inside it somewhere",
	}
	for in, want := range cases {
		if got := stripFence(in); got != want {
			t.Errorf("stripFence(%q) = %q, want %q", in, got, want)
		}
	}
}
