package distill

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// The confidence log is only worth charting if every row is exactly what happened: the kind of
// change, the value the step started FROM (the decayed one the model saw), where it landed, the
// clock it was stamped at, and the facet it was actually written under. Every assertion below
// compares whole events, because a check on count and kind alone passed real bugs before.

// stageObs appends one uniquely-identified observation and returns its seq — the clock a fold
// over it stamps. stageObsForReview reuses one id, so a second call would be ignored.
func stageObs(t *testing.T, s *store.Store, lensName, text string) int64 {
	t.Helper()
	if err := s.AppendObservations([]store.Observation{{
		ID: obsID("sess", lensName, text), TS: "2026-11-01T00:00:00Z", Session: "sess",
		Lens: lensName, Dimension: "d", Observation: text, Poignancy: 5,
	}}); err != nil {
		t.Fatalf("AppendObservations: %v", err)
	}
	seq, err := s.ObservationHighWater()
	if err != nil {
		t.Fatalf("ObservationHighWater: %v", err)
	}
	return seq
}

// answering is a review runner whose reply the test sets before each fold.
func answering(reply *string) MineFunc {
	return func(context.Context, string, string, string) (string, error) { return *reply, nil }
}

func defaultLens() []*lens.Lens { return []*lens.Lens{{Name: "default", Review: "REVIEW"}} }

func readLog(t *testing.T, s *store.Store) []store.ConfidenceEvent {
	t.Helper()
	log, err := s.ReadConfidenceLog()
	if err != nil {
		t.Fatalf("ReadConfidenceLog: %v", err)
	}
	return log
}

func assertLog(t *testing.T, s *store.Store, want []store.ConfidenceEvent) {
	t.Helper()
	if got := readLog(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("confidence log:\n got %+v\nwant %+v", got, want)
	}
}

func writeFacets(t *testing.T, s *store.Store, fs ...store.Facet) {
	t.Helper()
	if err := s.WriteFacets(fs); err != nil {
		t.Fatalf("WriteFacets: %v", err)
	}
}

func openFacet(dim, key, value string, conf float64, clock int64) store.Facet {
	return store.Facet{
		Lens: "default", Dimension: dim, Key: key, LastSeen: "2026-01-01T00:00:00Z",
		Versions: []store.FacetVersion{{
			Value: value, ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z",
			Confidence: conf, ConfidenceSeq: clock, BecauseOf: []string{"o1"},
		}},
	}
}

// One fold per branch, through ONE Reviewer across five Runs — so an event that is not cleared
// once written would be written again by every later fold and show up as a duplicate row.
func TestReviewLogsEachBranch(t *testing.T) {
	s := newStore(t)
	gone := openFacet("d", "gone", "used to pair", 0.6, 0)
	gone.Versions[0].ValidTo = "2026-01-02T00:00:00Z" // every version closed → the next assertion reopens
	writeFacets(t, s, gone)

	var reply string
	r := &Reviewer{Store: s, Lenses: defaultLens(), Runner: answering(&reply)}
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	steps := []struct {
		reply string
		want  func(at string, seq int64) store.ConfidenceEvent
	}{
		{mustJSON([]reviewedFacet{{Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.5}}),
			func(at string, seq int64) store.ConfidenceEvent {
				return store.ConfidenceEvent{Lens: "default", Dimension: "d", Key: "k", At: at, ClockSeq: seq, From: 0, To: 0.5, Kind: store.EventNew}
			}},
		{facetReply("d", "k", "prefers Go"), // 0.9 asked, one step allowed
			func(at string, seq int64) store.ConfidenceEvent {
				return store.ConfidenceEvent{Lens: "default", Dimension: "d", Key: "k", At: at, ClockSeq: seq, From: 0.5, To: 0.6, Kind: store.EventReinforce}
			}},
		{mustJSON([]reviewedFacet{{Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.2}}),
			func(at string, seq int64) store.ConfidenceEvent {
				return store.ConfidenceEvent{Lens: "default", Dimension: "d", Key: "k", At: at, ClockSeq: seq, From: 0.6, To: 0.5, Kind: store.EventDowngrade}
			}},
		{mustJSON([]reviewedFacet{{Dimension: "d", Key: "k", Value: "prefers Rust", Confidence: 0.7, Contradicts: true}}),
			func(at string, seq int64) store.ConfidenceEvent {
				return store.ConfidenceEvent{Lens: "default", Dimension: "d", Key: "k", At: at, ClockSeq: seq, From: 0.5, To: 0.7, Kind: store.EventRewrite}
			}},
		{mustJSON([]reviewedFacet{{Dimension: "d", Key: "gone", Value: "pairs again", Confidence: 0.4}}),
			func(at string, seq int64) store.ConfidenceEvent {
				return store.ConfidenceEvent{Lens: "default", Dimension: "d", Key: "gone", At: at, ClockSeq: seq, From: 0.6, To: 0.4, Kind: store.EventReopen}
			}},
	}
	var want []store.ConfidenceEvent
	for i, st := range steps {
		seq := stageObs(t, s, "default", "fold "+string(rune('a'+i)))
		now := t0.Add(time.Duration(i) * time.Hour)
		reply = st.reply
		if err := r.Run(context.Background(), now); err != nil {
			t.Fatalf("fold %d: %v", i, err)
		}
		want = append(want, st.want(now.Format(time.RFC3339), seq))
	}
	assertLog(t, s, want)
}

