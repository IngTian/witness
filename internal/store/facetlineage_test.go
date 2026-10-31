package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The structural half of facet history: archived_at + merged_into. The subtlest failure here
// is not losing a column on write — it is losing it on the WRITE-BACK, because the reviewer
// reads the whole profile and rewrites it on every fold. A dropped column would not corrupt
// anything at compaction time; it would quietly un-compact the profile at the next review.

func lineageStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func lf(lens, dim, key, value string) Facet {
	return Facet{
		Lens: lens, Dimension: dim, Key: key, LastSeen: "2026-10-01T00:00:00Z",
		Versions: []FacetVersion{{
			Value: value, ValidFrom: "2026-10-01T00:00:00Z", RecordedAt: "2026-10-01T00:00:00Z",
			Confidence: 0.7, BecauseOf: []string{"obs-1"},
		}},
	}
}

// The delayed-data-loss case. If WriteFacets drops these columns, compaction appears to work
// and is silently reverted by the next review.
func TestArchiveStateSurvivesTheReviewWriteBack(t *testing.T) {
	s := lineageStore(t)
	absorbed := lf("default", "thinking", "absorbed", "narrow value")
	absorbed.ArchivedAt = "2026-10-02T00:00:00Z"
	absorbed.MergedInto = "thinking/survivor"
	if err := s.WriteFacets([]Facet{lf("default", "thinking", "survivor", "broad value"), absorbed}); err != nil {
		t.Fatal(err)
	}

	// Simulate exactly what a review does: read everything, write it all back unchanged.
	for i := 0; i < 3; i++ {
		all, err := s.ReadFacets()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteFacets(all); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("both facets must survive, got %d", len(all))
	}
	var found bool
	for _, f := range all {
		if f.Key != "absorbed" {
			continue
		}
		found = true
		if f.Active() {
			t.Error("archived_at was lost across the write-back — compaction would silently revert")
		}
		if f.MergedInto != "thinking/survivor" {
			t.Errorf("merged_into was lost across the write-back, got %q", f.MergedInto)
		}
	}
	if !found {
		t.Fatal("the archived facet disappeared entirely")
	}
}

func TestReadActiveFacetsExcludesArchived(t *testing.T) {
	s := lineageStore(t)
	archived := lf("default", "thinking", "gone", "v")
	archived.ArchivedAt = "2026-10-02T00:00:00Z"
	if err := s.WriteFacets([]Facet{lf("default", "thinking", "live", "v"), archived}); err != nil {
		t.Fatal(err)
	}

	active, err := s.ReadActiveFacets()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Key != "live" {
		t.Fatalf("want only the live facet, got %+v", active)
	}
	// And the whole-history read must still see both, or writers would delete the archive.
	all, _ := s.ReadFacets()
	if len(all) != 2 {
		t.Fatalf("ReadFacets must still return archived facets, got %d", len(all))
	}
}

func TestLensFacetCounts(t *testing.T) {
	s := lineageStore(t)
	a := lf("default", "thinking", "arch", "v")
	a.ArchivedAt = "2026-10-02T00:00:00Z"
	other := lf("math", "thinking", "elsewhere", "v")
	if err := s.WriteFacets([]Facet{lf("default", "thinking", "live", "v"), a, other}); err != nil {
		t.Fatal(err)
	}
	active, archived := s.LensFacetCounts("default")
	if active != 1 || archived != 1 {
		t.Errorf("want 1 active / 1 archived for default, got %d/%d", active, archived)
	}
	// Another lens's facets must not be counted — compaction reports per lens.
	if a2, ar2 := s.LensFacetCounts("math"); a2 != 1 || ar2 != 0 {
		t.Errorf("want 1/0 for math, got %d/%d", a2, ar2)
	}
}

