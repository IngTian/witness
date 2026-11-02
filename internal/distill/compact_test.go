package distill

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Compaction is the only thing in witness that takes a facet OUT of the profile, so these
// tests are mostly about what it must refuse to do. The model proposes; Go decides what is
// permissible (same division of labour as applyFacet). Every refusal below corresponds to a
// concrete way a bad reply would damage a profile.

func cf(dim, key, value string, conf float64, versions int) store.Facet {
	f := store.Facet{Lens: "default", Dimension: dim, Key: key, LastSeen: "2026-10-01T00:00:00Z"}
	for i := 0; i < versions; i++ {
		v := store.FacetVersion{
			Value: value, Confidence: conf, RecordedAt: "2026-10-01T00:00:00Z",
			ValidFrom: "2026-10-01T00:00:00Z", BecauseOf: []string{"obs-" + key},
		}
		if i < versions-1 {
			v.ValidTo = "2026-10-01T00:00:00Z"
		}
		f.Versions = append(f.Versions, v)
	}
	return f
}

type fakeCompactStore struct {
	facets    []store.Facet
	writes    int
	written   []store.Facet
	highWater int64 // the decay clock's "now"; 0 leaves every confidence as stored
}

func (f *fakeCompactStore) ReadFacets() ([]store.Facet, error)   { return f.facets, nil }
func (f *fakeCompactStore) ObservationHighWater() (int64, error) { return f.highWater, nil }
func (f *fakeCompactStore) WriteFacets(fs []store.Facet) error {
	f.writes++
	f.written = fs
	return nil
}

// mergedValue is what the stage-2 stub returns. It must be at least as long as the longest
// input value or validateMergedValue refuses it as a summary — which is the guard working.
const mergedValue = "one combined trait, written long enough to clear the length guard that refuses a merge shorter than its largest input"

// compactor wires a fake that answers BOTH stages: stage 1 (grouping) gets the canned JSON
// array, stage 2 (writing one value) gets prose. They are told apart by the stage-2 header,
// because a single canned reply would feed a JSON array in as a facet value.
func compactor(st *fakeCompactStore, groupReply string) *Compactor {
	return &Compactor{
		Store:  st,
		Lenses: []*lens.Lens{{Name: "default", Review: "REVIEW"}},
		Config: store.Config{},
		Runner: func(_ context.Context, _, _, input string) (string, error) {
			if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
				return mergedValue, nil
			}
			return groupReply, nil
		},
	}
}

func writtenByRef(t *testing.T, st *fakeCompactStore, dim, key string) store.Facet {
	t.Helper()
	for _, f := range st.written {
		if f.Dimension == dim && f.Key == key {
			return f
		}
	}
	t.Fatalf("facet %s/%s missing from the write — compaction must archive, never delete", dim, key)
	return store.Facet{}
}

// A dry run that writes would make the whole --apply gate a lie.
func TestPlanWritesNothing(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "value a", 0.7, 1),
		cf("thinking", "b", "value b", 0.7, 1),
		cf("thinking", "c", "value c", 0.7, 1),
	}}
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"a","value":"merged","absorb":["b","c"],"confidence":0.8,"why":"same"}]`)
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.writes != 0 {
		t.Fatalf("Plan must not write; got %d writes", st.writes)
	}
	if len(plans) != 1 || len(plans[0].Actions) != 1 {
		t.Fatalf("want one planned action, got %+v", plans)
	}
	if plans[0].Archived != 2 {
		t.Errorf("want 2 facets archived, got %d", plans[0].Archived)
	}
}

// The headline behaviour: a merge archives the absorbed facets WITH their history, points them
// at the survivor, and rewrites the survivor's value as a new version rather than overwriting
// the old one.
func TestMergeArchivesAbsorbedAndKeepsHistory(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "survivor", "old survivor value", 0.7, 2),
		cf("thinking", "dup1", "dup one", 0.6, 1),
		cf("thinking", "dup2", "dup two", 0.6, 1),
	}}
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"survivor","value":"one combined trait","absorb":["dup1","dup2"],"confidence":0.85,"why":"paraphrases"}]`)
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(context.Background(), plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(st.written) != 3 {
		t.Fatalf("all 3 facets must survive the write (archived, not deleted); got %d", len(st.written))
	}

	for _, k := range []string{"dup1", "dup2"} {
		got := writtenByRef(t, st, "thinking", k)
		if got.Active() {
			t.Errorf("%s should be archived", k)
		}
		if got.MergedInto != "thinking/survivor" {
			t.Errorf("%s must link to its survivor, got %q", k, got.MergedInto)
		}
		if len(got.Versions) != 1 || got.Versions[0].Value != "dup "+map[string]string{"dup1": "one", "dup2": "two"}[k] {
			t.Errorf("%s lost its history: %+v", k, got.Versions)
		}
	}

	surv := writtenByRef(t, st, "thinking", "survivor")
	if !surv.Active() {
		t.Fatal("the survivor must stay active")
	}
	if len(surv.Versions) != 3 {
		t.Fatalf("survivor should gain ONE version (2 -> 3), got %d", len(surv.Versions))
	}
	if surv.Versions[1].ValidTo == "" {
		t.Error("the superseded version must be closed — a value rewrite is a change arc")
	}
	cur := surv.Current()
	if cur == nil || cur.Value != mergedValue {
		t.Fatalf("survivor's current value should be the rewritten one, got %+v", cur)
	}
	// Provenance is the whole claim of the archive: a merge that drops the absorbed
	// facets' evidence makes the merged value unjustifiable.
	for _, want := range []string{"obs-dup1", "obs-dup2", "obs-survivor"} {
		if !containsStr(cur.BecauseOf, want) {
			t.Errorf("merged version must cite %s; got %v", want, cur.BecauseOf)
		}
	}
}

