package distill

import (
	"context"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// Compaction must not exempt a facet from decay.
//
// Decay-on-read (#173) ages a facet by the observations recorded since it was last
// confirmed, read from FacetVersion.ConfidenceSeq — and a ConfidenceSeq of 0 means "no
// clock", so DecayedConfidence returns the stored value untouched. Every version the
// REVIEWER writes stamps the current seq. Compaction landed first and was never updated:
// all three of its version writes left ConfidenceSeq at 0, so every facet compaction
// created or rewrote stopped decaying until some later review happened to reconfirm it.
// The facets that never recur are precisely the ones decay exists for, and those were the
// ones left frozen.
//
// The rule is "inherit the FRESHEST real confirmation among the facets being merged", not
// "stamp now": a rewrite is not new evidence, and restarting the clock would hand every
// compacted facet a free confidence refresh — the same bug pointed the other way.

// cfSeq is a one-version active facet confirmed at a given observation seq.
func cfSeq(dim, key, value string, conf float64, seq int64) store.Facet {
	return store.Facet{
		Lens: "default", Dimension: dim, Key: key, LastSeen: "2026-10-01T00:00:00Z",
		Versions: []store.FacetVersion{{
			Value: value, Confidence: conf, ConfidenceSeq: seq,
			RecordedAt: "2026-10-01T00:00:00Z", ValidFrom: "2026-10-01T00:00:00Z",
			BecauseOf: []string{"obs-" + key},
		}},
	}
}

func applyOne(t *testing.T, st *fakeCompactStore, plan string) {
	t.Helper()
	c := compactor(st, plan)
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) == 0 || len(plans[0].Actions) == 0 {
		t.Fatalf("the plan was refused, so nothing below is exercised: %+v", plans)
	}
	if err := c.Apply(context.Background(), plans, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionKeepsTheDecayClock(t *testing.T) {
	cases := []struct {
		name    string
		facets  []store.Facet
		plan    string
		want    int64
		surv    string
		comment string
	}{
		{
			name: "new abstraction node",
			facets: []store.Facet{
				cfSeq("thinking", "a", "value a", 0.8, 120),
				cfSeq("thinking", "b", "value b", 0.8, 450),
				cfSeq("thinking", "c", "value c", 0.8, 300),
			},
			plan: `[{"action":"merge","dimension":"thinking","key":"node","value":"x","absorb":["a","b","c"],"confidence":0.8,"why":"same"}]`,
			want: 450, surv: "node",
			comment: "the freshest member was confirmed at 450",
		},
		{
			name: "existing survivor rewritten, absorbed member is fresher",
			facets: []store.Facet{
				cfSeq("thinking", "surv", "old survivor value", 0.8, 200),
				cfSeq("thinking", "a", "value a", 0.8, 900),
				cfSeq("thinking", "b", "value b", 0.8, 100),
			},
			plan: `[{"action":"merge","dimension":"thinking","key":"surv","value":"x","absorb":["a","b"],"confidence":0.8,"why":"same"}]`,
			want: 900, surv: "surv",
			comment: "an absorbed facet was reconfirmed more recently than the survivor",
		},
		{
			name: "existing survivor rewritten, survivor is fresher",
			facets: []store.Facet{
				cfSeq("thinking", "surv", "old survivor value", 0.8, 1000),
				cfSeq("thinking", "a", "value a", 0.8, 300),
				cfSeq("thinking", "b", "value b", 0.8, 100),
			},
			plan: `[{"action":"merge","dimension":"thinking","key":"surv","value":"x","absorb":["a","b"],"confidence":0.8,"why":"same"}]`,
			want: 1000, surv: "surv",
			comment: "the survivor's own confirmation must not be pulled BACK by older members",
		},
		{
			name: "existing survivor, value unchanged",
			facets: []store.Facet{
				cfSeq("thinking", "surv", mergedValue, 0.8, 200),
				cfSeq("thinking", "a", "value a", 0.8, 700),
				cfSeq("thinking", "b", "value b", 0.8, 100),
			},
			plan: `[{"action":"merge","dimension":"thinking","key":"surv","value":"x","absorb":["a","b"],"confidence":0.8,"why":"same"}]`,
			want: 700, surv: "surv",
			comment: "no new version is opened, but the survivor now speaks for a fresher member",
		},
	}

	// The fourth write path: the survivor exists but every version of it is CLOSED, so there is
	// no current version to rewrite and compaction appends a fresh one.
	closed := cfSeq("thinking", "surv", "an expired survivor value", 0.8, 50)
	closed.Versions[0].ValidTo = "2026-10-15T00:00:00Z"
	cases = append(cases, struct {
		name    string
		facets  []store.Facet
		plan    string
		want    int64
		surv    string
		comment string
	}{
		name: "existing survivor with every version closed",
		facets: []store.Facet{
			closed,
			cfSeq("thinking", "a", "value a", 0.8, 640),
			cfSeq("thinking", "b", "value b", 0.8, 200),
		},
		plan: `[{"action":"merge","dimension":"thinking","key":"surv","value":"x","absorb":["a","b"],"confidence":0.8,"why":"same"}]`,
		want: 640, surv: "surv",
		comment: "the reopened survivor has no current version of its own to contribute",
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCompactStore{facets: tc.facets}
			applyOne(t, st, tc.plan)
			surv := writtenByRef(t, st, "thinking", tc.surv)
			cur := surv.Current()
			if cur == nil {
				t.Fatal("survivor has no current version")
			}
			if cur.ConfidenceSeq != tc.want {
				t.Errorf("survivor ConfidenceSeq = %d, want %d (%s)", cur.ConfidenceSeq, tc.want, tc.comment)
			}

			// The OUTCOME, not just the field: one half-life after its freshest confirmation,
			// the merged facet must have decayed. With ConfidenceSeq left at 0 it reads back
			// exactly as stored, forever.
			later := tc.want + int64(store.FacetDecayHalfLifeObs)
			if got := cur.DecayedConfidence(later); got >= cur.Confidence {
				t.Errorf("a compacted facet did not decay: confidence %.2f, one half-life later still %.2f",
					cur.Confidence, got)
			}
		})
	}
}
