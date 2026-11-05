package distill

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// Compaction must log every confidence change it makes. The confidence_log is the
// append-only record of what changed facet confidence and when, so readers can see the
// trajectory and understand corrections. Retired and absorbed facets log nothing: their
// confidence is not set, they are archived.

func TestCompactLogsSurvivorCases(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	nowStr := now.UTC().Format(time.RFC3339)

	cases := []struct {
		name        string
		facets      []store.Facet
		plan        string
		decaySeq    int64
		wantEvents  []store.ConfidenceEvent
		description string
	}{
		{
			name: "new abstraction",
			facets: []store.Facet{
				cfSeq("thinking", "a", "value a", 0.7, 500),
				cfSeq("thinking", "b", "value b", 0.8, 3500),
				cfSeq("thinking", "c", "value c", 0.6, 1000),
			},
			plan:     `[{"action":"merge","dimension":"thinking","key":"node","value":"merged","absorb":["a","b","c"],"confidence":0.85,"claim":"short","why":"same"}]`,
			decaySeq: 3500,
			wantEvents: []store.ConfidenceEvent{
				{
					Lens:      "default",
					Dimension: "thinking",
					Key:       "node",
					At:        nowStr,
					ClockSeq:  3500,
					From:      0,
					To:        0.85,
					Kind:      store.EventCompaction,
				},
			},
			description: "new abstraction: survivor did not exist, From=0",
		},
		{
			name: "rewritten",
			facets: []store.Facet{
				cfSeq("thinking", "survivor", "old value", 0.9, 500),
				cfSeq("thinking", "a", "value a", 0.7, 3500),
				cfSeq("thinking", "b", "value b", 0.8, 2000),
			},
			plan:     `[{"action":"merge","dimension":"thinking","key":"survivor","value":"new value","absorb":["a","b"],"confidence":0.8,"claim":"short","why":"same"}]`,
			decaySeq: 3500,
			wantEvents: []store.ConfidenceEvent{
				{
					Lens:      "default",
					Dimension: "thinking",
					Key:       "survivor",
					At:        nowStr,
					ClockSeq:  3500,
					From:      0.45, // 0.9 decayed from seq 500 to 3500 = 3000 obs elapsed = 1 half-life
					To:        0.8,
					Kind:      store.EventCompaction,
				},
			},
			description: "rewritten: old version closed, From is decayed confidence",
		},
		{
			name: "same value",
			facets: []store.Facet{
				cfSeq("thinking", "survivor", mergedValue, 0.9, 500),
				cfSeq("thinking", "a", "value a", 0.7, 3500),
				cfSeq("thinking", "b", "value b", 0.8, 2000),
			},
			plan:     `[{"action":"merge","dimension":"thinking","key":"survivor","value":"` + mergedValue + `","absorb":["a","b"],"confidence":0.9,"claim":"short","why":"same"}]`,
			decaySeq: 3500,
			wantEvents: []store.ConfidenceEvent{
				{
					Lens:      "default",
					Dimension: "thinking",
					Key:       "survivor",
					At:        nowStr,
					ClockSeq:  3500,
					From:      0.45, // 0.9 decayed from seq 500 to 3500
					To:        0.9,
					Kind:      store.EventCompaction,
				},
			},
			description: "same value: clock refreshed, From is pre-refresh decayed confidence",
		},
		{
			name: "reopen",
			facets: []store.Facet{
				{
					Lens: "default", Dimension: "thinking", Key: "survivor", LastSeen: "2026-10-01T00:00:00Z",
					ArchivedAt: "2026-10-01T00:00:00Z",
					Versions: []store.FacetVersion{{
						Value: "old value", Confidence: 0.9, ConfidenceSeq: 500,
						RecordedAt: "2026-10-01T00:00:00Z", ValidFrom: "2026-10-01T00:00:00Z",
						ValidTo: "2026-10-01T00:00:00Z", BecauseOf: []string{"obs-1"},
					}},
				},
				cfSeq("thinking", "a", "value a", 0.7, 3500),
				cfSeq("thinking", "b", "value b", 0.8, 2000),
			},
			plan:     `[{"action":"merge","dimension":"thinking","key":"survivor","value":"reopened value","absorb":["a","b"],"confidence":0.75,"claim":"short","why":"same"}]`,
			decaySeq: 3500,
			wantEvents: []store.ConfidenceEvent{
				{
					Lens:      "default",
					Dimension: "thinking",
					Key:       "survivor",
					At:        nowStr,
					ClockSeq:  3500,
					From:      0.45, // last version (0.9) decayed from seq 500 to 3500
					To:        0.75,
					Kind:      store.EventCompaction,
				},
			},
			description: "reopen: archived facet re-enters, From is last version decayed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCompactStore{facets: tc.facets, highWater: tc.decaySeq}
			c := compactor(st, tc.plan)
			plans, err := c.Plan(context.Background())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plans) == 0 || len(plans[0].Actions) == 0 {
				t.Fatalf("no plan returned")
			}
			if err := c.Apply(context.Background(), plans, now); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			if !reflect.DeepEqual(st.events, tc.wantEvents) {
				t.Errorf("%s:\nwant events: %+v\ngot events:  %+v", tc.description, tc.wantEvents, st.events)
			}
		})
	}
}