// A retirement has no survivor, so MergedInto must stay empty — otherwise trajectory
// expansion would follow a dangling link.
func TestRetireArchivesWithoutASurvivorLink(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{cf("habits", "noise", "one-off incident", 0.3, 1)}}
	c := compactor(st, `[{"action":"retire","dimension":"habits","key":"noise","why":"single incident, not a pattern"}]`)
	plans, _ := c.Plan(context.Background())
	if err := c.Apply(context.Background(), plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := writtenByRef(t, st, "habits", "noise")
	if got.Active() {
		t.Error("retired facet should be archived")
	}
	if got.MergedInto != "" {
		t.Errorf("a retirement must not name a survivor, got %q", got.MergedInto)
	}
	if len(got.Versions) != 1 {
		t.Error("retirement must keep the facet's history")
	}
}

// The survivor may be a key that did not exist — abstracting several specific facets into one
// general trait is the point of "rebalancing" rather than mere deduplication.
func TestMergeCanMintANewAbstractionKey(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "specific1", "v1", 0.6, 1),
		cf("thinking", "specific2", "v2", 0.6, 1),
	}}
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"general_trait","value":"the general form","absorb":["specific1","specific2"],"confidence":0.8,"why":"abstraction"}]`)
	plans, _ := c.Plan(context.Background())
	if err := c.Apply(context.Background(), plans, time.Now()); err != nil {
		t.Fatal(err)
	}
	newF := writtenByRef(t, st, "thinking", "general_trait")
	if !newF.Active() || newF.Current() == nil || newF.Current().Value != mergedValue {
		t.Fatalf("the new abstraction should be active with the merged value, got %+v", newF)
	}
	if !containsStr(newF.Current().BecauseOf, "obs-specific1") {
		t.Errorf("a new abstraction must inherit provenance, got %v", newF.Current().BecauseOf)
	}
}

// Everything the validator must refuse. Each case is a distinct way a model reply would
// otherwise corrupt the profile; they are grouped because they share one setup.
func TestValidatorRefusals(t *testing.T) {
	batch := []store.Facet{
		cf("thinking", "a", "va", 0.7, 1),
		cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1),
	}
	byKey := map[string]store.Facet{
		"thinking/a": batch[0], "thinking/b": batch[1], "thinking/c": batch[2],
		"thinking/unseen": cf("thinking", "unseen", "hidden", 0.7, 1),
	}
	cases := []struct {
		name   string
		action compactAction
		want   string
	}{
		{"merging a facet it was never shown",
			compactAction{Action: "merge", Key: "a", Value: "v", Absorb: []string{"b", "nope"}}, "absorb_unseen_facet"},
		{"rewriting a facet it was never shown",
			compactAction{Action: "merge", Key: "unseen", Value: "v", Absorb: []string{"a", "b"}}, "survivor_unseen"},
		{"absorbing the survivor into itself",
			compactAction{Action: "merge", Key: "a", Value: "v", Absorb: []string{"a", "b"}}, "absorb_includes_survivor"},
		{"a merge of one facet (a rename)",
			compactAction{Action: "merge", Key: "a", Value: "v", Absorb: []string{"b"}}, "merge_too_small"},
		{"the same facet twice in one merge",
			compactAction{Action: "merge", Key: "a", Value: "v", Absorb: []string{"b", "b"}}, "absorb_duplicate"},
		{"retiring a facet it was never shown",
			compactAction{Action: "retire", Key: "unseen"}, "retire_unseen_facet"},
		{"acting on another dimension",
			compactAction{Action: "retire", Dimension: "habits", Key: "a"}, "wrong_dimension"},
		{"an empty key",
			compactAction{Action: "retire", Key: ""}, "empty_key"},
		{"an action that is neither",
			compactAction{Action: "split", Key: "a"}, "unknown_action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateGrouping(tc.action, "thinking", batch, byKey, map[string]bool{}); got != tc.want {
				t.Errorf("want refusal %q, got %q", tc.want, got)
			}
		})
	}
}

// Two actions must never consume the same facet: the second would archive a facet already
// archived under a different survivor, so one of the two merges silently loses its content.
func TestAFacetCanOnlyBeClaimedOnce(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1),
		cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1),
		cf("thinking", "d", "vd", 0.7, 1),
	}}
	// Both groups try to absorb "b".
	c := compactor(st, `[
	  {"action":"merge","dimension":"thinking","key":"a","value":"m1","absorb":["b","c"],"confidence":0.8,"why":"x"},
	  {"action":"merge","dimension":"thinking","key":"d","value":"m2","absorb":["b","c"],"confidence":0.8,"why":"y"}
	]`)
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans[0].Actions) != 1 {
		t.Fatalf("the second overlapping merge must be refused, got %d actions", len(plans[0].Actions))
	}
	if plans[0].Skipped["already_claimed"] != 1 {
		t.Errorf("the refusal should be tallied as already_claimed, got %v", plans[0].Skipped)
	}
}

// The guard that replaced the archived-FRACTION cap. The fraction was the wrong shape: it
// limited how much is removed, while the damage case is being left with almost nothing — and
// the intended result IS a large removal. On real data it refused a correct 29-of-33 plan.
func TestCollapsingALargeDimensionIsRefused(t *testing.T) {
	// 12 facets (over the guard threshold) proposed down to a single survivor.
	var facets []store.Facet
	var absorb []string
	for i := 0; i < 12; i++ {
		k := string(rune('a' + i))
		facets = append(facets, cf("thinking", k, "v"+k, 0.7, 1))
		if i > 0 {
			absorb = append(absorb, k)
		}
	}
	st := &fakeCompactStore{facets: facets}
	c := compactor(st, mergeReply("a", absorb))
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if len(p.Actions) != 0 {
			t.Errorf("a collapse to 1 survivor must not be applied, got %d actions", len(p.Actions))
		}
		if p.Skipped["would_collapse_the_dimension"] == 0 {
			t.Errorf("the refusal should be tallied, got %v", p.Skipped)
		}
	}
	if st.writes != 0 {
		t.Error("a refused plan must write nothing")
	}
}

// The other half, and the one the old cap got wrong: a LARGE dimension reduced hard but not to
// nothing is exactly the goal. 20 facets to 4 survivors is an 80% reduction and must pass.
func TestADeepButNonCollapsingReductionIsAllowed(t *testing.T) {
	var facets []store.Facet
	for i := 0; i < 20; i++ {
		facets = append(facets, cf("thinking", string(rune('a'+i)), "v", 0.7, 1))
	}
	st := &fakeCompactStore{facets: facets}
	// Four groups of four absorbed each: 20 -> 4 survivors.
	c := compactor(st, `[{"action":"merge","dimension":"thinking","key":"a","absorb":["b","c","d","e"],"why":"1"},
	 {"action":"merge","dimension":"thinking","key":"f","absorb":["g","h","i","j"],"why":"2"},
	 {"action":"merge","dimension":"thinking","key":"k","absorb":["l","m","n","o"],"why":"3"},
	 {"action":"merge","dimension":"thinking","key":"p","absorb":["q","r","s","t"],"why":"4"}]`)
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Actions) != 4 {
		t.Fatalf("an 80%% reduction leaving 4 facets is the goal and must be allowed, got %+v", plans)
	}
	if plans[0].Archived != 16 {
		t.Errorf("want 16 archived, got %d", plans[0].Archived)
	}
}

// A small profile must still be compactable: a pure percentage cap would make a 3-facet lens
// unable to merge 2 of them, which is exactly the work compaction exists to do.
func TestSmallProfileIsStillCompactable(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1),
		cf("thinking", "b", "vb", 0.7, 1),
		cf("thinking", "c", "vc", 0.7, 1),
	}}
	c := compactor(st, mergeReply("a", []string{"b", "c"}))
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatalf("merging 2 of 3 facets must be allowed: %v", err)
	}
	if len(plans) != 1 || plans[0].Archived != 2 {
		t.Fatalf("want a plan archiving 2, got %+v", plans)
	}
}

func mergeReply(survivor string, absorb []string) string {
	var b strings.Builder
	b.WriteString(`[{"action":"merge","dimension":"thinking","key":"` + survivor + `","value":"merged","absorb":[`)
	for i, k := range absorb {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + k + `"`)
	}
	b.WriteString(`],"confidence":0.8,"why":"x"}]`)
	return b.String()
}

// Compaction only ever considers the ACTIVE profile. An already-archived facet reaching a
// batch would be proposed for merging again, and Apply would re-point its MergedInto —
// rewriting lineage that is supposed to be a historical record.
func TestAlreadyArchivedFacetsAreNotReconsidered(t *testing.T) {
	archived := cf("thinking", "gone", "already archived", 0.5, 1)
	archived.ArchivedAt = "2026-10-02T00:00:00Z"
	archived.MergedInto = "thinking/keeper"
	st := &fakeCompactStore{facets: []store.Facet{cf("thinking", "keeper", "live", 0.7, 1), archived}}

	var sawInput string
	c := compactor(st, `[]`)
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		sawInput = input
		return "[]", nil
	}
	if _, err := c.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sawInput, "gone") {
		t.Error("an archived facet must not be offered to the model again")
	}
	if !strings.Contains(sawInput, "keeper") {
		t.Error("the active facet should be in the batch")
	}
}

