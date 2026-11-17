package store

import (
	"math"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// Decay-on-absence (the second half of #173). Confidence is "how sure we were when the evidence
// last arrived"; what a reader should see accounts for the observations that have gone by since
// without confirming it.
//
// The clock is OBSERVATIONS, not time or folds: decay must mean "evidence was possible and did
// not arrive". Wall-clock would punish a holiday; fold count is configurable (ReviewMaxChars) so
// a per-fold rate silently changes meaning when that setting changes.

func cv(conf float64, seq int64) FacetVersion {
	return FacetVersion{Value: "v", Confidence: conf, ConfidenceSeq: seq}
}

// globalDecay is the formula DecayedConfidence used before the clock was per lens: elapsed
// counted on the archive's global seq, every lens's observations included. Kept verbatim so the
// tests below can show where the two agree (a single-lens archive) and where they part.
func globalDecay(v FacetVersion, currentSeq int64) float64 {
	if v.ConfidenceSeq <= 0 || currentSeq <= v.ConfidenceSeq {
		return v.Confidence
	}
	if v.Confidence <= FacetConfidenceFloor {
		return v.Confidence // already at or under the floor; decay has nothing to do
	}
	elapsed := float64(currentSeq - v.ConfidenceSeq)
	decayed := v.Confidence * math.Pow(0.5, elapsed/FacetDecayHalfLifeObs)
	if decayed < FacetConfidenceFloor {
		return FacetConfidenceFloor
	}
	return math.Round(decayed*100) / 100
}

// lensArchive is a fresh archive whose observation seq i+1 belongs to lenses[i], written in one
// append so the seqs are dense.
func lensArchive(t *testing.T, lenses ...string) *Store {
	t.Helper()
	s := tempStore(t)
	obs := make([]Observation, len(lenses))
	for i, l := range lenses {
		obs[i] = Observation{ID: "o" + strconv.Itoa(i+1), Lens: l, Observation: "seen"}
	}
	if err := s.AppendObservations(obs); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLensClockIsOneLensSeqsInOrder(t *testing.T) {
	s := lensArchive(t, "default", "math", "default", "math", "default", "default")
	def, err := s.LensClock("default")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(def, LensClock{1, 3, 5, 6}) {
		t.Errorf("default's clock = %v, want [1 3 5 6]", def)
	}
	if m, _ := s.LensClock("math"); !reflect.DeepEqual(m, LensClock{2, 4}) {
		t.Errorf("math's clock = %v, want [2 4]", m)
	}
	none, err := s.LensClock("never-mined")
	if err != nil || len(none) != 0 {
		t.Errorf("a lens with no observations has an empty clock, got %v (err %v)", none, err)
	}
	if got := def.Through(4); !reflect.DeepEqual(got, LensClock{1, 3}) {
		t.Errorf("Through(4) = %v, want [1 3]", got)
	}
	if got := def.Through(6); !reflect.DeepEqual(got, def) {
		t.Errorf("Through(6) = %v, want the whole clock %v", got, def)
	}
	if got := def.Through(0); len(got) != 0 {
		t.Errorf("Through(0) = %v, want empty", got)
	}
	v := cv(0.9, 5)
	if got := v.DecayedConfidence(none); got != 0.9 {
		t.Errorf("an empty clock has nothing elapsed, got %v", got)
	}
	if got := v.DecayedConfidence(nil); got != 0.9 {
		t.Errorf("a nil clock has nothing elapsed, got %v", got)
	}
}

// On a single-lens, gap-free archive the per-lens count IS the old n - ConfidenceSeq, so an
// archive with one lens reads exactly as it did.
func TestASingleLensArchiveDecaysAsBefore(t *testing.T) {
	s := lensArchive(t, slices.Repeat([]string{"default"}, 1000)...)
	clock, err := s.LensClock("default")
	if err != nil {
		t.Fatal(err)
	}
	for cs, want := range map[int64]float64{1: 0.71, 400: 0.78, 999: 0.9, 1000: 0.9} {
		v := cv(0.9, cs)
		if got := v.DecayedConfidence(clock); got != want || got != globalDecay(v, 1000) {
			t.Errorf("ConfidenceSeq %d: got %v, want %v (the global formula gives %v)", cs, got, want, globalDecay(v, 1000))
		}
	}
}

// A review folds only its own lens, so a facet in a lens holding 2/11 of the observations was
// aged by the 9/11 it could never have been cited in: 0.29 where its own clock reads 0.73.
func TestAMinorityLensDecaysOnItsOwnClock(t *testing.T) {
	s := lensArchive(t, append([]string{"math"}, slices.Repeat(append(slices.Repeat([]string{"default"}, 9), "math", "math"), 443)...)...)
	mathC, err := s.LensClock("math")
	if err != nil {
		t.Fatal(err)
	}
	defC, err := s.LensClock("default")
	if err != nil {
		t.Fatal(err)
	}
	if len(mathC) != 887 || len(defC) != 3987 {
		t.Errorf("setup: math %d, default %d observations, want 887 and 3987", len(mathC), len(defC))
	}
	m := cv(0.9, 1)
	if got := m.DecayedConfidence(mathC); got != 0.73 {
		t.Errorf("math facet on math's clock = %v, want 0.73", got)
	}
	if got := globalDecay(m, 4874); got != 0.29 {
		t.Errorf("math facet on the global clock = %v, want 0.29", got)
	}
	d := cv(0.9, 10)
	if got := d.DecayedConfidence(defC); got != 0.36 {
		t.Errorf("default facet on default's clock = %v, want 0.36", got)
	}
	if got := globalDecay(d, 4874); got != 0.29 {
		t.Errorf("default facet on the global clock = %v, want 0.29", got)
	}
}

// A lens nothing has been mined into since the facet's stamp had no chance to cite it, however
// busy the other lenses were.
func TestALensWithNothingSinceItsStampDoesNotDecay(t *testing.T) {
	s := lensArchive(t, append([]string{"math"}, slices.Repeat([]string{"default"}, 1000)...)...)
	mathC, err := s.LensClock("math")
	if err != nil {
		t.Fatal(err)
	}
	v := cv(0.9, 1)
	if got := v.DecayedConfidence(mathC); got != 0.9 {
		t.Errorf("a facet with no own-lens observations since its stamp must read as stored, got %v", got)
	}
	if got := globalDecay(v, 1001); got != 0.71 {
		t.Errorf("the global clock would have read %v, want 0.71", got)
	}
}

// The migration trap, pinned end to end. A facet that predates the column must be backfilled to
// the CURRENT watermark — treated as confirmed as of the upgrade. Leaving it at 0 would date it
// to the beginning of time; combined with any reading that treats 0 as "stale", an entire
// archive would re-level to the floor on first read.
func TestMigrationBackfillsTheDecayClock(t *testing.T) {
	s := lineageStore(t)
	// Observations give the watermark the backfill should pick up.
	if err := s.AppendObservations([]Observation{
		{ID: "o1", Lens: "default", Observation: "first"},
		{ID: "o2", Lens: "default", Observation: "second"},
	}); err != nil {
		t.Fatal(err)
	}
	f := lf("default", "thinking", "pre_existing", "a value")
	f.Versions[0].Confidence = 0.9
	if err := s.WriteFacets([]Facet{f}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-v12 row: clear the clock the way an un-migrated database would have it.
	if _, err := s.db.Exec(`UPDATE facet_versions SET confidence_seq = 0`); err != nil {
		t.Fatal(err)
	}
	// Re-run the migration step on this database.
	tx, err := beginImmediate(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`ALTER TABLE facet_versions RENAME COLUMN confidence_seq TO confidence_seq_old`); err != nil {
		tx.Rollback()
		t.Skipf("this SQLite build cannot rename a column: %v", err)
	}
	if err := addFacetConfidenceSeqColumn(tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	all, _ := s.ReadFacets()
	if len(all) != 1 {
		t.Fatalf("want 1 facet, got %d", len(all))
	}
	got := all[0].Current().ConfidenceSeq
	if got <= 0 {
		t.Fatalf("the migration must backfill the clock, got %d — every facet would read as "+
			"infinitely stale", got)
	}
	// And with that clock, nothing decays yet: the archive is not re-levelled retroactively.
	if dc := all[0].Current().DecayedConfidence(ContiguousClock(got)); dc != 0.9 {
		t.Errorf("a freshly migrated facet must not already be decayed, got %v", dc)
	}
}

// The v12->v13 repair: facet versions that compaction wrote with no decay clock (confidence_seq
// 0, which reads as "never decays") get one, and versions the reviewer stamped are left alone.
// Driven through migrate() itself from a database stamped v12, because the repair is version-
// gated and a test calling the function directly would not prove the gate is wired.
func TestMigrationRestartsClocksCompactionFroze(t *testing.T) {
	s := lineageStore(t)
	if err := s.AppendObservations([]Observation{
		{ID: "o1", Lens: "default", Observation: "first"},
		{ID: "o2", Lens: "default", Observation: "second"},
		{ID: "o3", Lens: "default", Observation: "third"},
	}); err != nil {
		t.Fatal(err)
	}
	frozen := lf("default", "thinking", "compacted", "a value compaction wrote")
	frozen.Versions[0].Confidence = 0.9
	frozen.Versions[0].ConfidenceSeq = 0 // what v0.9.0's compaction wrote
	reviewed := lf("default", "thinking", "reviewed", "a value the reviewer stamped")
	reviewed.Versions[0].Confidence = 0.9
	reviewed.Versions[0].ConfidenceSeq = 2
	if err := s.WriteFacets([]Facet{frozen, reviewed}); err != nil {
		t.Fatal(err)
	}
	var watermark int64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM observations`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version=12`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("migrate from v12: %v", err)
	}

	got := map[string]int64{}
	all, _ := s.ReadFacets()
	for i := range all {
		got[all[i].Key] = all[i].Current().ConfidenceSeq
	}
	if got["compacted"] != watermark {
		t.Errorf("a compaction-frozen clock must restart at the watermark %d, got %d — the facet "+
			"would never decay", watermark, got["compacted"])
	}
	if got["reviewed"] != 2 {
		t.Errorf("a clock the reviewer stamped must be left alone, got %d (want 2) — the repair "+
			"would be handing out free confidence refreshes", got["reviewed"])
	}

	// Once out of v12 the repair must never run again: on a later upgrade a 0 would no longer mean
	// "frozen by v0.9.0", and re-stamping it would silently reset a clock.
	tx, err := beginImmediate(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE facet_versions SET confidence_seq = 0`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := repairCompactionDecayClock(tx, 13); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	var stillZero int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM facet_versions WHERE confidence_seq = 0`).Scan(&stillZero); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Rollback()
	if stillZero == 0 {
		t.Error("the repair ran on a database already at v13; it must be gated to the v12 upgrade")
	}
}

func TestDecayHalvesOverTheHalfLife(t *testing.T) {
	v := cv(0.8, 1000)
	at := 1000 + int64(FacetDecayHalfLifeObs)
	if got := v.DecayedConfidence(ContiguousClock(at)); math.Abs(got-0.4) > 0.005 {
		t.Errorf("one half-life should halve 0.8, got %v", got)
	}
	if got := v.DecayedConfidence(ContiguousClock(1000 + int64(2*FacetDecayHalfLifeObs))); math.Abs(got-0.2) > 0.005 {
		t.Errorf("two half-lives should quarter 0.8, got %v", got)
	}
}

// The floor is load-bearing, not a safety rail: the invalidation rule is explicit that absence
// must NEVER invalidate a facet, only lower confidence. A decay reaching 0 would turn "no
// evidence lately" into "not true", which is the one inference this archive must not make.
func TestDecayNeverPassesTheFloor(t *testing.T) {
	v := cv(0.95, 1)
	for _, elapsed := range []int64{10_000, 100_000, 1_000_000} {
		got := v.DecayedConfidence(ContiguousClock(1 + elapsed))
		if got < FacetConfidenceFloor {
			t.Errorf("after %d observations confidence fell to %v, below the floor", elapsed, got)
		}
	}
	if got := v.DecayedConfidence(ContiguousClock(1 + 1_000_000)); got != FacetConfidenceFloor {
		t.Errorf("a long-uncited facet should rest exactly at the floor, got %v", got)
	}
}

// A facet already at or under the floor has nothing to decay — and must not be pushed below it.
func TestAFacetAtTheFloorIsUnchanged(t *testing.T) {
	for _, conf := range []float64{0.1, 0.05, 0.0} {
		v := cv(conf, 1)
		if got := v.DecayedConfidence(ContiguousClock(1_000_000)); got != conf {
			t.Errorf("confidence %v should be left alone, got %v", conf, got)
		}
	}
}

// The migration trap, pinned: an unknown clock must mean NO decay, never "infinitely stale".
// The reverse would decay every pre-existing facet to the floor on the first read and re-level
// an entire archive retroactively, which is why the migration backfills the column.
func TestAnUnknownClockMeansNoDecay(t *testing.T) {
	v := cv(0.9, 0)
	if got := v.DecayedConfidence(ContiguousClock(500_000)); got != 0.9 {
		t.Errorf("a facet with no recorded clock must not decay, got %v", got)
	}
}

// Going backwards is not decay. A stale or lower currentSeq (a re-read, a lens whose watermark
// has not caught up) must not inflate or reduce anything.
func TestAClockAtOrBeforeTheStampDoesNotDecay(t *testing.T) {
	v := cv(0.7, 5000)
	for _, at := range []int64{5000, 4999, 0, -1} {
		if got := v.DecayedConfidence(ContiguousClock(at)); got != 0.7 {
			t.Errorf("currentSeq %d should leave confidence alone, got %v", at, got)
		}
	}
}

// Decay is monotone in elapsed observations: more missed chances to be confirmed can only lower
// it. A non-monotone curve would make the number unreadable.
func TestDecayIsMonotone(t *testing.T) {
	v := cv(0.9, 100)
	prev := 1.0
	for elapsed := int64(0); elapsed < 20_000; elapsed += 500 {
		got := v.DecayedConfidence(ContiguousClock(100 + elapsed))
		if got > prev {
			t.Fatalf("confidence rose with elapsed %d: %v after %v", elapsed, got, prev)
		}
		prev = got
	}
}

// Rounded to two decimals, like every other confidence write: the archive should not store or
// print precision the judgment does not have.
func TestDecayedConfidenceIsRoundedToTwoDecimals(t *testing.T) {
	v := cv(0.87, 1000)
	got := v.DecayedConfidence(ContiguousClock(1337))
	if got != math.Round(got*100)/100 {
		t.Errorf("decayed confidence should be 2dp, got %v", got)
	}
}

// Grounding the default rate in the measured archive, so a future change to the constant has to
// confront what it does to a real profile: a facet is cited about once every 1,486 observations
// in the `default` lens, so after one typical gap it should read as modestly less certain —
// visibly uncertain, not alarming — and a facet uncited for many gaps should reach the floor.
func TestTheDefaultRateIsSaneOnMeasuredGaps(t *testing.T) {
	v := cv(0.9, 0)
	v.ConfidenceSeq = 1 // a known clock
	typical := v.DecayedConfidence(ContiguousClock(1 + 1486))
	if typical < 0.5 || typical > 0.8 {
		t.Errorf("after one typical citation gap 0.9 should read 0.5-0.8, got %v", typical)
	}
	stale := v.DecayedConfidence(ContiguousClock(1 + 10*1486))
	if stale > 0.2 {
		t.Errorf("after ten gaps a facet should be near the floor, got %v", stale)
	}
}