// The link that makes a compact profile honest: any node unfolds into what produced it.
func TestFacetTrajectoryExpandsTheMergeTree(t *testing.T) {
	s := lineageStore(t)
	// survivor <- mid <- leaf, plus a direct tributary and an unrelated facet.
	mid := lf("default", "thinking", "mid", "mid value")
	mid.ArchivedAt, mid.MergedInto = "t", "thinking/survivor"
	leaf := lf("default", "thinking", "leaf", "leaf value")
	leaf.ArchivedAt, leaf.MergedInto = "t", "thinking/mid"
	direct := lf("default", "thinking", "direct", "direct value")
	direct.ArchivedAt, direct.MergedInto = "t", "thinking/survivor"

	if err := s.WriteFacets([]Facet{
		lf("default", "thinking", "survivor", "broad"), mid, leaf, direct,
		lf("default", "habits", "unrelated", "x"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.FacetTrajectory("default", "thinking", "survivor")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, f := range got {
		keys[f.Key] = true
		if len(f.Versions) == 0 {
			t.Errorf("%s must carry its version history", f.Key)
		}
	}
	for _, want := range []string{"mid", "leaf", "direct"} {
		if !keys[want] {
			t.Errorf("trajectory should include %s, got %v", want, keys)
		}
	}
	if keys["survivor"] {
		t.Error("the survivor itself must not be in its own trajectory")
	}
	if keys["unrelated"] {
		t.Error("an unrelated facet leaked into the trajectory")
	}
}

// A hand-edited archive could produce a cycle; expansion must terminate.
func TestFacetTrajectoryTerminatesOnACycle(t *testing.T) {
	s := lineageStore(t)
	a := lf("default", "thinking", "a", "va")
	a.ArchivedAt, a.MergedInto = "t", "thinking/b"
	b := lf("default", "thinking", "b", "vb")
	b.ArchivedAt, b.MergedInto = "t", "thinking/a"
	if err := s.WriteFacets([]Facet{a, b}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.FacetTrajectory("default", "thinking", "a"); err != nil {
			t.Error(err)
		}
	}()
	<-done // a non-terminating expansion fails the test by timeout rather than hanging silently
}

// A database written by the previous schema version must gain the columns with every existing
// facet still ACTIVE — nothing could archive one before v9, so empty is the truthful default.
func TestMigrationFromV8LeavesEveryFacetActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witness.db")

	// Build a v8-shaped facets table by hand: no archived_at, no merged_into.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE facets (
		   id        INTEGER PRIMARY KEY AUTOINCREMENT,
		   lens      TEXT NOT NULL,
		   dimension TEXT NOT NULL,
		   key       TEXT NOT NULL,
		   last_seen TEXT NOT NULL DEFAULT '',
		   UNIQUE(lens, dimension, key)
		 )`,
		`INSERT INTO facets(lens, dimension, key, last_seen) VALUES ('default','thinking','old','2026-01-01T00:00:00Z')`,
		`PRAGMA user_version=8`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed v8: %v", err)
		}
	}
	db.Close()

	t.Setenv("WITNESS_HOME", dir)
	s, err := Open()
	if err != nil {
		t.Fatalf("migrate v8 -> current: %v", err)
	}
	defer s.Close()

	all, err := s.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the pre-existing facet must survive the migration, got %d", len(all))
	}
	if !all[0].Active() || all[0].MergedInto != "" {
		t.Errorf("a migrated facet must be active and unmerged, got %+v", all[0])
	}
	// And the migration must be idempotent — a second Open re-runs it.
	s.Close()
	s2, err := Open()
	if err != nil {
		t.Fatalf("second Open after migration: %v", err)
	}
	defer s2.Close()
	if all2, _ := s2.ReadActiveFacets(); len(all2) != 1 {
		t.Errorf("re-opening must not disturb the facet, got %d", len(all2))
	}
}

// The exact bug a real run caught: archive_reason was added to the v9 step AFTER a build had
// already stamped user_version=9, and migrate() early-returns when the stored version is >=
// schemaVersion — so the column's own guard was never reached and the archive opened with a
// query referencing a column that did not exist ("no such column: f.archive_reason"). A column
// can never be added to a version number some binary has already written.
func TestMigrationAddsALateColumnToAnAlreadyStampedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witness.db")

	// A database as a pre-release v9 build left it: the first two lineage columns, stamped 9,
	// and no archive_reason.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE facets (
		   id          INTEGER PRIMARY KEY AUTOINCREMENT,
		   lens        TEXT NOT NULL,
		   dimension   TEXT NOT NULL,
		   key         TEXT NOT NULL,
		   last_seen   TEXT NOT NULL DEFAULT '',
		   archived_at TEXT NOT NULL DEFAULT '',
		   merged_into TEXT NOT NULL DEFAULT '',
		   UNIQUE(lens, dimension, key)
		 )`,
		`INSERT INTO facets(lens, dimension, key) VALUES ('default','thinking','stamped_at_v9')`,
		`PRAGMA user_version=9`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed pre-release v9: %v", err)
		}
	}
	db.Close()

	t.Setenv("WITNESS_HOME", dir)
	s, err := Open()
	if err != nil {
		t.Fatalf("a stamped-v9 archive must still gain the late column: %v", err)
	}
	defer s.Close()

	// The read itself is the assertion: it SELECTs archive_reason.
	all, err := s.ReadFacets()
	if err != nil {
		t.Fatalf("ReadFacets after migration: %v", err)
	}
	if len(all) != 1 || all[0].Key != "stamped_at_v9" {
		t.Fatalf("the pre-existing facet must survive, got %+v", all)
	}
	if all[0].ArchiveReason != "" {
		t.Errorf("a migrated facet has no archive reason yet, got %q", all[0].ArchiveReason)
	}
}

