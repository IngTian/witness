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
// edit its last record in place (as `witness ingest` does) or not, and merge each other in random
// order. After every merge: no transcript of either side is lost (each is a prefix of a session
// afterwards); none is held twice (appends and edits write fresh text, so only a merge can make a
// second copy); none is folded (every session is a prefix of a state some feed really had); the
// dry run reported exactly what the merge did; and merging the same export again is empty and
// changes nothing.
func TestMergeFamilyInvariants(t *testing.T) {
	// 30 ops a history. Besides seeds 1 and 2 of each kind, the first seeds whose history broke an
	// earlier classifySessions: the one before #223's fix lost a minted copy as its own duplicate
	// in 3/plain 32, 3/edits 16, 4/plain 8 and 4/edits 7; the first fix appended a re-merged
	// transcript to itself in 3/edits 37 and held a copy twice in 4/edits 57. A seed names a history
	// only for this mergeHistory: change how it draws and the seeds name others.
	const steps = 30
	for _, c := range []struct {
		machines int
		edits    bool
		seeds    []int64
	}{{3, false, []int64{1, 2, 32}}, {3, true, []int64{1, 2, 16, 37}}, {4, false, []int64{1, 2, 8}}, {4, true, []int64{1, 2, 7, 57}}} {
		for _, seed := range c.seeds {
			t.Run(fmt.Sprintf("machines=%d/edits=%v/seed=%d", c.machines, c.edits, seed), func(t *testing.T) {
				if broke := mergeHistory(t, seed, c.machines, steps, c.edits); broke != "" {
					t.Error(broke)
				}
			})
		}
	}
}

// mergeHistory plays one history and returns the first broken invariant, with the ops that led to
// it, or "".
func mergeHistory(t *testing.T, seed int64, machines, steps int, edits bool) string {
	ctx := context.Background()
	r := rand.New(rand.NewSource(seed))
	names := []string{"A", "B", "C", "D"}[:machines]
	ar := map[string]*arch{}
	legit := [][]string{{"a"}} // every state some machine's feed really had
	var ops []string
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
		if r.Intn(3) == 0 {
			m := names[r.Intn(machines)]
			text := fmt.Sprintf("%s%d", m, step)
			if edits && ar[m].st.RawCount("file:feed") > 1 && r.Intn(3) == 0 {
				editLast(t, ar[m], "file:feed", text)
				ops = append(ops, m+".feed edit-last "+text)
			} else {
				ar[m].raw("file:feed", "user:"+text)
				ops = append(ops, m+".feed += "+text)
			}
			legit = append(legit, rawTexts(t, ar[m].st, "file:feed"))
			continue
		}
		i := r.Intn(machines)
		x, y := names[i], names[(i+1+r.Intn(machines-1))%machines]
		ops = append(ops, x+" <- "+y)
		ours, theirs := heldTranscripts(t, ar[x]), heldTranscripts(t, ar[y])
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
		for k, n := range copiesOf(after) {
			if n > 1 {
				return broke("%s holds [%s] %d times: %v", x, k, n, after)
			}
		}
		for s, turns := range after {
			if !slices.ContainsFunc(legit, func(l []string) bool { return isPrefix(turns, l) }) {
				return broke("%s holds %s %v, which no feed ever held", x, s, turns)
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
	return ""
}