func TestCompactLogsNothingForRetiredOrAbsorbed(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		facets []store.Facet
		plan   string
	}{
		{
			name: "retire",
			facets: []store.Facet{
				cfSeq("thinking", "obsolete", "value", 0.7, 500),
			},
			plan: `[{"action":"retire","dimension":"thinking","key":"obsolete","why":"no longer relevant"}]`,
		},
		{
			name: "absorbed facets",
			facets: []store.Facet{
				cfSeq("thinking", "survivor", mergedValue, 0.9, 3500),
				cfSeq("thinking", "absorbed_a", "value a", 0.7, 500),
				cfSeq("thinking", "absorbed_b", "value b", 0.8, 1000),
			},
			plan: `[{"action":"merge","dimension":"thinking","key":"survivor","value":"` + mergedValue + `","absorb":["absorbed_a","absorbed_b"],"confidence":0.9,"claim":"short","why":"same"}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCompactStore{facets: tc.facets, highWater: 3500}
			c := compactor(st, tc.plan)
			plans, err := c.Plan(context.Background())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if err := c.Apply(context.Background(), plans, now); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			// For retire: expect NO events at all
			// For merge: expect exactly ONE event (the survivor), not one per absorbed facet
			if tc.name == "retire" {
				if len(st.events) != 0 {
					t.Errorf("retire must log nothing; got %d events: %+v", len(st.events), st.events)
				}
			} else {
				if len(st.events) != 1 {
					t.Errorf("merge must log only the survivor; got %d events: %+v", len(st.events), st.events)
				}
				if len(st.events) > 0 && st.events[0].Key != "survivor" {
					t.Errorf("the one event must be for the survivor, got key=%s", st.events[0].Key)
				}
			}
		})
	}
}

func TestCompactSkippedGroupLogsNothing(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	// A merge that absorbs < compactMinAbsorb facets is skipped (the absorbed facets
	// vanished between plan and apply). No event for the survivor: it was not written.
	st := &fakeCompactStore{
		facets: []store.Facet{
			cfSeq("thinking", "survivor", mergedValue, 0.9, 500),
			// The plan says absorb ["a","b"], but only "a" exists in the store — so
			// absorbed count = 1, below compactMinAbsorb (2), and the merge is skipped.
			cfSeq("thinking", "a", "value a", 0.7, 500),
		},
		highWater: 3500,
	}

	// Plan says absorb both "a" and "b", but "b" is missing from the store
	plan := `[{"action":"merge","dimension":"thinking","key":"survivor","value":"` + mergedValue + `","absorb":["a","b"],"confidence":0.9,"claim":"short","why":"same"}]`
	c := compactor(st, plan)
	plans, err := c.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := c.Apply(context.Background(), plans, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(st.events) != 0 {
		t.Errorf("skipped group (absorbed < compactMinAbsorb) must log nothing; got %d events: %+v", len(st.events), st.events)
	}
}