func TestArchiveReasonSurvivesTheWriteBack(t *testing.T) {
	s := lineageStore(t)
	f := lf("default", "thinking", "absorbed", "v")
	f.ArchivedAt, f.MergedInto, f.ArchiveReason = "2026-10-02T00:00:00Z", "thinking/survivor", "one shared mechanism"
	if err := s.WriteFacets([]Facet{lf("default", "thinking", "survivor", "v"), f}); err != nil {
		t.Fatal(err)
	}
	// Same round-trip a review performs, which is where a dropped column would surface.
	all, _ := s.ReadFacets()
	if err := s.WriteFacets(all); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ReadFacets()
	for _, got := range all {
		if got.Key == "absorbed" && got.ArchiveReason != "one shared mechanism" {
			t.Errorf("the merge reason was lost across the write-back, got %q", got.ArchiveReason)
		}
	}
}

// The claim has to survive the same review write-back the archive state does: the reviewer
// reads the whole profile and rewrites it every fold, so a column dropped on write would erase
// every claim on the next review rather than at claim time — a silent, delayed loss that would
// quietly put the full prose back into every review call.
//
// Needs a REAL store: a fake that holds the slice in memory cannot catch a missing SQL column,
// which is exactly how the first version of this passed while the column was being dropped.
func TestClaimSurvivesTheWriteBack(t *testing.T) {
	s := lineageStore(t)
	f := lf("default", "thinking", "claimed", "the full analysis, which is long")
	f.Versions[0].Claim = "He checks state before acting."
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
	all, err := s.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 facet, got %d", len(all))
	}
	cur := all[0].Current()
	if cur.Claim != "He checks state before acting." {
		t.Errorf("the claim was lost across the write-back, got %q", cur.Claim)
	}
	if cur.Value != "the full analysis, which is long" {
		t.Errorf("the value must be preserved too, got %q", cur.Value)
	}
	// And Gist prefers the claim, which is what shrinks the review call.
	if cur.Gist() != cur.Claim {
		t.Errorf("Gist should return the claim, got %q", cur.Gist())
	}
}

// A version with no claim must Gist to its value, or an un-claimed archive would send empty
// current_value to the review call and the model would mint duplicates of everything.
func TestGistFallsBackToTheValue(t *testing.T) {
	v := FacetVersion{Value: "the full value"}
	if v.Gist() != "the full value" {
		t.Errorf("want the value, got %q", v.Gist())
	}
	v.Claim = "   "
	if v.Gist() != "the full value" {
		t.Errorf("a blank claim must still fall back, got %q", v.Gist())
	}
}

func TestParseFacetRef(t *testing.T) {
	cases := []struct {
		in       string
		dim, key string
		ok       bool
	}{
		{"thinking/zooms_out", "thinking", "zooms_out", true},
		{"", "", "", false},
		{"noslash", "", "", false},
		{"/leading", "", "", false},
		{"trailing/", "", "", false},
		// A key containing a slash is not producible by the prompt, but splitting on the
		// FIRST separator keeps the dimension correct rather than returning junk.
		{"dim/a/b", "dim", "a/b", true},
	}
	for _, tc := range cases {
		dim, key, ok := ParseFacetRef(tc.in)
		if dim != tc.dim || key != tc.key || ok != tc.ok {
			t.Errorf("ParseFacetRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, dim, key, ok, tc.dim, tc.key, tc.ok)
		}
	}
}
