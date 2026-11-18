package store

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// How classifySessions picks a target for an incoming session among our copies of its family
// (feed, feed@<hash>, ...). Whatever the history, a merge must not lose a transcript, hold one
// twice, or fold two into one session, and merging the same export again must change nothing.

// heldTranscripts is every session of an archive and its turn texts.
func heldTranscripts(t *testing.T, a *arch) map[string][]string {
	t.Helper()
	sessions, err := queryStrings(context.Background(), a.st.db, `SELECT DISTINCT session FROM raw`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, s := range sessions {
		out[s] = rawTexts(t, a.st, s)
	}
	return out
}

// copiesOf counts the sessions holding each transcript.
func copiesOf(held map[string][]string) map[string]int {
	n := map[string]int{}
	for _, turns := range held {
		n[strings.Join(turns, " ")]++
	}
	return n
}

func isPrefix(p, of []string) bool { return len(p) <= len(of) && slices.Equal(p, of[:len(p)]) }

// heldIn reports whether some session holds turns or an extension of them.
func heldIn(turns []string, held map[string][]string) bool {
	for _, h := range held {
		if isPrefix(turns, h) {
			return true
		}
	}
	return false
}

// editLast rewrites a session's last turn in place, as `witness ingest` does when a record it
// already ingested is edited (ApplyRawImport with replace).
func editLast(t *testing.T, a *arch, session, text string) {
	t.Helper()
	recs, err := a.st.ReadRaw(session)
	if err != nil || len(recs) == 0 {
		t.Fatalf("edit %s: %v (%d records)", session, err, len(recs))
	}
	recs[len(recs)-1].Text = text
	if err := a.st.ApplyRawImport(SessionMeta{Session: session}, recs, "", "", true); err != nil {
		t.Fatal(err)
	}
}

// recorded seeds an in-session (active) observation and a staged row on a session: what the MCP
// record_observation handler writes, the only copies there are.
func recorded(t *testing.T, a *arch, id, session string) {
	t.Helper()
	ob := Observation{ID: id, TS: "2026-03-01T00:00:00Z", Session: session, Lens: "default", Dimension: "d",
		Observation: "text of " + id, Source: "active"}
	if err := a.st.AppendObservations([]Observation{ob}); err != nil {
		t.Fatal(err)
	}
	ob.ID = id + "-staged"
	if err := a.st.StageObservation(ob); err != nil {
		t.Fatal(err)
	}
}

// minedState is each mined observation's session, and each session's watermark for the lens
// "default".
func minedState(t *testing.T, a *arch) (mined map[string]string, progress map[string]int) {
	t.Helper()
	mined, progress = map[string]string{}, map[string]int{}
	ids, err := queryStrings(context.Background(), a.st.db, `SELECT obs_id || '|' || session FROM observations WHERE source <> 'active'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ids {
		id, sess, _ := strings.Cut(s, "|")
		mined[id] = sess
	}
	for _, sess := range heldSessions(t, a) {
		if n := a.st.DistilledCount(sess, "default"); n > 0 {
			progress[sess] = n
		}
	}
	return mined, progress
}

func heldSessions(t *testing.T, a *arch) []string {
	t.Helper()
	s, err := queryStrings(context.Background(), a.st.db, `SELECT DISTINCT session FROM raw ORDER BY session`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// recordedOn maps each active observation and staged row to the sessions it is filed under. A
// staged row whose payload names another session than its column (DrainStaged trusts the payload)
// reads "column|payload".
func recordedOn(t *testing.T, a *arch) map[string][]string {
	t.Helper()
	rows, err := a.st.db.Query(`SELECT obs_id, session FROM observations WHERE source = 'active'
		UNION ALL SELECT obs_id, session || CASE WHEN json_extract(payload, '$.session') = session THEN ''
		  ELSE '|' || json_extract(payload, '$.session') END FROM staged ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, s string
		if err := rows.Scan(&id, &s); err != nil {
			t.Fatal(err)
		}
		out[id] = append(out[id], s)
	}
	return out
}

// A duplicate's transcript can go, since ours holds the turns; what it carries cannot, and is
// re-filed to the session holding its turns, here its in-session (active) observations and staged
// rows, the only copies there are. That session is OURS, under an id the incoming archive occupies
// with another transcript, which the merge moves on to a minted id; rows re-filed to that id in
// the copy would have gone with it, onto turns they were never recorded on.
func TestMergeRefilesWhatADuplicateRecordedOntoOurCopy(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("file:feed@11111111", "user:X1", "user:X2")
	inc.raw("file:feed@11111111", "user:X1", "user:X3")
	inc.raw("file:feed@00000000", "user:X1", "user:X2")
	recorded(t, inc, "dup", "file:feed@00000000")
	recorded(t, inc, "claimant", "file:feed@11111111")
	inc.cursor("default", inc.highWater()) // both reviewed there

	rep := base.merge(inc.export(), true)
	ours, minted := "file:feed@11111111", mintedID("file:feed", "user:X1", "user:X3")
	want := map[string][]string{"dup": {ours}, "dup-staged": {ours}, "claimant": {minted}, "claimant-staged": {minted}}
	if got := recordedOn(t, base); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded rows filed as %v, want %v (%+v)", got, want, rep.Sessions)
	}
	if o := rep.Observations; o.Reviewed != 2 || o.Unreviewed != 0 {
		t.Errorf("both observations were reviewed on the incoming side and must arrive below the cursor: %+v", o)
	}
	if n := copiesOf(heldTranscripts(t, base)); n["X1 X2"] != 1 {
		t.Errorf("[X1 X2] held %d times: %v", n["X1 X2"], heldTranscripts(t, base))
	}
	if again := base.merge(inc.export(), true); !again.Empty() {
		t.Errorf("merging the same export again: %+v staged=%d observations=%+v", again.Sessions, again.Staged, again.Observations)
	}
}

