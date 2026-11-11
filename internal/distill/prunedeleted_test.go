package distill

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// A delete reaches the profile at the start of the next review (pruneDeletedEvidence): the gone
// id is stripped from every facet citing it, and a facet left citing nothing is retired. It must
// cost no model call, which every test here pins with a runner that counts.

func citing(key string, ids ...string) store.Facet {
	return store.Facet{
		Lens: "default", Dimension: "d", Key: key, LastSeen: "2026-01-01T00:00:00Z",
		Versions: []store.FacetVersion{{
			Value: key + " value", ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z",
			Confidence: 0.8, ConfidenceSeq: 1, BecauseOf: ids,
		}},
	}
}

func facetNamed(t *testing.T, s *store.Store, lensName, key string) store.Facet {
	t.Helper()
	all, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets: %v", err)
	}
	for _, f := range all {
		if f.Lens == lensName && f.Key == key {
			return f
		}
	}
	t.Fatalf("no facet %s/%s", lensName, key)
	return store.Facet{}
}

func counting(calls *int, reply *string) MineFunc {
	return func(context.Context, string, string, string) (string, error) { *calls++; return *reply, nil }
}

// pruneOnly runs a review over a lens with nothing new to fold, so only the prune can act.
func pruneOnly(t *testing.T, rs store.ReviewStore, s *store.Store, through int64, now time.Time) {
	t.Helper()
	if err := s.StampReviewLens("default", through); err != nil {
		t.Fatal(err)
	}
	calls, reply := 0, "[]"
	r := &Reviewer{Store: rs, Lenses: defaultLens(), Runner: counting(&calls, &reply)}
	if err := r.Run(context.Background(), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("the prune made %d model calls; it must make none", calls)
	}
}

// Only the ids that are gone are stripped — from every version, not just the current one — and
// only from the lens under review.
func TestPruneStripsOnlyMissingIDs(t *testing.T) {
	s := newStore(t)
	seq := stageObs(t, s, "default", "kept")
	kept := store.ObservationID("sess", "default", "kept")
	f := citing("k", "gone_new", kept)
	f.Versions = append([]store.FacetVersion{{
		Value: "older", ValidFrom: "2025-12-01T00:00:00Z", ValidTo: "2026-01-01T00:00:00Z",
		Confidence: 0.5, BecauseOf: []string{kept, "gone_old"},
	}}, f.Versions...)
	other := citing("k", "gone_old")
	other.Lens = "math"
	writeFacets(t, s, f, other)

	pruneOnly(t, s, s, seq, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))

	got := facetNamed(t, s, "default", "k")
	for i, v := range got.Versions {
		if !reflect.DeepEqual(v.BecauseOf, []string{kept}) {
			t.Errorf("version %d cites %v, want only the surviving %s", i, v.BecauseOf, kept)
		}
	}
	if !got.Active() {
		t.Error("a facet that still cites a live observation must stay active")
	}
	if m := facetNamed(t, s, "math", "k"); !reflect.DeepEqual(m.Versions[0].BecauseOf, []string{"gone_old"}) {
		t.Errorf("a lens not under review was touched: %v", m.Versions[0].BecauseOf)
	}
	assertLog(t, s, nil)
}

// A facet whose current version cited something and now cites nothing is retired, and the log
// says so. One that never cited anything is left exactly as it was, and one compaction already
// archived is stripped but not retired again.
func TestPruneRetiresAFacetLeftCitingNothing(t *testing.T) {
	s := newStore(t)
	stageObs(t, s, "default", "unrelated")
	archived := citing("archived", "gone")
	archived.ArchivedAt, archived.MergedInto, archived.ArchiveReason = "2026-02-01T00:00:00Z", "d/orphan", "same trait"
	writeFacets(t, s, citing("orphan", "gone"), citing("uncited"), archived)
	before := facetNamed(t, s, "default", "uncited")

	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	nowStr := now.Format(time.RFC3339)
	const through = 3001 // a half-life past the facet's clock, so From must be the decayed value
	pruneOnly(t, s, s, through, now)

	orphan := facetNamed(t, s, "default", "orphan")
	if orphan.ArchivedAt != nowStr || orphan.MergedInto != "" || orphan.ArchiveReason != retiredReason {
		t.Errorf("orphan: archived_at=%q merged_into=%q reason=%q, want retired outright at %s",
			orphan.ArchivedAt, orphan.MergedInto, orphan.ArchiveReason, nowStr)
	}
	if cur := orphan.Current(); cur == nil || cur.Value != "orphan value" || len(cur.BecauseOf) != 0 {
		t.Errorf("orphan's history must be kept with the dead id stripped, got %+v", orphan.Versions)
	}
	if got := facetNamed(t, s, "default", "uncited"); !reflect.DeepEqual(got, before) {
		t.Errorf("a facet that never cited anything was changed:\n got %+v\nwant %+v", got, before)
	}
	a := facetNamed(t, s, "default", "archived")
	if a.ArchivedAt != "2026-02-01T00:00:00Z" || a.MergedInto != "d/orphan" || len(a.Versions[0].BecauseOf) != 0 {
		t.Errorf("an archived facet keeps its archive and loses only the dead id, got %+v", a)
	}
	from := (&store.FacetVersion{Confidence: 0.8, ConfidenceSeq: 1}).DecayedConfidence(through)
	assertLog(t, s, []store.ConfidenceEvent{
		{Lens: "default", Dimension: "d", Key: "orphan", At: nowStr, ClockSeq: 1, From: from, To: 0.8, Kind: store.EventRetire},
	})
}

