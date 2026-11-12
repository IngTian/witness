package distill

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/store"
)

// The regroup round. Round 1 merges only within a batch, so a dimension that needed several
// batches ended near batches x 12 with one trait formed once per batch: on an A/B over the
// owner's archive, state's 4 batches left 48 survivors holding 7 obvious duplicate clusters, and
// in none of them did two members share a batch. These pin the round that joins them.

// fakeRounds answers the three kinds of call and counts each: round-1 grouping (the reply whose
// trigger key is in the batch, else none), the regroup call, and stage-2 writes.
type fakeRounds struct {
	round1  map[string]string
	regroup func() (string, error)
	write   func(input string) (string, error)

	groups, regroups int
	regroupInput     string
	writes           []string
	last             Progress // the final progress event Plan reported
}

func (f *fakeRounds) run(_ context.Context, _, system, input string) (string, error) {
	switch {
	case system == compactRegroupPrompt:
		f.regroups++
		f.regroupInput = input
		return f.regroup()
	case strings.Contains(input, "THE TRAIT YOU ARE WRITING"):
		f.writes = append(f.writes, input)
		if f.write != nil {
			return f.write(input)
		}
		return mergedValue, nil
	}
	f.groups++
	for k, reply := range f.round1 {
		if strings.Contains(input, "key: "+k+"\n") {
			return reply, nil
		}
	}
	return "[]", nil
}

func mg(key, why string, conf float64, absorb ...string) compactAction {
	return compactAction{Action: "merge", Dimension: "thinking", Key: key, Absorb: absorb, Why: why, Confidence: conf}
}

func actionsJSON(as ...compactAction) string {
	b, _ := json.Marshal(as)
	return string(b)
}

func replies(s string) func() (string, error) { return func() (string, error) { return s, nil } }

// thinking/a..d at 7 chars each, so BatchChars 14 makes the batches [a b] [c d], and round 1
// forms g1 from the first and g2 from the second.
func twoBatchRounds() (*fakeCompactStore, *fakeRounds) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "value a", 0.7, 1), cf("thinking", "b", "value b", 0.7, 1),
		cf("thinking", "c", "value c", 0.7, 1), cf("thinking", "d", "value d", 0.7, 1),
	}}
	return st, &fakeRounds{round1: map[string]string{
		"a": actionsJSON(mg("g1", "why one", 0.6, "a", "b")),
		"c": actionsJSON(mg("g2", "why two", 0.8, "c", "d")),
	}}
}

func planWith(t *testing.T, st *fakeCompactStore, f *fakeRounds, batchChars int) (*Compactor, CompactPlan) {
	t.Helper()
	c := compactor(st, "")
	c.Runner, c.BatchChars = f.run, batchChars
	c.Progress = func(p Progress) { f.last = p }
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("want one plan, got %+v", plans)
	}
	return c, plans[0]
}

func keysOf(as []compactAction) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Key)
	}
	slices.Sort(out)
	return out
}

func sortedAbsorb(a compactAction) []string {
	out := slices.Clone(a.Absorb)
	slices.Sort(out)
	return out
}