// The same when the duplicate is a second incoming copy: what it recorded arrives on the twin that
// is kept, and so does its watermark where it is the higher one (the turns are the same: the
// lower one would mine the rest again, beside what the dropped twin mined of them).
func TestMergeRefilesWhatADuplicateRecordedOntoItsTwin(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("file:feed", "user:a")
	inc.raw("file:feed", "user:a", "assistant:b")
	copyID := mintedID("file:feed", "user:a", "assistant:c")
	twin := "file:feed@00000000"
	if twin == copyID {
		twin = "file:feed@11111111"
	}
	inc.raw(copyID, "user:a", "assistant:c")
	inc.raw(twin, "user:a", "assistant:c")
	recorded(t, inc, "twin", twin)
	for s, n := range map[string]int{copyID: 1, twin: 2} {
		if err := inc.st.MarkDistilled(s, "default", n); err != nil {
			t.Fatal(err)
		}
	}

	rep := base.merge(inc.export(), true)
	if rep.Sessions.Duplicates != 1 {
		t.Fatalf("setup: want the twin dropped as a duplicate, got %+v", rep.Sessions)
	}
	want := map[string][]string{"twin": {copyID}, "twin-staged": {copyID}}
	if got := recordedOn(t, base); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded rows filed as %v, want %v", got, want)
	}
	if n := base.st.DistilledCount(copyID, "default"); n != 2 {
		t.Errorf("%s has progress %d, want the twin's 2", copyID, n)
	}
}

// Ours holds the incoming [X1 X2] already, under an id the incoming archive claims with another
// transcript. Re-minting it gave it an id of its own beside ours: the same turns held, and later
// mined, twice. It is a duplicate of what we hold.
func TestMergeDropsACopyWeAlreadyHoldWhenItsIDIsClaimed(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("file:feed@11111111", "user:X1", "user:X2")
	inc.raw("file:feed@11111111", "user:X1", "user:X3")
	inc.raw("file:feed@00000000", "user:X1", "user:X2")

	rep := base.merge(inc.export(), true)
	held := heldTranscripts(t, base)
	if n := copiesOf(held); n["X1 X2"] != 1 || n["X1 X3"] != 1 {
		t.Errorf("want [X1 X2] and [X1 X3] held once each, got %v (%+v)", held, rep.Sessions)
	}
	if got := held[mintedID("file:feed", "user:X1", "user:X3")]; !reflect.DeepEqual(got, []string{"X1", "X3"}) {
		t.Errorf("the diverged [X1 X3] should arrive under its minted id: %v", held)
	}
	if rep.Sessions.Duplicates != 1 {
		t.Errorf("want the copy of [X1 X2] counted as a duplicate, got %+v", rep.Sessions)
	}
}

// A native incoming session whose transcript is a prefix of an incoming copy that grew from it is
// not a duplicate: a prefix is not a copy. Both arrive, even into an empty archive. Dropping the
// native as a "duplicate" of the longer copy lost what was mined from it, its progress and its
// importer keys, so a facet cited an observation that existed nowhere and the next ingest
// re-created the session and mined it again.
func TestMergeKeepsANativeSessionNextToALongerCopy(t *testing.T) {
	t.Run("into an empty archive", func(t *testing.T) {
		base, inc := newArch(t), newArch(t)
		inc.raw("file:feed", "user:a")
		inc.raw(mintedID("file:feed", "user:a"), "user:a", "user:b")
		inc.obs("n1", "file:feed", "default", "2026-03-01T00:00:00Z")
		if err := inc.st.MarkDistilled("file:feed", "default", 1); err != nil {
			t.Fatal(err)
		}
		if err := inc.st.SetMetaString(FileImportKeysNS+":file:feed", `["r1:h"]`); err != nil {
			t.Fatal(err)
		}
		inc.cursor("default", inc.highWater())
		inc.facets(mf("d", "k", "v", 0.9, 1, "n1"))

		rep := base.merge(inc.export(), true)
		if got, want := heldTranscripts(t, base), heldTranscripts(t, inc); !reflect.DeepEqual(got, want) {
			t.Errorf("a merge into an empty archive holds %v, the archive merged %v (%+v)", got, want, rep.Sessions)
		}
		if rep.Sessions.New != 2 || rep.Sessions.Duplicates != 0 {
			t.Errorf("want both sessions new and no duplicate, got %+v", rep.Sessions)
		}
		obs, _ := base.st.ReadObservations("default")
		if !slices.ContainsFunc(obs, func(o Observation) bool { return o.ID == "n1" && o.Session == "file:feed" }) {
			t.Errorf("mined n1 lost while a facet cites it: %v", idsOf(obs))
		}
		if n := base.st.DistilledCount("file:feed", "default"); n != 1 {
			t.Errorf("file:feed progress %d, want 1", n)
		}
		if base.st.MetaString(FileImportKeysNS+":file:feed") == "" {
			t.Errorf("file:feed's importer keys lost")
		}
	})
	// Y ingests record 1 as [q], merges Z's [a] (minted as a copy) and Z's growth [a b], then
	// re-ingests record 1, edited to a: Y holds [a] natively beside the copy [a b].
	t.Run("after a real history", func(t *testing.T) {
		y, z, b := newArch(t), newArch(t), newArch(t)
		y.raw("file:feed", "user:q")
		z.raw("file:feed", "user:a")
		y.merge(z.export(), true)
		z.raw("file:feed", "user:b")
		y.merge(z.export(), true)
		editLast(t, y, "file:feed", "a")
		y.obs("y-mined", "file:feed", "default", "2026-03-01T00:00:00Z")
		if err := y.st.MarkDistilled("file:feed", "default", 1); err != nil {
			t.Fatal(err)
		}

		rep := b.merge(y.export(), true)
		if got, want := heldTranscripts(t, b), heldTranscripts(t, y); !reflect.DeepEqual(got, want) {
			t.Errorf("B holds %v after taking Y's archive into an empty one, Y holds %v (%+v)", got, want, rep.Sessions)
		}
		obs, _ := b.st.ReadObservations("default")
		if !slices.ContainsFunc(obs, func(o Observation) bool { return o.ID == "y-mined" }) {
			t.Errorf("y-mined lost: %v", idsOf(obs))
		}
	})
}