// countWrites is a real store that counts facet writes.
type countWrites struct {
	*store.Store
	n *int
}

func (c countWrites) WriteFacetsLogged(fs []store.Facet, evs []store.ConfidenceEvent) error {
	*c.n++
	return c.Store.WriteFacetsLogged(fs, evs)
}

// Every review runs the prune on every lens, so with nothing deleted it must not rewrite the
// facet table.
func TestPruneWritesNothingWhenNothingIsMissing(t *testing.T) {
	s := newStore(t)
	seq := stageObs(t, s, "default", "live")
	writeFacets(t, s, citing("k", store.ObservationID("sess", "default", "live")), citing("uncited"))
	writes := 0
	pruneOnly(t, countWrites{Store: s, n: &writes}, s, seq, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	if writes != 0 {
		t.Errorf("%d facet writes with nothing missing, want 0", writes)
	}
}

// The whole path: a delete re-reads nothing (it used to clear the review cursor and refold the
// lens), the next review retires the facet that rested only on the deleted observation and keeps
// the one with other evidence, and a later review that re-asserts the key brings it back.
func TestDeleteThenReviewRetiresWithoutRefolding(t *testing.T) {
	s := newStore(t)
	stageObs(t, s, "default", "x")
	stageObs(t, s, "default", "y")
	x, y := store.ObservationID("sess", "default", "x"), store.ObservationID("sess", "default", "y")
	calls, reply := 0, mustJSON([]reviewedFacet{
		{Dimension: "d", Key: "only", Value: "rests on x", Confidence: 0.6, BecauseOf: []string{x}},
		{Dimension: "d", Key: "both", Value: "rests on x and y", Confidence: 0.6, BecauseOf: []string{x, y}},
	})
	r := &Reviewer{Store: s, Lenses: defaultLens(), Runner: counting(&calls, &reply)}
	if err := r.Run(context.Background(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil || calls != 1 {
		t.Fatalf("first review: calls=%d err=%v", calls, err)
	}

	if deleted, err := s.DeleteObservation(x); err != nil || !deleted {
		t.Fatalf("DeleteObservation: deleted=%v err=%v", deleted, err)
	}
	calls = 0
	if err := r.Run(context.Background(), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("the review after a delete made %d model calls: it refolded the lens", calls)
	}
	if f := facetNamed(t, s, "default", "only"); f.Active() {
		t.Error("the facet resting only on the deleted observation must be retired")
	}
	if f := facetNamed(t, s, "default", "both"); !f.Active() || !reflect.DeepEqual(f.Current().BecauseOf, []string{y}) {
		t.Errorf("the facet with other evidence must stay active citing only %s, got %+v", y, f)
	}

	stageObs(t, s, "default", "z")
	z := store.ObservationID("sess", "default", "z")
	reply = mustJSON([]reviewedFacet{{Dimension: "d", Key: "only", Value: "rests on x", Confidence: 0.6, BecauseOf: []string{z}}})
	if err := r.Run(context.Background(), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if f := facetNamed(t, s, "default", "only"); !f.Active() || !reflect.DeepEqual(f.Current().BecauseOf, []string{z}) {
		t.Errorf("re-asserted, the retired facet must come back citing %s, got %+v", z, f)
	}
}