// Found by the first real run: one batch failing threw away every action already planned from
// the batches that succeeded. On a 45-call lens that means any single transient model error
// costs the whole pass, so it would essentially never complete.
func TestAFailedBatchKeepsTheActionsAlreadyPlanned(t *testing.T) {
	// Two dimensions, so there are two batches and the first can succeed while the second
	// fails. 12 facets keeps the plan under the archive cap.
	var facets []store.Facet
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		facets = append(facets, cf("thinking", k, "v"+k, 0.7, 1))
		facets = append(facets, cf("habits", k, "v"+k, 0.7, 1))
	}
	st := &fakeCompactStore{facets: facets}
	c := compactor(st, "")
	groupCalls := 0
	c.Runner = func(_ context.Context, _, _, input string) (string, error) {
		if strings.Contains(input, "THE TRAIT YOU ARE WRITING") {
			return mergedValue, nil // stage 2 for whichever group survived
		}
		groupCalls++
		if strings.Contains(input, "DIMENSION: habits") {
			return "", context.DeadlineExceeded // the auth timeout / wall the real run hit
		}
		return mergeReply("a", []string{"b", "c"}), nil
	}

	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if groupCalls != 2 {
		t.Fatalf("both dimensions should be grouped, got %d grouping calls", groupCalls)
	}
	if len(plans) != 1 {
		t.Fatalf("the lens should still produce a plan, got %d", len(plans))
	}
	if len(plans[0].Actions) != 1 {
		t.Errorf("the successful batch's action must survive, got %d", len(plans[0].Actions))
	}
	if plans[0].Failed != 1 {
		t.Errorf("the failure must be reported so the plan reads as PARTIAL, got Failed=%d", plans[0].Failed)
	}
}

// A lens where every call failed must not report "nothing to compact" — that is the one
// wrong conclusion, because it looks exactly like an already-tight profile.
func TestAnAllFailedLensIsReportedNotSilentlyDropped(t *testing.T) {
	st := &fakeCompactStore{facets: []store.Facet{
		cf("thinking", "a", "va", 0.7, 1),
		cf("thinking", "b", "vb", 0.7, 1),
	}}
	c := compactor(st, "")
	c.Runner = func(context.Context, string, string, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Failed != 1 || len(plans[0].Actions) != 0 {
		t.Fatalf("want one plan with 0 actions and 1 failure, got %+v", plans)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