// Re-asserting a facet exactly as it stands still went through the step, so it is logged —
// as a reinforcement that moved nothing, not silently skipped.
func TestReviewLogsUnchangedAssertionAsReinforce(t *testing.T) {
	s := newStore(t)
	writeFacets(t, s, openFacet("d", "k", "prefers Go", 0.9, 0))
	reply := facetReply("d", "k", "prefers Go") // the stored value and confidence, unchanged
	r := &Reviewer{Store: s, Lenses: defaultLens(), Runner: answering(&reply)}
	seq := stageObs(t, s, "default", "again")
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := r.Run(context.Background(), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertLog(t, s, []store.ConfidenceEvent{
		{Lens: "default", Dimension: "d", Key: "k", At: now.Format(time.RFC3339), ClockSeq: seq, From: 0.9, To: 0.9, Kind: store.EventReinforce},
	})
}

// A malformed assertion changes nothing (wellFormed), so it must not appear in the log either:
// a row for it would chart a confidence change the facet never had.
func TestReviewLogsOneEventPerAcceptedFacet(t *testing.T) {
	s := newStore(t)
	writeFacets(t, s, openFacet("d", "k", "prefers Go", 0.9, 0))
	reply := mustJSON([]reviewedFacet{
		{Dimension: "d", Key: "k", Value: "", Confidence: 0.3, Contradicts: true}, // empty value
		{Dimension: "d", Key: "", Value: "keyless", Confidence: 0.3},              // empty key
		{Dimension: "d", Key: "fresh", Value: "a new trait", Confidence: 0.7},
	})
	r := &Reviewer{Store: s, Lenses: defaultLens(), Runner: answering(&reply)}
	seq := stageObs(t, s, "default", "mixed")
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := r.Run(context.Background(), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertLog(t, s, []store.ConfidenceEvent{
		{Lens: "default", Dimension: "d", Key: "fresh", At: now.Format(time.RFC3339), ClockSeq: seq, From: 0, To: 0.7, Kind: store.EventNew},
	})
}

// An assertion on a merged-away facet is applied to its survivor (resolveArchived), here in a
// DIFFERENT dimension. The row must name the facet whose confidence actually moved; logged
// under the name the model used, the chart would show a change on an archived facet and none
// on the survivor that really changed.
func TestReviewLogsRedirectUnderSurvivor(t *testing.T) {
	s := newStore(t)
	old := openFacet("d", "old", "old wording", 0.9, 0)
	old.ArchivedAt = "2026-10-01T00:00:00Z"
	old.MergedInto = store.FacetRef("e", "survivor")
	writeFacets(t, s, old, openFacet("e", "survivor", "merged wording", 0.5, 0))
	reply := facetReply("d", "old", "old wording")
	r := &Reviewer{Store: s, Lenses: defaultLens(), Runner: answering(&reply)}
	seq := stageObs(t, s, "default", "redirected")
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := r.Run(context.Background(), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertLog(t, s, []store.ConfidenceEvent{
		{Lens: "default", Dimension: "e", Key: "survivor", At: now.Format(time.RFC3339), ClockSeq: seq, From: 0.5, To: 0.6, Kind: store.EventReinforce},
	})
}

// failLoggedWriteOnce is a real store whose first logged write fails.
type failLoggedWriteOnce struct {
	*store.Store
	failed *bool
}

func (f failLoggedWriteOnce) WriteFacetsLogged(fs []store.Facet, evs []store.ConfidenceEvent) error {
	if !*f.failed {
		*f.failed = true
		return errors.New("disk full")
	}
	return f.Store.WriteFacetsLogged(fs, evs)
}

// Events describe the facets of ONE write. A failed write commits neither, so its events must
// be dropped with it — carried into the next write, they would log a change that was never
// stored, at the wrong time, next to the retry's real one.
func TestReviewLogsNothingFromAFailedWrite(t *testing.T) {
	s := newStore(t)
	var reply string
	failed := false
	r := &Reviewer{Store: failLoggedWriteOnce{Store: s, failed: &failed}, Lenses: defaultLens(), Runner: answering(&reply)}
	seq := stageObs(t, s, "default", "retried")

	reply = mustJSON([]reviewedFacet{{Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.5}})
	if err := r.Run(context.Background(), time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("the failed write must surface")
	}
	reply = mustJSON([]reviewedFacet{{Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.7}})
	now := time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)
	if err := r.Run(context.Background(), now); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertLog(t, s, []store.ConfidenceEvent{
		{Lens: "default", Dimension: "d", Key: "k", At: now.Format(time.RFC3339), ClockSeq: seq, From: 0, To: 0.7, Kind: store.EventNew},
	})
}

// From is the DECAYED confidence — what the model was shown and what the step started from —
// on every branch. With the stored value instead, a facet that decayed 0.9→0.45 and was then
// re-judged at 0.5 would be charted as a downgrade 0.9→0.5 when it was a reinforcement.
func TestApplyFacetLogsFromTheDecayedConfidence(t *testing.T) {
	later := int64(establishedClock) + int64(store.FacetDecayHalfLifeObs) // one half-life: 0.9 → 0.45
	cases := []struct {
		name   string
		facet  store.Facet
		assert reviewedFacet
		want   store.ConfidenceEvent
	}{
		{"in place", clockedFacet(false),
			reviewedFacet{Dimension: "d", Key: "k", Value: "prefers Go", Confidence: 0.5},
			store.ConfidenceEvent{Lens: "L", Dimension: "d", Key: "k", At: applyNow, ClockSeq: later, From: 0.45, To: 0.5, Kind: store.EventReinforce}},
		{"rewrite", clockedFacet(false),
			reviewedFacet{Dimension: "d", Key: "k", Value: "prefers Rust now", Confidence: 0.7, Contradicts: true},
			store.ConfidenceEvent{Lens: "L", Dimension: "d", Key: "k", At: applyNow, ClockSeq: later, From: 0.45, To: 0.7, Kind: store.EventRewrite}},
		{"reopen", clockedFacet(true),
			reviewedFacet{Dimension: "d", Key: "k", Value: "prefers Go again", Confidence: 0.7},
			store.ConfidenceEvent{Lens: "L", Dimension: "d", Key: "k", At: applyNow, ClockSeq: later, From: 0.45, To: 0.7, Kind: store.EventReopen}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rv := &Reviewer{}
			if !rv.applyFacet(indexFacets([]store.Facet{tc.facet}), "L", tc.assert, applyNow, later) {
				t.Fatal("the assertion was rejected, so nothing below is exercised")
			}
			if got := rv.takeEvents(); !reflect.DeepEqual(got, []store.ConfidenceEvent{tc.want}) {
				t.Errorf("events:\n got %+v\nwant %+v", got, []store.ConfidenceEvent{tc.want})
			}
		})
	}
}

// The long-arc pass has no clock (seq 0). A new arc is logged without one; an arc landing on an
// established facet is logged at the clock that facet KEPT (knownClock), not at 0 — the log
// must say what the version carries, not what the caller happened to pass.
func TestEmergentLogsWithKnownClock(t *testing.T) {
	const key = "abstracts_to_structure"
	verify := func(context.Context, string, string, string) (string, error) {
		return `[{"dimension":"thinking","key":"` + key + `","value":"reflexively abstracts","confidence":0.8,"contradicts_prior":false}]`, nil
	}
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	at := now.Format(time.RFC3339)

	t.Run("new arc", func(t *testing.T) {
		s := newStore(t)
		seedArc(t, s, "a", 6, []float32{1, 0, 0})
		if err := emergentReviewer(s, verify).RunFull(context.Background(), now); err != nil {
			t.Fatalf("RunFull: %v", err)
		}
		assertLog(t, s, []store.ConfidenceEvent{
			{Lens: "math", Dimension: "thinking", Key: key, At: at, ClockSeq: 0, From: 0, To: 0.8, Kind: store.EventNew},
		})
	})
	t.Run("existing facet", func(t *testing.T) {
		s := newStore(t)
		f := openFacet("thinking", key, "reflexively abstracts", 0.5, establishedClock)
		f.Lens = "math"
		writeFacets(t, s, f)
		seedArc(t, s, "a", 6, []float32{1, 0, 0})
		if err := emergentReviewer(s, verify).RunFull(context.Background(), now); err != nil {
			t.Fatalf("RunFull: %v", err)
		}
		assertLog(t, s, []store.ConfidenceEvent{
			{Lens: "math", Dimension: "thinking", Key: key, At: at, ClockSeq: establishedClock, From: 0.5, To: 0.6, Kind: store.EventReinforce},
		})
	})
}

// Claims rewrite the profile (WriteFacets is a full replace) but change no confidence, so the
// claim pass must leave the log exactly as it found it — no rows added, none lost to the replace.
func TestClaimPassLogsNothing(t *testing.T) {
	s := newStore(t)
	facets := []store.Facet{openFacet("d", "a", "a long value", 0.8, 3), openFacet("d", "b", "another value", 0.6, 4)}
	seeded := []store.ConfidenceEvent{
		{Lens: "default", Dimension: "d", Key: "a", At: "2026-11-01T00:00:00Z", ClockSeq: 3, From: 0, To: 0.8, Kind: store.EventNew},
		{Lens: "default", Dimension: "d", Key: "b", At: "2026-11-02T00:00:00Z", ClockSeq: 4, From: 0.7, To: 0.6, Kind: store.EventDowngrade},
	}
	if err := s.WriteFacetsLogged(facets, seeded); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := readLog(t, s)

	c := &Claimer{Store: s, Lenses: defaultLens(), Runner: func(context.Context, string, string, string) (string, error) {
		return `[{"dimension":"d","key":"a","claim":"Claim A."},{"dimension":"d","key":"b","claim":"Claim B."}]`, nil
	}}
	if n := c.Run(context.Background(), c.Lenses[0]); n != 2 {
		t.Fatalf("the claim pass wrote %d claims, want 2 — without a write this test proves nothing", n)
	}
	if after := readLog(t, s); !reflect.DeepEqual(after, before) {
		t.Errorf("the claim pass changed the confidence log:\nbefore %+v\n after %+v", before, after)
	}
}