// Two groups formed in different batches, plus a facet round 1 left alone, become ONE merge
// over all the original facets, written once — and applied with every original pointing at it.
func TestARegroupJoinsTraitsFormedInSeparateBatches(t *testing.T) {
	st, f := twoBatchRounds()
	e := cf("thinking", "e", "value e", 0.7, 1)
	e.Versions[0].Claim = "claim of e"
	st.facets = append(st.facets, e) // its own third batch, which round 1 leaves alone
	f.regroup = replies(actionsJSON(mg("g", "one mechanism", 0, "g1", "g2", "e")))

	c, plan := planWith(t, st, f, 14)
	if f.groups != 3 || f.regroups != 1 || len(f.writes) != 1 {
		t.Fatalf("want 3 grouping calls, 1 regroup and 1 write; got %d, %d, %d", f.groups, f.regroups, len(f.writes))
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("the joined groups must be one action, got %+v", plan.Actions)
	}
	a := plan.Actions[0]
	if a.Key != "g" || a.Why != "one mechanism" || !slices.Equal(sortedAbsorb(a), []string{"a", "b", "c", "d", "e"}) {
		t.Errorf("want g absorbing a..e for the regroup's reason, got %+v", a)
	}
	if a.Confidence != 0.8 {
		t.Errorf("the union takes its strongest member's confidence (g2's 0.8), got %v", a.Confidence)
	}
	if plan.Archived != 5 {
		t.Errorf("want 5 archived, got %d", plan.Archived)
	}
	for _, v := range []string{"value a", "value b", "value c", "value d", "value e"} {
		if !strings.Contains(f.writes[0], v) {
			t.Errorf("the union is written from the original facets, missing %q", v)
		}
	}

	// The regroup sees each group as key + why and each lone facet as key + claim. No values,
	// and none of the bookkeeping the writer would later copy as evidence.
	for _, want := range []string{"g1", "why one", "g2", "why two", "key: e\n", "claim of e"} {
		if !strings.Contains(f.regroupInput, want) {
			t.Errorf("the regroup input is missing %q:\n%s", want, f.regroupInput)
		}
	}
	for _, leak := range []string{"value a", "value e", "0.6", "0.7", "0.8", "confidence", "version", "last_seen", "2026-10-01"} {
		if strings.Contains(f.regroupInput, leak) {
			t.Errorf("the regroup input carries %q", leak)
		}
	}

	if err := c.Apply(context.Background(), []CompactPlan{plan}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if got := writtenByRef(t, st, "thinking", k); got.MergedInto != "thinking/g" {
			t.Errorf("%s should be merged into thinking/g, got %q", k, got.MergedInto)
		}
	}
	if g := writtenByRef(t, st, "thinking", "g"); len(g.Current().BecauseOf) != 5 {
		t.Errorf("the survivor carries every original's evidence, got %v", g.Current().BecauseOf)
	}
}

// The commonest second-round case: a PAIR whose survivor is one of the two. Reusing round 1's
// validator would refuse it as merge_too_small. An existing survivor of the other group is
// absorbed like any member.
func TestARegroupPairKeepsAnExistingSurvivor(t *testing.T) {
	st := &fakeCompactStore{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		st.facets = append(st.facets, cf("thinking", k, "value "+k, 0.7, 1))
	}
	f := &fakeRounds{
		round1: map[string]string{ // BatchChars 21: [a b c] [d e f]
			"a": actionsJSON(mg("a", "w1", 0.6, "b", "c")),
			"d": actionsJSON(mg("d", "w2", 0.6, "e", "f")),
		},
		regroup: replies(actionsJSON(mg("a", "same", 0, "d"))),
	}
	_, plan := planWith(t, st, f, 21)
	if len(plan.Actions) != 1 || len(f.writes) != 1 {
		t.Fatalf("the pair must be one action written once, got %d writes and %+v", len(f.writes), plan.Actions)
	}
	if a := plan.Actions[0]; a.Key != "a" || !slices.Equal(sortedAbsorb(a), []string{"b", "c", "d", "e", "f"}) {
		t.Errorf("want a absorbing b..f, got %+v", a)
	}
	for _, want := range []string{"--- a\n", "--- d\n", "value f"} {
		if !strings.Contains(f.writes[0], want) {
			t.Errorf("the pair's write is missing %q", want)
		}
	}
}

// A union whose originals exceed the cap would be written in one call expected to hit the
// runner's 20-minute wall, so it is not folded: its groups are written separately, as before.
func TestAUnionOverTheCapStaysSplit(t *testing.T) {
	n := compactUnionMaxChars/4 + 1 // four of these exceed the cap together
	st := &fakeCompactStore{}
	for _, k := range []string{"a", "b", "c", "d"} {
		st.facets = append(st.facets, cf("thinking", k, k+strings.Repeat(".", n-1), 0.7, 1))
	}
	_, f := twoBatchRounds()
	f.regroup = replies(actionsJSON(mg("g", "one mechanism", 0, "g1", "g2")))
	long := strings.Repeat("m", n)
	f.write = func(string) (string, error) { return long, nil }

	_, plan := planWith(t, st, f, 2*n)
	if f.regroups != 1 || len(f.writes) != 2 {
		t.Fatalf("want 1 regroup and 2 writes, got %d and %d", f.regroups, len(f.writes))
	}
	if got := keysOf(plan.Actions); !slices.Equal(got, []string{"g1", "g2"}) {
		t.Errorf("an over-cap union keeps its round-1 groups, got %v", got)
	}
	if plan.Skipped["regroup_union_over_cap"] != 1 {
		t.Errorf("the capped union should be tallied, got %v", plan.Skipped)
	}
}

