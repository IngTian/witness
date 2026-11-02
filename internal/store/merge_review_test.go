package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// Regression tests for the defects an adversarial review of `witness merge` found and reproduced
// (four dimension reviewers, each finding then verified by a skeptic). Every test below failed on
// the first implementation. They are separate from merge_test.go so the record of what the review
// caught stays legible.

func pending(t *testing.T, st *Store, lenses ...string) []string {
	t.Helper()
	got, err := st.PendingSessions(lenses)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

// An ignored INSERT still consumes an AUTOINCREMENT value. Re-merging an archive whose rows were
// all already here pushed the sequence past MAX(seq), so the next observation anyone mined advanced
// the decay clock by the whole corpus at once — halving every facet in the profile.
func TestReviewRemergeDoesNotBurnSequenceNumbers(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	for i := 0; i < 50; i++ {
		inc.obs(fmt.Sprintf("o%d", i), "s1", "default", "2026-03-01T00:00:00Z")
	}
	inc.cursor("default", 50)
	inc.raw("s1", "user:a")
	path := inc.export()
	base.merge(path, true)
	inc.raw("s1", "assistant:one more turn") // something new, so the re-merge really runs
	base.merge(inc.export(), true)

	var maxSeq, seqNext int64
	if err := base.st.db.QueryRow(`SELECT MAX(seq) FROM observations`).Scan(&maxSeq); err != nil {
		t.Fatal(err)
	}
	if err := base.st.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'observations'`).Scan(&seqNext); err != nil {
		t.Fatal(err)
	}
	if seqNext != maxSeq {
		t.Errorf("the observation sequence sits at %d with MAX(seq)=%d: the next observation will age every "+
			"facet by %d at once", seqNext, maxSeq, seqNext-maxSeq)
	}
}

// The "already mined here" test was a correlated subquery over observations, which has no
// (session, lens) index: quadratic, ~15 minutes at 75k a side, with the write lock held and every
// capture hook in the window losing its turn. Bounded generously; the quadratic took 22s here.
func TestReviewMergeIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const n = 6000
	mk := func(prefix, sess string) []Observation {
		out := make([]Observation, n)
		for i := range out {
			out[i] = Observation{ID: fmt.Sprintf("%s%d", prefix, i), Session: fmt.Sprintf("%s%d", sess, i%200),
				Lens: "default", Dimension: "d", Observation: "x", Source: "mined", TS: "2026-03-01T00:00:00Z"}
		}
		return out
	}
	base, inc := newArch(t), newArch(t)
	if err := base.st.AppendObservations(mk("b", "bs")); err != nil {
		t.Fatal(err)
	}
	if err := inc.st.AppendObservations(mk("i", "is")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	base.merge(inc.export(), false)
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("a %dx%d dry run took %v; the write lock is held for all of it", n, n, d)
	}
}

// Our cleanup reclaimed a session's transcript and progress but kept its observations. Merging a
// copy that still has the transcript must not leave it pending — it would be mined a second time
// beside the observations we kept, the permanent duplication the shared-mined rule exists to stop.
func TestReviewCleanedUpSessionDoesNotComeBackPending(t *testing.T) {
	// theirs = how much of the transcript the other side had mined: all of it, none of it, or
	// only the first turn. The partial case is the one a "keep their progress" rule got wrong: it
	// left our second turn pending, re-mined beside the observations we already hold.
	for _, theirs := range []int{2, 0, 1} {
		theyMined := theirs > 0
		t.Run(fmt.Sprintf("they mined %d of 2 turns", theirs), func(t *testing.T) {
			base, inc := newArch(t), newArch(t)
			base.raw("old", "user:a", "assistant:b")
			base.obs("bo", "old", "default", "2026-01-01T00:00:00Z")
			if err := base.st.MarkDistilled("old", "default", 2); err != nil {
				t.Fatal(err)
			}
			if _, _, err := base.st.PruneSessionsBefore("2030-01-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			inc.raw("old", "user:a", "assistant:b")
			if theyMined {
				inc.obs("io", "old", "default", "2026-01-01T00:00:00Z")
				if err := inc.st.MarkDistilled("old", "default", theirs); err != nil {
					t.Fatal(err)
				}
			}
			base.merge(inc.export(), true)
			if p := pending(t, base.st, "default"); len(p) != 0 {
				t.Errorf("the reclaimed session came back pending %v: it would be mined again beside our L1", p)
			}
		})
	}
}

// With no new folded row arriving, rows the incoming side folded but we had not stayed above our
// cursor and were folded twice; and new unfolded rows were queued "ours, then theirs".
func TestReviewNoNewFoldedRowsStillPlacesEverything(t *testing.T) {
	t.Run("overlap only", func(t *testing.T) {
		base, inc := newArch(t), newArch(t)
		base.obs("r1", "s1", "default", "2026-03-01T00:00:01Z")
		base.obs("shared", "s2", "default", "2026-03-01T00:00:02Z")
		base.cursor("default", 1)
		inc.obs("r1", "s1", "default", "2026-03-01T00:00:01Z")
		inc.obs("shared", "s2", "default", "2026-03-01T00:00:02Z")
		inc.cursor("default", 2)
		inc.facets(mf("d", "k", "v", 0.7, 2, "shared"))
		base.merge(inc.export(), true)
		got, _ := base.st.ReadObservationsSinceOrdered("default", base.st.ReviewRowid("default"))
		if ids := idsOf(got); len(ids) != 0 {
			t.Errorf("rows the other side already folded are up for review again: %v", ids)
		}
	})
	t.Run("time order", func(t *testing.T) {
		base, inc := newArch(t), newArch(t)
		base.obs("aug", "s1", "default", "2026-08-01T00:00:00Z")
		inc.obs("june", "s2", "default", "2026-06-01T00:00:00Z") // the other machine never reviewed
		base.merge(inc.export(), true)
		got, _ := base.st.ReadObservationsSinceOrdered("default", base.st.ReviewRowid("default"))
		if ids := idsOf(got); strings.Join(ids, ",") != "june,aug" {
			t.Errorf("unreviewed rows must be queued in time order, got %v", ids)
		}
	})
}

// The other side's newer rows are counted against each version's OWN evidence, not this side's
// newest row: one late observation on an idle laptop used to cancel every real chance the other
// machine had to confirm a June facet.
func TestReviewDecayCountsAgainstEachFacetsOwnEvidence(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	for i := 0; i < 10; i++ {
		base.obs(fmt.Sprintf("june%d", i), "s1", "default", "2026-06-01T00:00:00Z")
	}
	base.obs("dec31", "s1", "default", "2026-12-31T00:00:00Z") // one straggler, months later
	base.cursor("default", 11)
	base.facets(mf("d", "june_trait", "seen in June", 0.9, 10, "june9"))
	for i := 0; i < 300; i++ {
		inc.obs(fmt.Sprintf("i%d", i), "s9", "default", "2026-09-01T00:00:00Z")
	}
	inc.cursor("default", 300)

	base.merge(inc.export(), true)
	behind := base.highWater() - base.facet("default", "d", "june_trait").Current().ConfidenceSeq
	if behind < 300 {
		t.Errorf("the June facet is %d observations behind; the other machine's 300 September rows were "+
			"chances to confirm it that never did", behind)
	}
}

// A longer chain is adopted only if its extension is NEWER evidence. Chain length alone let the
// other machine's February change overrule our December reaffirmation of the old value.
func TestReviewFastForwardNeedsNewerEvidence(t *testing.T) {
	v1 := FacetVersion{Value: "v1", ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z", Confidence: 0.9, ConfidenceSeq: 1}
	base, inc := newArch(t), newArch(t)
	base.obs("o_dec", "s1", "default", "2026-12-01T00:00:00Z")
	ours := v1
	ours.BecauseOf = []string{"o_dec"}
	base.facets(Facet{Lens: "default", Dimension: "d", Key: "k", LastSeen: "2026-12-01T00:00:00Z", Versions: []FacetVersion{ours}})

	inc.obs("o_feb", "s2", "default", "2026-02-01T00:00:00Z")
	closed := v1
	closed.ValidTo = "2026-02-01T00:00:00Z"
	v2 := FacetVersion{Value: "v2", ValidFrom: "2026-02-01T00:00:00Z", RecordedAt: "2026-02-01T00:00:00Z", Confidence: 0.7, ConfidenceSeq: 1, BecauseOf: []string{"o_feb"}}
	inc.facets(Facet{Lens: "default", Dimension: "d", Key: "k", LastSeen: "2026-02-01T00:00:00Z", Versions: []FacetVersion{closed, v2}})

	base.merge(inc.export(), true)
	k := base.facet("default", "d", "k")
	if k.Current() == nil || k.Current().Value != "v1" {
		t.Fatalf("our December-evidenced v1 must stay current over their February change, got %+v", k.Versions)
	}
	if k.Versions[0].ValidTo != "" {
		t.Errorf("our v1 was closed at %q while citing December evidence", k.Versions[0].ValidTo)
	}
	if other := base.facet("default", "d", "k__m1"); other == nil || other.Current() == nil || other.Current().Value != "v2" {
		t.Errorf("their change history must be kept beside it, got %+v", other)
	}
}

// Round 1 archives the losing copy into the winner. The other machine then re-confirms its copy
// (same version, later LastSeen). Round 2 must not un-archive the merge's own loser: that left two
// active copies of one trait.
func TestReviewRepeatedMergeKeepsItsLoserArchived(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.obs("ob", "s1", "default", "2026-08-01T00:00:00Z")
	base.facets(mf("d", "k", "ours, August", 0.9, 1, "ob"))
	inc.obs("oi", "s2", "default", "2026-02-01T00:00:00Z")
	theirs := mf("d", "k", "theirs, February", 0.7, 1, "oi")
	inc.facets(theirs)
	base.merge(inc.export(), true)

	theirs.LastSeen = "2026-12-01T00:00:00Z" // re-confirmed over there after the merge
	inc.facets(theirs)
	base.merge(inc.export(), true)

	active := 0
	all, _ := base.st.ReadFacets()
	for _, f := range all {
		if keyFamily(f.Key) == "k" && f.Active() {
			active++
		}
	}
	if active != 1 {
		t.Errorf("want exactly one active copy of the trait, got %d", active)
	}
}

// Merging back can bring our own facet home ARCHIVED by the other side's merge. Adopting that
// archive state archived our winning facet into itself — the trait vanished from the profile.
func TestReviewMergeBackNeverArchivesAFacetIntoItself(t *testing.T) {
	c1 := FacetVersion{Value: "ours", ValidFrom: "2026-06-01T00:00:00Z", RecordedAt: "2026-06-01T00:00:00Z", Confidence: 0.9, ConfidenceSeq: 1, BecauseOf: []string{"ob"}}
	base, inc := newArch(t), newArch(t)
	base.obs("ob", "s1", "default", "2026-10-01T00:00:00Z")
	base.facets(Facet{Lens: "default", Dimension: "d", Key: "k", LastSeen: "2026-10-05T00:00:00Z", Versions: []FacetVersion{c1}})

	inc.obs("oi", "s2", "default", "2026-06-01T00:00:00Z")
	ownK := mf("d", "k", "theirs", 0.7, 1, "oi")
	ourOld := Facet{Lens: "default", Dimension: "d", Key: "k__m1", LastSeen: "2026-06-01T00:00:00Z", Versions: []FacetVersion{c1},
		ArchivedAt: "2026-10-20T00:00:00Z", MergedInto: "d/k", ArchiveReason: mergeArchiveReasonPrefix + " (witness merge, 2026-10-20)"}
	inc.facets(ownK, ourOld)

	base.merge(inc.export(), true)
	all, _ := base.st.ReadFacets()
	active := 0
	for _, f := range all {
		if f.MergedInto == FacetRef(f.Dimension, f.Key) {
			t.Errorf("%s is archived into itself", f.Key)
		}
		if keyFamily(f.Key) == "k" && f.Active() {
			active++
		}
	}
	if active == 0 {
		t.Error("the trait has no active copy at all")
	}
}

// A facet-only delta — the other machine's downgrade, compaction or claim pass — is a real change.
// Reporting it as empty is how the CLI said "nothing to merge" and dropped it.
func TestReviewFacetOnlyChangeIsNotEmpty(t *testing.T) {
	v := FacetVersion{Value: "v", ValidFrom: "2026-05-01T00:00:00Z", RecordedAt: "2026-05-01T00:00:00Z", Confidence: 0.9, ConfidenceSeq: 1, BecauseOf: []string{"o1"}}
	base, inc := newArch(t), newArch(t)
	for _, a := range []*arch{base, inc} {
		a.obs("o1", "s1", "default", "2026-05-01T00:00:00Z")
		a.obs("o2", "s1", "default", "2026-09-01T00:00:00Z")
		a.cursor("default", 2)
	}
	base.facets(Facet{Lens: "default", Dimension: "d", Key: "k", Versions: []FacetVersion{v}})
	down := v
	down.Confidence, down.BecauseOf = 0.6, []string{"o1", "o2"}
	inc.facets(Facet{Lens: "default", Dimension: "d", Key: "k", Versions: []FacetVersion{down}})

	rep := base.merge(inc.export(), false)
	if rep.Empty() || rep.Facets.Updated != 1 {
		t.Errorf("a downgrade on the other machine must count as a change; Empty=%v facets=%+v", rep.Empty(), rep.Facets)
	}
}

// An identical copy is a no-op, including a facet that cites nothing. Found on a real archive, not
// by review: unioning two empty citation lists produced an empty non-nil slice, counted as a change,
// so merging an archive with itself reported "4 updated".
func TestReviewIdenticalCopyWithUncitedFacetsIsEmpty(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	uncited := mf("d", "k", "no evidence recorded", 0.5, 1) // BecauseOf nil
	for _, a := range []*arch{base, inc} {
		a.obs("o1", "s1", "default", "2026-05-01T00:00:00Z")
		a.cursor("default", 1)
		a.facets(uncited)
	}
	if rep := base.merge(inc.export(), false); !rep.Empty() {
		t.Errorf("an identical archive must merge as a no-op; facets %+v", rep.Facets)
	}
}

// Evidence time reads BOTH archives' observations. Two machines restored from one archive hold the
// same facets but, after cleanup, not the same observations: a version citing one this side lacks
// fell back to its RecordedAt, so an identical copy looked newer and its clock was "adopted". On a
// real archive split in two, all 1,553 such facets were reported as updated.
func TestReviewEvidenceTimeSpansBothArchives(t *testing.T) {
	cited := mf("d", "k", "same on both", 0.8, 1, "o_old")
	base, inc := newArch(t), newArch(t)
	base.obs("o_new", "s2", "default", "2026-10-01T00:00:00Z") // we no longer hold o_old
	base.facets(cited)
	inc.obs("o_old", "s1", "default", "2026-08-01T00:00:00Z")
	inc.facets(cited)
	rep := base.merge(inc.export(), false)
	if rep.Facets.Updated != 0 || rep.Facets.Shared != 1 {
		t.Errorf("an identical facet must read as already here, got %+v", rep.Facets)
	}
}

// A reference adopted from the other side follows the OTHER side's renames: their f was compacted
// into THEIR x, which lost the key to ours — f must point at their x, now x__m1, not at our x.
func TestReviewAdoptedLineageFollowsItsOwnSide(t *testing.T) {
	fv := FacetVersion{Value: "f", ValidFrom: "2026-01-01T00:00:00Z", RecordedAt: "2026-01-01T00:00:00Z", Confidence: 0.5, ConfidenceSeq: 1}
	base, inc := newArch(t), newArch(t)
	base.obs("ob", "s1", "default", "2026-08-01T00:00:00Z")
	base.facets(Facet{Lens: "default", Dimension: "d", Key: "f", LastSeen: "2026-01-01T00:00:00Z", Versions: []FacetVersion{fv}},
		mf("d", "x", "our x, August", 0.9, 1, "ob"))
	inc.obs("oi", "s2", "default", "2026-03-01T00:00:00Z")
	incF := Facet{Lens: "default", Dimension: "d", Key: "f", LastSeen: "2026-03-01T00:00:00Z", Versions: []FacetVersion{fv},
		ArchivedAt: "2026-03-02T00:00:00Z", MergedInto: "d/x", ArchiveReason: "compacted"}
	inc.facets(incF, mf("d", "x", "their x, March", 0.8, 1, "oi"))

	base.merge(inc.export(), true)
	if got := base.facet("default", "d", "f").MergedInto; got != "d/x__m1" {
		t.Errorf("f must point at THEIR x (now x__m1), got %q", got)
	}
}

// Merging back must not store our own transcript a second time. A: feed=[a,A]; B: feed=[a,B].
// B into A, then A into B, then B into A again: each archive holds each transcript exactly once.
func TestReviewMergeBackDoesNotDuplicateTranscripts(t *testing.T) {
	a, b := newArch(t), newArch(t)
	a.raw("file:feed", "user:a", "assistant:A")
	b.raw("file:feed", "user:a", "assistant:B")
	a.merge(b.export(), true)
	b.merge(a.export(), true)
	rep := a.merge(b.export(), true)

	for name, ar := range map[string]*arch{"A": a, "B": b} {
		seen := map[string]int{}
		sessions, _ := queryStrings(context.Background(), ar.st.db, `SELECT DISTINCT session FROM raw`)
		for _, s := range sessions {
			recs, _ := ar.st.ReadRaw(s)
			var parts []string
			for _, r := range recs {
				parts = append(parts, r.Text)
			}
			seen[strings.Join(parts, "|")]++
		}
		for transcript, n := range seen {
			if n != 1 {
				t.Errorf("archive %s holds transcript %q %d times (sessions %v)", name, transcript, n, sessions)
			}
		}
	}
	if rep.RawRows != 0 {
		t.Errorf("the third round added %d raw rows; everything was already in both", rep.RawRows)
	}
}

// A diverged feed that keeps growing on the other machine extends its earlier copy here, instead of
// arriving in full under a new id on every merge (its shared turns held, and mined, again and again).
func TestReviewGrowingDivergedFeedExtendsItsCopy(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("file:feed", "user:a", "assistant:ours")
	inc.raw("file:feed", "user:a", "assistant:theirs")
	first := base.merge(inc.export(), true)
	if len(first.Sessions.Diverged) != 1 {
		t.Fatalf("setup: want one diverged session, got %+v", first.Sessions)
	}
	copyID := first.Sessions.Diverged[0].To

	inc.raw("file:feed", "user:more")
	second := base.merge(inc.export(), true)
	if len(second.Sessions.Diverged) != 0 || second.Sessions.LongerThere != 1 || second.RawRows != 1 {
		t.Errorf("want the growth appended to %s (1 row), got %+v rawRows=%d", copyID, second.Sessions, second.RawRows)
	}
	if recs, _ := base.st.ReadRaw(copyID); len(recs) != 3 {
		t.Errorf("%s holds %d turns, want 3", copyID, len(recs))
	}
}

// Observations the other side mined from a transcript it has since cleaned up, of a session whose
// transcript we hold and never mined for that lens, would arrive with no progress and be mined
// again beside themselves. They are dropped; ours is mined.
func TestReviewOrphanedIncomingObservationsAreDropped(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("s1", "user:a", "assistant:b")
	inc.obs("their-math", "s1", "math", "2026-03-01T00:00:00Z")
	rep := base.merge(inc.export(), true)
	if rep.Observations.Orphaned != 1 {
		t.Errorf("want 1 orphaned observation dropped, got %d", rep.Observations.Orphaned)
	}
	if obs, _ := base.st.ReadObservations("math"); len(obs) != 0 {
		t.Errorf("the orphan arrived; enabling math would mine s1 beside it: %v", idsOf(obs))
	}
}

// A staged observation of a renamed (diverged) session drains under its NEW id. DrainStaged trusts
// the payload, so renaming only the column filed it under OUR transcript.
func TestReviewRenamedSessionsStagedRowsFollowIt(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("div", "user:a", "assistant:ours")
	inc.raw("div", "user:a", "assistant:theirs")
	if err := inc.st.StageObservation(Observation{ID: "st", Session: "div", Lens: "default", Dimension: "d", Observation: "x", Source: "active"}); err != nil {
		t.Fatal(err)
	}
	rep := base.merge(inc.export(), true)
	to := rep.Sessions.Diverged[0].To
	got, _, err := base.st.DrainStaged(to)
	if err != nil || len(got) != 1 || got[0].Session != to {
		t.Errorf("staged row filed under %s drains as %+v (err %v)", to, got, err)
	}
}
