package store

import (
	"math"
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

// The decay clock has to survive the review write-back, like every other column: the reviewer
// reads the whole profile and rewrites it each fold, so a column dropped on write would reset
// every facet's clock on the next review — which reads as "just confirmed" and silently
// disables decay entirely. Needs a REAL store; an in-memory fake cannot catch a missing column.
func TestDecayClockSurvivesTheWriteBack(t *testing.T) {
	s := lineageStore(t)
	f := lf("default", "thinking", "clocked", "a value")
	f.Versions[0].Confidence = 0.8
	f.Versions[0].ConfidenceSeq = 4242
	if err := s.WriteFacets([]Facet{f}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // three folds' worth of read-modify-write
		all, err := s.ReadFacets()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteFacets(all); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.ReadFacets()
	if len(all) != 1 {
		t.Fatalf("want 1 facet, got %d", len(all))
	}
	cur := all[0].Current()
	if cur.ConfidenceSeq != 4242 {
		t.Errorf("the decay clock was lost across the write-back, got %d", cur.ConfidenceSeq)
	}
	if cur.Confidence != 0.8 {
		t.Errorf("stored confidence should be untouched, got %v", cur.Confidence)
	}
	// With the clock intact, decay actually happens.
	if got := cur.DecayedConfidence(4242 + int64(FacetDecayHalfLifeObs)); got >= 0.8 {
		t.Errorf("a surviving clock should produce decay, got %v", got)
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
	if dc := all[0].Current().DecayedConfidence(got); dc != 0.9 {
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
	if got := v.DecayedConfidence(at); math.Abs(got-0.4) > 0.005 {
		t.Errorf("one half-life should halve 0.8, got %v", got)
	}
	if got := v.DecayedConfidence(1000 + int64(2*FacetDecayHalfLifeObs)); math.Abs(got-0.2) > 0.005 {
		t.Errorf("two half-lives should quarter 0.8, got %v", got)
	}
}

// The floor is load-bearing, not a safety rail: the invalidation rule is explicit that absence
// must NEVER invalidate a facet, only lower confidence. A decay reaching 0 would turn "no
// evidence lately" into "not true", which is the one inference this archive must not make.
func TestDecayNeverPassesTheFloor(t *testing.T) {
	v := cv(0.95, 1)
	for _, elapsed := range []int64{10_000, 100_000, 10_000_000} {
		got := v.DecayedConfidence(1 + elapsed)
		if got < FacetConfidenceFloor {
			t.Errorf("after %d observations confidence fell to %v, below the floor", elapsed, got)
		}
	}
	if got := v.DecayedConfidence(1 + 10_000_000); got != FacetConfidenceFloor {
		t.Errorf("a long-uncited facet should rest exactly at the floor, got %v", got)
	}
}

// A facet already at or under the floor has nothing to decay — and must not be pushed below it.
func TestAFacetAtTheFloorIsUnchanged(t *testing.T) {
	for _, conf := range []float64{0.1, 0.05, 0.0} {
		v := cv(conf, 1)
		if got := v.DecayedConfidence(1_000_000); got != conf {
			t.Errorf("confidence %v should be left alone, got %v", conf, got)
		}
	}
}

// The migration trap, pinned: an unknown clock must mean NO decay, never "infinitely stale".
// The reverse would decay every pre-existing facet to the floor on the first read and re-level
// an entire archive retroactively, which is why the migration backfills the column.
func TestAnUnknownClockMeansNoDecay(t *testing.T) {
	v := cv(0.9, 0)
	if got := v.DecayedConfidence(500_000); got != 0.9 {
		t.Errorf("a facet with no recorded clock must not decay, got %v", got)
	}
}

// Going backwards is not decay. A stale or lower currentSeq (a re-read, a lens whose watermark
// has not caught up) must not inflate or reduce anything.
func TestAClockAtOrBeforeTheStampDoesNotDecay(t *testing.T) {
	v := cv(0.7, 5000)
	for _, at := range []int64{5000, 4999, 0, -1} {
		if got := v.DecayedConfidence(at); got != 0.7 {
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
		got := v.DecayedConfidence(100 + elapsed)
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
	got := v.DecayedConfidence(1337)
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
	typical := v.DecayedConfidence(1 + 1486)
	if typical < 0.5 || typical > 0.8 {
		t.Errorf("after one typical citation gap 0.9 should read 0.5-0.8, got %v", typical)
	}
	stale := v.DecayedConfidence(1 + 10*1486)
	if stale > 0.2 {
		t.Errorf("after ten gaps a facet should be near the floor, got %v", stale)
	}
}