// The same transcript under two incoming ids is one transcript, whatever ours holds: here ours
// holds a prefix of it, so the first twin is appended to ours and, with that member claimed, the
// second arrived beside it as a session of its own. What the dropped twin recorded in-session
// lands where the kept one does.
func TestMergeHoldsAnExactTwinOnceEvenWhenAppended(t *testing.T) {
	for _, other := range []string{"file:feed@ffffffff", "file:feed@00000000"} { // after, before the minted id
		t.Run(other, func(t *testing.T) {
			base, inc := newArch(t), newArch(t)
			base.raw("file:feed", "user:a")
			inc.raw(mintedID("file:feed", "user:a", "user:b"), "user:a", "user:b")
			inc.raw(other, "user:a", "user:b")
			recorded(t, inc, "r-other", other)

			rep := base.merge(inc.export(), true)
			held := heldTranscripts(t, base)
			if !reflect.DeepEqual(held, map[string][]string{"file:feed": {"a", "b"}}) || rep.Sessions.Duplicates != 1 {
				t.Errorf("want [a b] held once, appended to file:feed, and one duplicate; got %v (%+v)", held, rep.Sessions)
			}
			if got, want := recordedOn(t, base), map[string][]string{"r-other": {"file:feed"}, "r-other-staged": {"file:feed"}}; !reflect.DeepEqual(got, want) {
				t.Errorf("recorded rows filed as %v, want %v", got, want)
			}
		})
	}
	// Y's [a x] diverges from Z's [a b], which Y mints as a copy; Y then edits record 2 to b.
	t.Run("after a real history", func(t *testing.T) {
		y, z, base := newArch(t), newArch(t), newArch(t)
		y.raw("file:feed", "user:a", "user:x")
		z.raw("file:feed", "user:a", "user:b")
		y.merge(z.export(), true)
		editLast(t, y, "file:feed", "b")
		base.raw("file:feed", "user:a")

		rep := base.merge(y.export(), true)
		if n := copiesOf(heldTranscripts(t, base))["a b"]; n != 1 {
			t.Errorf("[a b] held %d times: %v (%+v)", n, heldTranscripts(t, base), rep.Sessions)
		}
	})
}

// Of two incoming twins the native one is kept, even when ours holds the other's id: keeping that id
// (with another transcript here) re-minted the twin under its hash, so the native session, the id
// its importer knows, was gone with its importer keys and the next ingest would re-create it.
func TestMergeKeepsTheNativeOfTwoTwins(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("file:feed@11111111", "user:x")
	inc.raw("file:feed", "user:a")
	inc.raw("file:feed@11111111", "user:a")
	if err := inc.st.SetMetaString(FileImportKeysNS+":file:feed", `["r1:h"]`); err != nil {
		t.Fatal(err)
	}

	rep := base.merge(inc.export(), true)
	want := map[string][]string{"file:feed": {"a"}, "file:feed@11111111": {"x"}}
	if got := heldTranscripts(t, base); !reflect.DeepEqual(got, want) || rep.Sessions.Duplicates != 1 {
		t.Errorf("want %v and one duplicate, got %v (%+v)", want, got, rep.Sessions)
	}
	if base.st.MetaString(FileImportKeysNS+":file:feed") == "" {
		t.Errorf("file:feed's importer keys lost")
	}
}

// Ours holds the incoming [X1 X2] under X, an id the incoming archive claims with another
// transcript, so the incoming copy is a duplicate. The other side mined and reviewed it, and we
// never mined X: its observation and progress must arrive on X, as they do when nothing claims X,
// or the facet citing it dangles and X is mined again (its evidence counted twice). If we mined
// X too, theirs is the second wording of what we hold and stays behind.
func TestMergeKeepsTheOtherSidesMiningWhenWeHoldTheTurns(t *testing.T) {
	const x = "file:feed@11111111"
	for _, c := range []struct {
		name             string
		claimed, oursToo bool
	}{{"available", false, false}, {"claimed", true, false}, {"claimed and mined here", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			base, inc := newArch(t), newArch(t)
			base.raw(x, "user:X1", "user:X2")
			want := []string{"m1"}
			if c.oursToo {
				base.obs("o1", x, "default", "2026-02-01T00:00:00Z")
				if err := base.st.MarkDistilled(x, "default", 2); err != nil {
					t.Fatal(err)
				}
				want = []string{"o1"}
			}
			if c.claimed {
				inc.raw(x, "user:X1", "user:X3")
			}
			inc.raw("file:feed@00000000", "user:X1", "user:X2")
			inc.obs("m1", "file:feed@00000000", "default", "2026-03-01T00:00:00Z")
			if err := inc.st.MarkDistilled("file:feed@00000000", "default", 2); err != nil {
				t.Fatal(err)
			}
			inc.cursor("default", inc.highWater())
			inc.facets(mf("d", "k", "v", 0.9, 1, "m1"))

			rep := base.merge(inc.export(), true)
			var onX []string
			obs, _ := base.st.ReadObservations("default")
			for _, o := range obs {
				if o.Session == x {
					onX = append(onX, o.ID)
				}
			}
			if !reflect.DeepEqual(onX, want) {
				t.Errorf("observations on %s: %v, want %v; all here: %v (%+v)", x, onX, want, idsOf(obs), rep.Sessions)
			}
			if !c.oursToo {
				if f := base.facet("default", "d", "k"); f == nil || !slices.ContainsFunc(obs, func(o Observation) bool { return o.ID == f.Versions[0].BecauseOf[0] }) {
					t.Errorf("the facet's citation does not resolve: %v; observations %v", f, idsOf(obs))
				}
				if o := rep.Observations; o.Reviewed != 1 || o.Unreviewed != 0 {
					t.Errorf("m1 was reviewed on the incoming side and must arrive below the cursor: %+v", o)
				}
			}
			if n := base.st.DistilledCount(x, "default"); n != 2 {
				t.Errorf("%s has progress %d, want 2", x, n)
			}
			if pending, _ := base.st.PendingSessions([]string{"default"}); slices.Contains(pending, x) {
				t.Errorf("%s is pending again: %v", x, pending)
			}
			if again := base.merge(inc.export(), true); !again.Empty() {
				t.Errorf("merging the same export again: %+v observations=%+v", again.Sessions, again.Observations)
			}
		})
	}
}

// localTwin is an archive holding one transcript under two ids of its own, with no help from a
// contrived state: B's feed [a b] minted A's [a c] as X, and B then took the same edit A made, so
// its feed equals X.
func localTwin(t *testing.T) (b *arch, x string) {
	a, b := newArch(t), newArch(t)
	a.raw("file:feed", "user:a", "user:c")
	b.raw("file:feed", "user:a", "user:b")
	b.merge(a.export(), true)
	x = mintedID("file:feed", "user:a", "user:c")
	editLast(t, b, "file:feed", "c")
	if got := heldTranscripts(t, b); !reflect.DeepEqual(got, map[string][]string{"file:feed": {"a", "c"}, x: {"a", "c"}}) {
		t.Fatalf("setup: B holds %v", got)
	}
	return b, x
}