// The owner's largest duplicate cluster (trading: 41 facets, ~24.3k chars) stayed split under the
// 20k cap the 10-minute wall set. The wall is 20 minutes now, so a union that size folds.
func TestAUnionTheSizeOfTheTradingClusterFolds(t *testing.T) {
	n := 24_300 / 4
	st := &fakeCompactStore{}
	for _, k := range []string{"a", "b", "c", "d"} {
		st.facets = append(st.facets, cf("thinking", k, k+strings.Repeat(".", n-1), 0.7, 1))
	}
	_, f := twoBatchRounds()
	f.regroup = replies(actionsJSON(mg("g", "one mechanism", 0, "g1", "g2")))
	long := strings.Repeat("m", n)
	f.write = func(string) (string, error) { return long, nil }

	_, plan := planWith(t, st, f, 2*n)
	if got := keysOf(plan.Actions); !slices.Equal(got, []string{"g"}) || len(f.writes) != 1 {
		t.Errorf("a 24.3k-char union should fold into one write, got %v in %d writes (skipped %v)", got, len(f.writes), plan.Skipped)
	}
}

// A union's write that fails or is refused must leave the dimension no worse than round 1: its
// groups are written separately, each fenced by the OTHER groups rather than by the union that
// covers its own ground.
func TestAFailedUnionWriteFallsBackToItsGroups(t *testing.T) {
	for name, reply := range map[string]func() (string, error){
		"fails":   func() (string, error) { return "", context.DeadlineExceeded },
		"refused": replies("short"), // shorter than its largest input
	} {
		t.Run(name, func(t *testing.T) {
			st, f := twoBatchRounds()
			f.regroup = replies(actionsJSON(mg("g", "one mechanism", 0, "g1", "g2")))
			f.write = func(in string) (string, error) {
				if strings.Contains(in, "THE TRAIT YOU ARE WRITING: g\n") {
					return reply()
				}
				return mergedValue, nil
			}
			_, plan := planWith(t, st, f, 14)
			if len(f.writes) != 3 {
				t.Fatalf("want the union's write then one per group, got %d writes", len(f.writes))
			}
			if m := stage(t, f.last, "writing merges"); m.Done != 3 || m.Total != 3 {
				t.Errorf("the fallback writes must join the merge total, ended at %d/%d", m.Done, m.Total)
			}
			if got := keysOf(plan.Actions); !slices.Equal(got, []string{"g1", "g2"}) {
				t.Fatalf("the fallback must write the round-1 groups, got %v", got)
			}
			for _, a := range plan.Actions {
				if a.Value != mergedValue {
					t.Errorf("%s should carry its own write, got %q", a.Key, a.Value)
				}
			}
			if name == "fails" && plan.Failed != 1 {
				t.Errorf("the failed union write must be reported, got Failed=%d", plan.Failed)
			}
			if name == "refused" && plan.Skipped["merged_value_shorter_than_its_largest_input"] != 1 {
				t.Errorf("the refused union must be tallied, got %v", plan.Skipped)
			}
			for _, in := range f.writes {
				if strings.Contains(in, "THE TRAIT YOU ARE WRITING: g2\n") &&
					(strings.Contains(in, "  g — ") || !strings.Contains(in, "  g1 — why one")) {
					t.Errorf("g2's fallback write must be fenced by g1, not by the union:\n%s", in)
				}
			}
		})
	}
}

// Facets round 1 left alone can be joined too, but two of them under one's own key absorb a
// single facet, which round 1 refuses as a rename and Apply would half-apply (the absorbed facet
// archived, the survivor never rewritten). Under a new key they are a real merge.
func TestARegroupOfLoneFacetsNeedsTwoAbsorbed(t *testing.T) {
	st, _ := twoBatchRounds()
	f := &fakeRounds{regroup: replies(actionsJSON( // round 1 merges nothing: a, b, c, d are lone
		mg("a", "pair", 0, "c"), mg("joined", "same", 0, "b", "d")))}
	_, plan := planWith(t, st, f, 14)
	if len(plan.Actions) != 1 || plan.Actions[0].Key != "joined" || !slices.Equal(sortedAbsorb(plan.Actions[0]), []string{"b", "d"}) {
		t.Fatalf("want only joined absorbing b and d, got %+v", plan.Actions)
	}
	if plan.Skipped["regroup_merge_too_small"] != 1 {
		t.Errorf("the one-facet merge should be tallied, got %v", plan.Skipped)
	}
	if plan.Actions[0].Confidence != 0.7 {
		t.Errorf("lone facets lend their own confidence, got %v", plan.Actions[0].Confidence)
	}
}

// A unit named by two merges would be folded twice, and one new key minted by two merges would
// make Apply create the facet and then rewrite it. The first claim wins; the rest are tallied.
func TestARegroupNamesEachUnitOnce(t *testing.T) {
	st, f := twoBatchRounds()
	st.facets = append(st.facets, cf("thinking", "e", "value e", 0.7, 1), cf("thinking", "f", "value f", 0.7, 1))
	f.regroup = replies(actionsJSON(
		mg("g", "w", 0, "g1", "g2"),
		mg("h", "reuses g2", 0, "g2", "e"),
		mg("g", "mints g again", 0, "e", "f")))
	_, plan := planWith(t, st, f, 14)
	if len(plan.Actions) != 1 || plan.Actions[0].Key != "g" || !slices.Equal(sortedAbsorb(plan.Actions[0]), []string{"a", "b", "c", "d"}) {
		t.Fatalf("want only g absorbing a..d, got %+v", plan.Actions)
	}
	if plan.Skipped["regroup_already_claimed"] != 2 {
		t.Errorf("both later claims should be refused, got %v", plan.Skipped)
	}
}

// One batch already saw the whole dimension, so there is nothing to join and no call to spend.
func TestASingleBatchDimensionMakesNoRegroupCall(t *testing.T) {
	st, f := twoBatchRounds()
	f.regroup = replies("[]")
	planWith(t, st, f, 0)
	if f.groups != 1 || f.regroups != 0 {
		t.Errorf("want 1 grouping call and no regroup, got %d and %d", f.groups, f.regroups)
	}
}

// A regroup that fails, is unparseable, or proposes nothing valid leaves round 1 exactly as it
// was: the round can only add merges, never cost the ones already found.
func TestAFailedRegroupLeavesRoundOneUnchanged(t *testing.T) {
	for name, reply := range map[string]func() (string, error){
		"fails":       func() (string, error) { return "", context.DeadlineExceeded },
		"unparseable": replies("not json"),
		"invalid":     replies(`[{"action":"retire","dimension":"thinking","key":"g1","why":"x"}]`),
	} {
		t.Run(name, func(t *testing.T) {
			st, f := twoBatchRounds()
			f.regroup = reply
			_, plan := planWith(t, st, f, 14)
			if f.regroups != 1 || len(f.writes) != 2 {
				t.Fatalf("want 1 regroup and 2 writes, got %d and %d", f.regroups, len(f.writes))
			}
			if got := keysOf(plan.Actions); !slices.Equal(got, []string{"g1", "g2"}) {
				t.Fatalf("round 1's groups must stand, got %v", got)
			}
			for _, a := range plan.Actions {
				if want := map[string][]string{"g1": {"a", "b"}, "g2": {"c", "d"}}[a.Key]; !slices.Equal(sortedAbsorb(a), want) {
					t.Errorf("%s changed its members: %v", a.Key, a.Absorb)
				}
			}
			wantFailed := 1
			if name == "invalid" {
				wantFailed = 0
				if plan.Skipped["regroup_not_a_merge"] != 1 {
					t.Errorf("the refused retire should be tallied, got %v", plan.Skipped)
				}
			}
			if plan.Failed != wantFailed {
				t.Errorf("want Failed=%d, got %d", wantFailed, plan.Failed)
			}
		})
	}
}