// mineWhole mines a whole session: one observation and the watermark at its end.
func mineWhole(t *testing.T, a *arch, id, session string) {
	t.Helper()
	a.obs(id, session, "default", "2026-03-01T00:00:00Z")
	if err := a.st.MarkDistilled(session, "default", a.st.RawCount(session)); err != nil {
		t.Fatal(err)
	}
}

// Incoming twins whose ids ours holds with their turns are not the merge's to collapse: each goes
// to its own session here. Collapsing them filed the copy's rows under the feed, so merging an
// archive into itself, or back from a restore of it (a merge into an empty archive, which keeps
// the twins as they are), staged the copy's recorded row a second time and marked the feed mined
// with the copy's watermark.
func TestMergeBackOfAnArchiveWithALocalTwinChangesNothing(t *testing.T) {
	for _, from := range []string{"itself", "a restore of it"} {
		t.Run(from, func(t *testing.T) {
			b, x := localTwin(t)
			recorded(t, b, "rX", x)
			mineWhole(t, b, "mX", x)
			export := b.export()
			if from == "a restore of it" {
				restore := newArch(t)
				restore.merge(export, true)
				export = restore.export()
			}
			before := snapshotArchive(t, b.st)
			if rep := b.merge(export, true); !rep.Empty() || rep.RawRows != 0 {
				t.Errorf("not empty: sessions %+v staged=%d progressRows=%d observations %+v",
					rep.Sessions, rep.Staged, rep.ProgressRows, rep.Observations)
			}
			if after := snapshotArchive(t, b.st); !reflect.DeepEqual(before, after) {
				t.Errorf("the archive changed:\n staged   %v -> %v\n progress %v -> %v",
					before["staged"], after["staged"], before["progress"], after["progress"])
			}
		})
	}
}

// Both sides hold the same local twin; we mined the feed, they mined the copy. Their mining of the
// copy belongs on our copy: filed under our feed with the twins collapsed, it was skipped as a
// second wording of what we mined there, and our copy stayed pending, to be mined again.
func TestMergeKeepsTheOtherSidesMiningOfATwinBothSidesHold(t *testing.T) {
	b, x := localTwin(t)
	c, _ := localTwin(t)
	mineWhole(t, b, "mB", "file:feed")
	mineWhole(t, c, "mC", x)

	rep := b.merge(c.export(), true)
	if n := b.st.DistilledCount(x, "default"); n != 2 {
		t.Errorf("B's %s has progress %d after taking C, which mined it: it is mined again (%+v %+v)", x, n, rep.Sessions, rep.Observations)
	}
	if m, _ := minedState(t, b); m["mC"] != x {
		t.Errorf("C's mining of %s (mC) is on %q in B, want it there", x, m["mC"])
	}
}

// Ours needs no twin of its own for that: ours holds the feed [a c], mined, and a copy X that
// extends it, unmined. The incoming archive holds [a c] under both ids and mined X, so its mining
// of X's first two turns belongs on our X.
func TestMergeKeepsTheMiningOfACopyOursExtends(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	x := mintedID("file:feed", "user:a", "user:c")
	base.raw("file:feed", "user:a", "user:c")
	base.raw(x, "user:a", "user:c", "user:d")
	mineWhole(t, base, "mB", "file:feed")
	inc.raw("file:feed", "user:a", "user:c")
	inc.raw(x, "user:a", "user:c")
	mineWhole(t, inc, "mX", x)

	rep := base.merge(inc.export(), true)
	if n := base.st.DistilledCount(x, "default"); n != 2 {
		t.Errorf("our %s [a c d] has progress %d: the incoming mining of its first two turns was discarded (%+v %+v)", x, n, rep.Sessions, rep.Observations)
	}
	if m, _ := minedState(t, base); m["mX"] != x {
		t.Errorf("mX is on %q, want %s", m["mX"], x)
	}
}

// An incoming session goes on our session of its own id when that holds its turns or more, though
// another of ours holds them exactly: ours holds the feed [a c], mined, and X [a c d], unmined, and
// the incoming archive mined X [a c]. Placed on our feed, its mining was skipped as a second
// wording of what we mined there, and our X stayed pending, to be mined again. So too beside an
// incoming twin of X.
func TestMergePlacesASessionOnOurOwnIDWhenItHoldsTheTurns(t *testing.T) {
	for _, twin := range []bool{false, true} {
		t.Run(map[bool]string{false: "alone", true: "beside an incoming twin"}[twin], func(t *testing.T) {
			base, inc := newArch(t), newArch(t)
			x := mintedID("file:feed", "user:a", "user:c")
			base.raw("file:feed", "user:a", "user:c")
			base.raw(x, "user:a", "user:c", "user:d")
			mineWhole(t, base, "mB", "file:feed")
			inc.raw(x, "user:a", "user:c")
			if twin {
				inc.raw("file:feed@ffffffff", "user:a", "user:c")
			}
			mineWhole(t, inc, "mX", x)

			rep := base.merge(inc.export(), true)
			if n := base.st.DistilledCount(x, "default"); n != 2 || rep.Sessions.LongerHere != 1 {
				t.Errorf("our %s [a c d] has progress %d, want 2 from the incoming %s it holds (%+v %+v)", x, n, x, rep.Sessions, rep.Observations)
			}
			if m, _ := minedState(t, base); m["mX"] != x || m["mB"] != "file:feed" {
				t.Errorf("mined %v, want mX on %s and mB on file:feed", m, x)
			}
			want := map[string][]string{"file:feed": {"a", "c"}, x: {"a", "c", "d"}}
			if got := heldTranscripts(t, base); !reflect.DeepEqual(got, want) {
				t.Errorf("holds %v, want %v", got, want)
			}
			snap := snapshotArchive(t, base.st)
			if again := base.merge(inc.export(), true); !again.Empty() || !reflect.DeepEqual(snap, snapshotArchive(t, base.st)) {
				t.Errorf("merging the same export again: %+v staged=%d progressRows=%d", again.Sessions, again.Staged, again.ProgressRows)
			}
		})
	}
}

// A twin whose id ours holds with ANOTHER transcript has no place under that id here: it still goes,
// with everything it carries, where its turns are held, and they are held once. Left out of the
// collapse like a twin ours holds with its turns, it would arrive beside its twin under the id
// minted from its hash. X grew from [a] on two machines: [a c] there, [a d] here.
func TestMergeFilesATwinOursHoldsWithAnotherTranscriptWhereItsTurnsAre(t *testing.T) {
	x, full := mintedID("file:feed", "user:a"), mintedID("file:feed", "user:a", "user:c")
	for _, c := range []struct {
		name, carrier, to string
		ours              map[string][]string
	}{
		{"the other twin's id holds the turns here", x, "file:feed", map[string][]string{"file:feed": {"a", "c"}, x: {"a", "d"}}},
		{"the copy's id holds the turns here", "file:feed", x, map[string][]string{"file:feed": {"z"}, x: {"a", "c"}}},
		{"neither id holds the turns here", "file:feed", full, map[string][]string{"file:feed": {"z"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, inc := newArch(t), newArch(t)
			for s, turns := range c.ours {
				for _, turn := range turns {
					base.raw(s, "user:"+turn)
				}
			}
			inc.raw("file:feed", "user:a", "user:c")
			inc.raw(x, "user:a", "user:c")
			recorded(t, inc, "r", c.carrier)
			mineWhole(t, inc, "m", c.carrier)

			rep := base.merge(inc.export(), true)
			want := map[string][]string{c.to: {"a", "c"}}
			for s, turns := range c.ours {
				if !slices.Equal(turns, []string{"a", "c"}) {
					want[s] = turns
				}
			}
			if got := heldTranscripts(t, base); !reflect.DeepEqual(got, want) {
				t.Errorf("holds %v, want %v (%+v)", got, want, rep.Sessions)
			}
			if got, want := recordedOn(t, base), map[string][]string{"r": {c.to}, "r-staged": {c.to}}; !reflect.DeepEqual(got, want) {
				t.Errorf("recorded rows filed as %v, want %v", got, want)
			}
			if mined, progress := minedState(t, base); mined["m"] != c.to || progress[c.to] != 2 {
				t.Errorf("mined %v, progress %v; want m and 2 on %s", mined, progress, c.to)
			}
			snap := snapshotArchive(t, base.st)
			if again := base.merge(inc.export(), true); !again.Empty() || !reflect.DeepEqual(snap, snapshotArchive(t, base.st)) {
				t.Errorf("merging the same export again: %+v staged=%d progressRows=%d", again.Sessions, again.Staged, again.ProgressRows)
			}
		})
	}
}

// mergeBackHistory is three machines, up to a merge back. A ingests a file [A1]; C has a feed of its
// own and mints A's as feed@hash([A1]), and B starts from C. A appends A2 and C's copy grows to
// [A1 A2]; A then edits that record in place (A3) and B's copy grows to [A1 A3]: one id, two
// growths. A takes C's copy [A1 A2] under that id, and B, which holds [A1 A3] there, mints A's
// copy as feed@hash([A1 A2]). Merging B back carries A's own [A1 A2] home under the minted id —
// which sorts first, so it is classified while B's [A1 A3] still claims the id A holds it under.
func mergeBackHistory(t *testing.T) (a, b *arch, ours, theirs string) {
	a, b, c := newArch(t), newArch(t), newArch(t)
	c.raw("file:feed", "user:C0")
	a.raw("file:feed", "user:A1")
	c.merge(a.export(), true)
	b.merge(c.export(), true)
	a.raw("file:feed", "user:A2")
	c.merge(a.export(), true)
	editLast(t, a, "file:feed", "A3")
	b.merge(a.export(), true)
	a.merge(c.export(), true)
	b.merge(a.export(), true)

	ours, theirs = mintedID("file:feed", "user:A1"), mintedID("file:feed", "user:A1", "user:A2")
	if got := rawTexts(t, a.st, ours); !reflect.DeepEqual(got, []string{"A1", "A2"}) {
		t.Fatalf("setup: A's %s holds %v", ours, got)
	}
	if got := rawTexts(t, b.st, theirs); !reflect.DeepEqual(got, []string{"A1", "A2"}) {
		t.Fatalf("setup: B's %s holds %v", theirs, got)
	}
	if theirs > ours {
		t.Fatalf("setup: %s must sort before %s", theirs, ours)
	}
	return a, b, ours, theirs
}

// The merge back must not hold A's copy a second time, and if both sides mined their copy, A must
// end with ONE set of observations of those turns: dropSharedMined cannot catch the second set,
// which arrives under another session id.
func TestMergeBackDoesNotHoldACopyTwiceAfterAGrowth(t *testing.T) {
	t.Run("transcript", func(t *testing.T) {
		a, b, _, _ := mergeBackHistory(t)
		rep := a.merge(b.export(), true)
		if n := copiesOf(heldTranscripts(t, a))["A1 A2"]; n != 1 {
			t.Errorf("A holds [A1 A2] %d times after the merge back: %v (%+v)", n, heldTranscripts(t, a), rep.Sessions)
		}
	})
	t.Run("both mined", func(t *testing.T) {
		a, b, ours, theirs := mergeBackHistory(t)
		a.obs("a-mined", ours, "default", "2026-03-01T00:00:00Z")
		if err := a.st.MarkDistilled(ours, "default", 2); err != nil {
			t.Fatal(err)
		}
		b.obs("b-mined", theirs, "default", "2026-03-01T00:00:00Z")
		if err := b.st.MarkDistilled(theirs, "default", 2); err != nil {
			t.Fatal(err)
		}
		a.merge(b.export(), true)
		obs, _ := a.st.ReadObservations("default")
		if got := idsOf(obs); !reflect.DeepEqual(got, []string{"a-mined"}) {
			t.Errorf("the turns [A1 A2] are mined twice in A: %v", got)
		}
	})
}

// Re-merging the same export changes nothing. A copy minted from [c] grows apart on O ([c b]) and
// on R ([c a]); R's feed [c b c] extends O's copy, but R's own copy claims that id, so it arrives
// under an id minted for it. On every later merge of the same file its first match was that
// claimed copy again: it was re-minted to the id it already had here and appended in full to the
// session holding it.
func TestRemergingTheSameExportAddsNothing(t *testing.T) {
	// c makes the claimed copy sort before the id its growth is minted under.
	c := ""
	for i := 0; c == "" && i < 100; i++ {
		if cc := fmt.Sprintf("c%d", i); mintedID("file:feed", "user:"+cc) < mintedID("file:feed", "user:"+cc, "user:b", "user:"+cc) {
			c = cc
		}
	}
	if c == "" {
		t.Fatal("setup: no first turn sorts the copies as needed")
	}
	p, o, s, r := newArch(t), newArch(t), newArch(t), newArch(t)
	p.raw("file:feed", "user:"+c)
	o.raw("file:feed", "user:ro")
	s.raw("file:feed", "user:s0")
	o.merge(p.export(), true) // O: copy [c]
	s.merge(p.export(), true) // S: copy [c]
	p.raw("file:feed", "user:a")
	s.merge(p.export(), true) // S: copy [c a]
	r.raw("file:feed", "user:"+c, "user:b")
	o.merge(r.export(), true) // O: copy [c b]
	r.merge(s.export(), true) // R: feed [c b], copy [c a]
	r.raw("file:feed", "user:"+c)

	exp := r.export()
	o.merge(exp, true)
	for round := 2; round <= 3; round++ {
		before := snapshotArchive(t, o.st)
		rep := o.merge(exp, true)
		if !rep.Empty() || rep.RawRows != 0 {
			t.Errorf("round %d of the same merge: %+v rawRows=%d; O holds %v", round, rep.Sessions, rep.RawRows, heldTranscripts(t, o))
		}
		if !reflect.DeepEqual(before, snapshotArchive(t, o.st)) {
			t.Errorf("round %d of the same merge changed the archive", round)
		}
	}
}

// Ours holds the id the merge mints for an incoming transcript, under a DIFFERENT transcript, and
// nothing in the family matches it. That takes an 8-hex collision, or a write into a minted id:
// renaming the incoming session onto it folded two transcripts into one session. The merge refuses
// and writes nothing — whether the incoming session already carries that id or is the family root.
func TestMergeRefusesToFoldIntoOurDifferentTranscript(t *testing.T) {
	copyID := mintedID("file:feed", "user:a", "assistant:c")
	for name, incoming := range map[string]string{"copy": copyID, "root": "file:feed"} {
		t.Run(name, func(t *testing.T) {
			base, inc := newArch(t), newArch(t)
			base.raw("file:feed", "user:q")
			base.raw(copyID, "user:x")
			inc.raw(incoming, "user:a", "assistant:c")

			before := snapshotArchive(t, base.st)
			_, err := base.st.Merge(context.Background(), inc.export(), true, mergeNow)
			if err == nil || !strings.Contains(err.Error(), copyID) {
				t.Fatalf("want a refusal naming %s, got %v; %s holds %v", copyID, err, copyID, rawTexts(t, base.st, copyID))
			}
			if !reflect.DeepEqual(before, snapshotArchive(t, base.st)) {
				t.Errorf("a refused merge changed the archive")
			}
		})
	}
}

// The incoming archive holds another transcript under the id a diverged incoming session needs
// (an 8-hex collision, or a session ingested under a name that looks minted). Renaming onto it
// would fold the two together in the copy; dropping the diverged one as a duplicate lost it. The
// merge refuses and writes nothing.
func TestMergeRefusesAMintedIDTheIncomingArchiveHoldsDifferently(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	copyID := mintedID("file:feed", "user:a", "assistant:c")
	base.raw("file:feed", "user:q")
	inc.raw("file:feed", "user:a", "assistant:c")
	inc.raw(copyID, "user:x")

	before := snapshotArchive(t, base.st)
	_, err := base.st.Merge(context.Background(), inc.export(), true, mergeNow)
	if err == nil || !strings.Contains(err.Error(), copyID) {
		t.Fatalf("want a refusal naming %s, got %v; this archive holds %v", copyID, err, heldTranscripts(t, base))
	}
	if !reflect.DeepEqual(before, snapshotArchive(t, base.st)) {
		t.Errorf("a refused merge changed the archive")
	}
}

// TestMergeFamilyInvariants plays fixed random histories of three and four machines that ingest one
// named file feed (sharing its first record, starting their own, or starting empty), append to it,
// edit its last record in place (as `witness ingest` does) or not, with fresh text or, with reuse,
// as another copy there has it, record active observations and staged rows on it, mine one of their
// sessions now and then, and merge each other in random order. After every merge: no transcript of
// either side is lost (each is a prefix of a session afterwards); the merge adds no second copy of
// one, and into an empty archive holds exactly what the other one does; none is folded (every
// session is a prefix of a state some feed really had); a native session of theirs is gone only
// for a session holding exactly its turns, or one of ours that held them already; every recorded row and mined observation of ours stays where it was, and every one
// of theirs arrives on a session holding the turns of the session it was on (a mined one may stay
// behind only where ours mined such a session itself), as does their progress, and on the session of
// its own id where ours holds that with the turns or more; the dry run
// reported exactly what the merge did; and merging the same export again is empty and changes
// nothing. Merging an archive into itself, or back from a restore of it, changes nothing either:
// checked after an edit that may have made the feed a twin of a copy, and at the end of a history.
func TestMergeFamilyInvariants(t *testing.T) {
	// 30 ops a history. Besides seed 1 of each kind, the first seeds whose history broke an earlier
	// classifySessions: the one before #223's fix lost a minted copy as its own duplicate in
	// 3/plain 32, 3/edits 16, 4/plain 8 and 4/edits 7; the first fix appended a re-merged transcript
	// to itself in 3/edits 37 and held a copy twice in 4/edits 57; the second lost what a dropped
	// duplicate recorded in 3/edits 145 and 4/plain 156; the third lost what one had mined in
	// 3/reuse 75; the fourth collapsed an archive's own twins, so merging it into itself was not
	// empty, in 3/reuse 26. A seed names a history only for this mergeHistory: change how it draws
	// and the seeds name others.
	const steps = 30
	for _, c := range []struct {
		machines     int
		edits, reuse bool
		seeds        []int64
	}{
		{3, false, false, []int64{1, 32}}, {3, true, false, []int64{1, 16, 37, 145}},
		{4, false, false, []int64{1, 8, 156}}, {4, true, false, []int64{1, 7, 57}},
		{3, true, true, []int64{26, 75}}, {4, true, true, []int64{1}},
	} {
		for _, seed := range c.seeds {
			t.Run(fmt.Sprintf("machines=%d/edits=%v/reuse=%v/seed=%d", c.machines, c.edits, c.reuse, seed), func(t *testing.T) {
				if broke := mergeHistory(t, seed, c.machines, steps, c.edits, c.reuse); broke != "" {
					t.Error(broke)
				}
			})
		}
	}
}

// mergeHistory plays one history and returns the first broken invariant, with the ops that led to
// it, or "".
func mergeHistory(t *testing.T, seed int64, machines, steps int, edits, reuse bool) string {
	ctx := context.Background()
	r := rand.New(rand.NewSource(seed))
	// Recording, mining and reusing a text draw from streams of their own, so a seed's other ops are
	// the same with or without them.
	rec, mine, pick := rand.New(rand.NewSource(-seed)), rand.New(rand.NewSource(seed+1<<40)), rand.New(rand.NewSource(seed+2<<40))
	names := []string{"A", "B", "C", "D"}[:machines]
	ar := map[string]*arch{}
	legit := [][]string{{"a"}} // every state some machine's feed really had
	var ops []string
	last := "" // the archive that took the last merge
	broke := func(format string, args ...any) string {
		return fmt.Sprintf(format, args...) + "\n  ops: " + strings.Join(ops, "; ")
	}
	for _, m := range names {
		ar[m] = newArch(t)
		switch r.Intn(3) {
		case 0:
			ar[m].raw("file:feed", "user:a")
			ops = append(ops, m+".feed = [a]")
		case 1:
			ar[m].raw("file:feed", "user:"+m+"0")
			ops = append(ops, m+".feed = ["+m+"0]")
			legit = append(legit, []string{m + "0"})
		default: // a relay, or a machine that ingests the feed later
			ops = append(ops, m+" empty")
		}
	}
	for step := 1; step <= steps; step++ {
		// Every step some machine records in-session, between turns as record_observation does: so
		// two copies of one transcript can carry different recorded rows, and a dropped duplicate
		// can hold rows nothing else does.
		if m := names[rec.Intn(machines)]; ar[m].st.RawCount("file:feed") > 0 {
			id := fmt.Sprintf("%s%d-rec", m, step)
			recorded(t, ar[m], id, "file:feed")
			ops = append(ops, m+".feed records "+id)
		}
		// And some machine's worker mines one of its sessions, copies included, up to its end: so a
		// session can arrive mined on one side and unmined on the other.
		m := names[mine.Intn(machines)]
		if sessions := heldSessions(t, ar[m]); len(sessions) > 0 {
			s := sessions[mine.Intn(len(sessions))]
			if n := ar[m].st.RawCount(s); ar[m].st.DistilledCount(s, "default") < n {
				id := fmt.Sprintf("%s%d-mined", m, step)
				ar[m].obs(id, s, "default", "2026-03-01T00:00:00Z")
				if err := ar[m].st.MarkDistilled(s, "default", n); err != nil {
					t.Fatal(err)
				}
				ops = append(ops, m+" mines "+s+" as "+id)
			}
		}
		if r.Intn(3) == 0 {
			m := names[r.Intn(machines)]
			text := fmt.Sprintf("%s%d", m, step)
			if edits && ar[m].st.RawCount("file:feed") > 1 && r.Intn(3) == 0 {
				if reuse && pick.Intn(2) == 0 {
					// The record edited the way another copy here has it (two machines made the same
					// edit): the feed can now equal a copy, or be a prefix of one.
					n := ar[m].st.RawCount("file:feed")
					var alt []string
					for _, s := range heldSessions(t, ar[m]) {
						if turns := rawTexts(t, ar[m].st, s); s != "file:feed" && len(turns) >= n {
							alt = append(alt, turns[n-1])
						}
					}
					if len(alt) > 0 {
						text = alt[pick.Intn(len(alt))]
					}
				}
				editLast(t, ar[m], "file:feed", text)
				ops = append(ops, m+".feed edit-last "+text)
				if reuse { // the edit can make the feed a twin of a copy here
					if b := roundTrip(t, ar[m]); b != "" {
						return broke("%s: %s", m, b)
					}
				}
			} else {
				ar[m].raw("file:feed", "user:"+text)
				ops = append(ops, m+".feed += "+text)
			}
			legit = append(legit, rawTexts(t, ar[m].st, "file:feed"))
			continue
		}
		i := r.Intn(machines)
		x, y := names[i], names[(i+1+r.Intn(machines-1))%machines]
		ops, last = append(ops, x+" <- "+y), x
		ours, theirs := heldTranscripts(t, ar[x]), heldTranscripts(t, ar[y])
		recOurs, recTheirs := recordedOn(t, ar[x]), recordedOn(t, ar[y])
		minedOurs, progOurs := minedState(t, ar[x])
		minedTheirs, progTheirs := minedState(t, ar[y])
		export := ar[y].export()
		dry, err := ar[x].st.Merge(ctx, export, false, mergeNow)
		if err != nil {
			return broke("dry run: %v", err)
		}
		rep, err := ar[x].st.Merge(ctx, export, true, mergeNow)
		if err != nil {
			return broke("merge: %v", err)
		}
		if !reflect.DeepEqual(dry, rep) {
			return broke("the dry run reported %+v, the merge %+v", dry.Sessions, rep.Sessions)
		}
		after := heldTranscripts(t, ar[x])
		for side, held := range map[string]map[string][]string{"ours": ours, "theirs": theirs} {
			for s, turns := range held {
				if !heldIn(turns, after) {
					return broke("lost %s %s %v; %s now holds %v", side, s, turns, x, after)
				}
			}
		}
		// Only an edit that writes a text back can make a second copy locally; a merge never adds one.
		// Into an empty archive it holds exactly what the other one does.
		for k, n := range copiesOf(after) {
			if len(ours) > 0 && n > max(1, copiesOf(ours)[k]) {
				return broke("%s holds [%s] %d times: %v", x, k, n, after)
			}
		}
		if len(ours) == 0 && !reflect.DeepEqual(after, theirs) {
			return broke("%s was empty and holds %v after taking %s, which holds %v", x, after, y, theirs)
		}
		for s, turns := range after {
			if !slices.ContainsFunc(legit, func(l []string) bool { return isPrefix(turns, l) }) {
				return broke("%s holds %s %v, which no feed ever held", x, s, turns)
			}
		}
		recAfter := recordedOn(t, ar[x])
		for id, sessions := range recOurs {
			for _, s := range sessions {
				if !slices.Contains(recAfter[id], s) {
					return broke("%s's recorded %s left %s: now on %v", x, id, s, recAfter[id])
				}
			}
		}
		for id, sessions := range recTheirs {
			if len(recOurs[id]) > 0 {
				continue // ours has it already, and keeps where it is
			}
			for _, s := range sessions {
				if !slices.ContainsFunc(recAfter[id], func(on string) bool { return isPrefix(theirs[s], after[on]) }) {
					return broke("%s's recorded %s on %s %v is on %v in %s, which lacks those turns: %v", y, id, s, theirs[s], recAfter[id], x, after)
				}
			}
		}
		// What the other side mined arrives on a session holding those turns, or stays behind only
		// because ours mined such a session itself (dropSharedMined: it is a second wording of what
		// ours holds, and ours's watermark wins).
		minedAfter, progAfter := minedState(t, ar[x])
		oursMined := map[string]bool{}
		for _, s := range minedOurs {
			oursMined[s] = true
		}
		for s := range progOurs {
			oursMined[s] = true
		}
		holding := func(turns []string, ok func(on string) bool) bool {
			for on, h := range after {
				if isPrefix(turns, h) && ok(on) {
					return true
				}
			}
			return false
		}
		for id, s := range minedOurs {
			if minedAfter[id] != s {
				return broke("%s's mined %s left %s: now on %q", x, id, s, minedAfter[id])
			}
		}
		for id, s := range minedTheirs {
			if _, ok := minedOurs[id]; ok {
				continue
			}
			if on, ok := minedAfter[id]; ok && !isPrefix(theirs[s], after[on]) || !ok && !holding(theirs[s], func(on string) bool { return oursMined[on] }) {
				return broke("%s's mined %s on %s %v is on %q in %s, which lacks those turns or never mined them: %v", y, id, s, theirs[s], on, x, after)
			}
		}
		for s, n := range progTheirs {
			if !holding(theirs[s], func(on string) bool { return progAfter[on] >= n || oursMined[on] }) {
				return broke("%s's progress %d on %s %v is on no session of %s holding those turns: %v %v", y, n, s, theirs[s], x, progAfter, after)
			}
		}
		// Where ours holds a session's own id with its turns or more, what it carries lands on that id:
		// on another copy of ours, mined there, its mining was skipped, and ours of its id was mined
		// again.
		for id, sessions := range recTheirs {
			for _, s := range sessions {
				if len(recOurs[id]) == 0 && isPrefix(theirs[s], ours[s]) && !slices.Contains(recAfter[id], s) {
					return broke("%s's recorded %s on %s %v is on %v in %s, which holds %s with those turns: %v", y, id, s, theirs[s], recAfter[id], x, s, after)
				}
			}
		}
		for id, s := range minedTheirs {
			if _, ok := minedOurs[id]; ok || !isPrefix(theirs[s], ours[s]) || oursMined[s] {
				continue
			}
			if on := minedAfter[id]; on != s {
				return broke("%s's mined %s on %s %v is on %q in %s, which holds %s with those turns: %v", y, id, s, theirs[s], on, x, s, after)
			}
		}
		for s, n := range progTheirs {
			if isPrefix(theirs[s], ours[s]) && !oursMined[s] && progAfter[s] < n {
				return broke("%s's progress %d on %s %v is %d on %s in %s, which holds those turns: %v", y, n, s, theirs[s], progAfter[s], s, x, after)
			}
		}
		// A native session leaves only for a session that holds it exactly, or one of ours that
		// held its turns already.
		for s, turns := range theirs {
			if s == sessionFamily(s) && after[s] == nil && copiesOf(after)[strings.Join(turns, " ")] == 0 && !heldIn(turns, ours) {
				return broke("%s's native %s %v was removed from %s, which holds no copy of it: %v", y, s, turns, x, after)
			}
		}
		snap := snapshotArchive(t, ar[x].st)
		again, err := ar[x].st.Merge(ctx, export, true, mergeNow)
		if err != nil {
			return broke("merging the same export again: %v", err)
		}
		if !again.Empty() || again.RawRows != 0 || !reflect.DeepEqual(snap, snapshotArchive(t, ar[x].st)) {
			return broke("merging the same export again changed %s: %+v rawRows=%d; it holds %v", x, again.Sessions, again.RawRows, heldTranscripts(t, ar[x]))
		}
	}
	// And once a history, on the archive that took the last merge: after every merge, or on every
	// machine, the test ran twice as long.
	if last == "" {
		return ""
	}
	if b := roundTrip(t, ar[last]); b != "" {
		return broke("%s: %s", last, b)
	}
	return ""
}

// roundTrip merges an archive into itself, and back from a restore of it (a merge into an empty
// archive): neither may change it.
func roundTrip(t *testing.T, a *arch) string {
	ctx := context.Background()
	export, restore := a.export(), newArch(t)
	if _, err := restore.st.Merge(ctx, export, true, mergeNow); err != nil {
		return fmt.Sprintf("restoring: %v", err)
	}
	snap := snapshotArchive(t, a.st)
	for _, from := range []struct{ name, path string }{{"itself", export}, {"a restore of it", restore.export()}} {
		rep, err := a.st.Merge(ctx, from.path, true, mergeNow)
		if err != nil {
			return fmt.Sprintf("merging %s: %v", from.name, err)
		}
		if !rep.Empty() || rep.RawRows != 0 || !reflect.DeepEqual(snap, snapshotArchive(t, a.st)) {
			return fmt.Sprintf("merging %s changed it: %+v rawRows=%d staged=%d progressRows=%d; it holds %v",
				from.name, rep.Sessions, rep.RawRows, rep.Staged, rep.ProgressRows, heldTranscripts(t, a))
		}
	}
	return ""
}