// The collapse guard refuses a whole dimension, so a runaway union that pushed it under the floor
// once cost round 1's valid merges with it: 10 facets, round 1 left 4 (floor 3), and one union
// over g1, g2 and i would leave 2, so nothing at all was applied. The regroup checks the floor
// itself and drops its unions instead — the round can add merges, never cost round 1's.
func TestARegroupThatWouldCollapseTheDimensionKeepsRoundOne(t *testing.T) {
	st := &fakeCompactStore{}
	for _, k := range strings.Split("abcdefghij", "") {
		st.facets = append(st.facets, cf("thinking", k, "value "+k, 0.7, 1))
	}
	f := &fakeRounds{
		round1: map[string]string{ // BatchChars 35: [a..e] [f..j]
			"a": actionsJSON(mg("g1", "w1", 0.6, "a", "b", "c", "d", "e")),
			"f": actionsJSON(mg("g2", "w2", 0.6, "f", "g", "h")),
		},
		regroup: replies(actionsJSON(mg("u", "everything", 0, "g1", "g2", "i"))), // leaves u and j
	}
	_, plan := planWith(t, st, f, 35)
	if got := keysOf(plan.Actions); !slices.Equal(got, []string{"g1", "g2"}) {
		t.Fatalf("round 1's merges must stand, got %v (skipped %v)", got, plan.Skipped)
	}
	if plan.Archived != 8 || len(f.writes) != 2 {
		t.Errorf("want round 1's 8 archived in 2 writes, got %d in %d", plan.Archived, len(f.writes))
	}
	if plan.Skipped["regroup_would_collapse_the_dimension"] != 1 || plan.Skipped["would_collapse_the_dimension"] != 0 {
		t.Errorf("the union, not the dimension, should be refused: %v", plan.Skipped)
	}
}

// The regroup is one call over every unit, so a lone facet with no claim (the claim pass is capped
// per run, and `witness compact` claims only after Apply) must not send its whole value: it is
// shown by a rune-safe excerpt, which keeps a unit's size independent of the value's.
func TestARegroupShowsAnUnclaimedFacetByAnExcerpt(t *testing.T) {
	st, f := twoBatchRounds()
	long := strings.Repeat("长", 3*regroupExcerptRunes) // multi-byte, so a byte cut would split a rune
	st.facets = append(st.facets, cf("thinking", "e", long, 0.7, 1))
	f.regroup = replies("[]")
	planWith(t, st, f, 14)
	if !utf8.ValidString(f.regroupInput) {
		t.Error("the excerpt split a rune")
	}
	if !strings.Contains(f.regroupInput, "key: e\nclaim: "+strings.Repeat("长", regroupExcerptRunes)) ||
		strings.Contains(f.regroupInput, strings.Repeat("长", regroupExcerptRunes+1)) {
		t.Errorf("want e shown by its first %d runes, got %d bytes of input", regroupExcerptRunes, len(f.regroupInput))
	}
}

func TestValidateRegroup(t *testing.T) {
	units := map[string]regroupUnit{"g1": {}, "g2": {}, "e": {}}
	byKey := map[string]store.Facet{}
	for _, k := range []string{"a", "b", "e"} { // a, b: members of g1, existing but not units
		byKey[store.FacetRef("thinking", k)] = cf("thinking", k, "v", 0.7, 1)
	}
	for _, tc := range []struct {
		a    compactAction
		used string
		want string
	}{
		{a: mg("g1", "", 0, "g2")},       // a pair whose survivor is one of the two
		{a: mg("new", "", 0, "g1", "e")}, // a new key over a group and a lone facet
		{a: mg("e", "", 0, "g1")},        // a lone facet as the survivor
		{a: compactAction{Action: "retire", Key: "g1"}, want: "not_a_merge"},
		{a: compactAction{Action: "merge", Dimension: "habits", Key: "g1", Absorb: []string{"g2"}}, want: "wrong_dimension"},
		{a: mg(" ", "", 0, "g1", "g2"), want: "empty_key"},
		{a: mg("a", "", 0, "g1", "g2"), want: "survivor_unseen"},
		{a: mg("g1", "", 0), want: "merge_too_small"},
		{a: mg("new", "", 0, "g1"), want: "merge_too_small"},
		{a: mg("g1", "", 0, "g1", "g2"), want: "absorb_includes_survivor"},
		{a: mg("new", "", 0, "g1", "b"), want: "absorb_unseen_unit"},
		{a: mg("new", "", 0, "g1", "g1"), want: "absorb_duplicate"},
		{a: mg("g1", "", 0, "g2"), used: "g2", want: "already_claimed"},
		{a: mg("g1", "", 0, "g2"), used: "g1", want: "already_claimed"},
		{a: mg("new", "", 0, "g1", "g2"), used: "new", want: "already_claimed"}, // one new key minted twice
	} {
		used := map[string]bool{tc.used: tc.used != ""}
		if got := validateRegroup(tc.a, "thinking", units, byKey, used); got != tc.want {
			t.Errorf("%+v (used %q): got %q, want %q", tc.a, tc.used, got, tc.want)
		}
	}
}
