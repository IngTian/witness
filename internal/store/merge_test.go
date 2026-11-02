package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Tests for `witness merge`. Each fixture builds TWO real archives and hands the merge a real
// `export` snapshot of the incoming one, so the input format is the one users have. Nothing calls
// a model.

var mergeNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

type arch struct {
	t  *testing.T
	st *Store
}

func newArch(t *testing.T) *arch {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &arch{t: t, st: st}
}

// raw appends turns "role:text" to a session.
func (a *arch) raw(session string, turns ...string) {
	a.t.Helper()
	have := a.st.RawCount(session)
	for i, turn := range turns {
		role, text, _ := strings.Cut(turn, ":")
		if err := a.st.AppendRaw(RawRecord{TS: "2026-01-01T00:00:00Z", Session: session, Seq: have + i, Role: role, Text: text}); err != nil {
			a.t.Fatal(err)
		}
	}
}

// obs appends one observation (seq assigned in call order).
func (a *arch) obs(id, session, lens, ts string) {
	a.t.Helper()
	if err := a.st.AppendObservations([]Observation{{ID: id, TS: ts, Session: session, Lens: lens,
		Dimension: "d", Observation: "text of " + id, Source: "mined"}}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *arch) cursor(lens string, through int64) {
	a.t.Helper()
	if err := a.st.StampReviewLens(lens, through); err != nil {
		a.t.Fatal(err)
	}
}

func (a *arch) facets(fs ...Facet) {
	a.t.Helper()
	if err := a.st.WriteFacets(fs); err != nil {
		a.t.Fatal(err)
	}
}

func (a *arch) export() string {
	a.t.Helper()
	p := filepath.Join(a.t.TempDir(), "incoming.db")
	if err := a.st.Export(p, false); err != nil {
		a.t.Fatal(err)
	}
	return p
}

func (a *arch) merge(path string, apply bool) *MergeReport {
	a.t.Helper()
	rep, err := a.st.Merge(context.Background(), path, apply, mergeNow)
	if err != nil {
		a.t.Fatalf("Merge: %v", err)
	}
	return rep
}

func (a *arch) facet(lens, dim, key string) *Facet {
	a.t.Helper()
	all, err := a.st.ReadFacets()
	if err != nil {
		a.t.Fatal(err)
	}
	for i := range all {
		if all[i].Lens == lens && all[i].Dimension == dim && all[i].Key == key {
			return &all[i]
		}
	}
	return nil
}

func (a *arch) highWater() int64 {
	a.t.Helper()
	hw, err := a.st.ObservationHighWater()
	if err != nil {
		a.t.Fatal(err)
	}
	return hw
}

// mf is a one-version active facet.
func mf(dim, key, value string, conf float64, clock int64, because ...string) Facet {
	return Facet{Lens: "default", Dimension: dim, Key: key, LastSeen: "2026-06-01T00:00:00Z",
		Versions: []FacetVersion{{Value: value, Confidence: conf, ConfidenceSeq: clock,
			ValidFrom: "2026-06-01T00:00:00Z", RecordedAt: "2026-06-01T00:00:00Z", BecauseOf: because}}}
}

func idsOf(obs []Observation) []string {
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.ID)
	}
	return out
}

// THE riskiest rule. Afterwards, for every lens, the rows above the cursor must be EXACTLY those
// folded on neither side: too low re-reviews the incoming corpus and counts its evidence twice,
// too high leaves rows never reviewed. Lenses interleave in seq (it is shared across lenses), one
// lens exists only here, one only there, and one here has never been reviewed at all.
func TestMergeReviewCursorExact(t *testing.T) {
	base := newArch(t)
	base.obs("a1", "s1", "A", "2026-03-01T00:00:01Z") // seq 1
	base.obs("b1", "s1", "B", "2026-03-01T00:00:02Z") // 2
	base.obs("a2", "s1", "A", "2026-03-01T00:00:03Z") // 3
	base.obs("b2", "s1", "B", "2026-03-01T00:00:04Z") // 4
	base.obs("a3", "s1", "A", "2026-03-01T00:00:05Z") // 5
	base.obs("d1", "s1", "D", "2026-03-01T00:00:11Z") // 6 — lens D was never reviewed
	base.cursor("A", 3)                               // a1, a2 folded; a3 not
	base.cursor("B", 2)                               // b1 folded; b2 not

	inc := newArch(t)
	inc.obs("x1", "s2", "A", "2026-03-01T00:00:06Z") // 1
	inc.obs("c1", "s2", "C", "2026-03-01T00:00:07Z") // 2
	inc.obs("x2", "s2", "A", "2026-03-01T00:00:08Z") // 3
	inc.obs("c2", "s2", "C", "2026-03-01T00:00:09Z") // 4
	inc.obs("x3", "s2", "A", "2026-03-01T00:00:10Z") // 5
	inc.cursor("A", 3)                               // x1, x2 folded; x3 not
	// lens C: no cursor on the incoming side — c1, c2 never folded

	rep := base.merge(inc.export(), true)
	if rep.Observations.Reviewed != 2 || rep.Observations.Unreviewed != 3 {
		t.Errorf("report: reviewed=%d unreviewed=%d, want 2 and 3", rep.Observations.Reviewed, rep.Observations.Unreviewed)
	}

	want := map[string][]string{"A": {"a3", "x3"}, "B": {"b2"}, "C": {"c1", "c2"}, "D": {"d1"}}
	for lens, ids := range want {
		got, err := base.st.ReadObservationsSinceOrdered(lens, base.st.ReviewRowid(lens))
		if err != nil {
			t.Fatal(err)
		}
		g := idsOf(got)
		sort.Strings(g)
		if !reflect.DeepEqual(g, ids) {
			t.Errorf("lens %s: unreviewed after merge = %v, want exactly %v", lens, g, ids)
		}
	}

	// The unreviewed block is in TIME order across both archives, not "ours then theirs".
	var order []string
	rows, err := base.st.db.Query(`SELECT obs_id FROM observations WHERE seq > ? ORDER BY seq`, base.st.ReviewRowid("A"))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		order = append(order, id)
	}
	rows.Close()
	if want := []string{"b2", "a3", "c1", "c2", "x3", "d1"}; !reflect.DeepEqual(order, want) {
		t.Errorf("unreviewed rows in seq order = %v, want time order %v", order, want)
	}
}

// Decay clocks: each keeps its distance from its own head, plus the newer observations the OTHER
// side brought. Here the incoming archive is an old laptop (January) and ours is newer (March):
// its facet must age by every one of our observations, and ours by none of its.
func TestMergeRebasesClocksIncludingTheOtherSidesNewerEvidence(t *testing.T) {
	inc := newArch(t)
	for i := 0; i < 10; i++ {
		inc.obs("i"+string(rune('a'+i)), "old", "default", "2026-01-10T00:00:00Z")
	}
	inc.cursor("default", 10)
	inc.facets(mf("traits", "old_trait", "seen in January", 0.9, 8, "ia")) // 2 behind its head

	base := newArch(t)
	for i := 0; i < 20; i++ {
		base.obs("b"+string(rune('a'+i)), "new", "default", "2026-03-10T00:00:00Z")
	}
	base.cursor("default", 20)
	base.facets(mf("traits", "new_trait", "seen in March", 0.9, 15, "ba")) // 5 behind its head

	base.merge(inc.export(), true)
	m := base.highWater()
	if m != 30 {
		t.Fatalf("merged high-water = %d, want 30", m)
	}
	if got := m - base.facet("default", "traits", "new_trait").Current().ConfidenceSeq; got != 5 {
		t.Errorf("our facet is %d observations behind, want 5: nothing the old laptop saw is newer than our evidence", got)
	}
	if got := m - base.facet("default", "traits", "old_trait").Current().ConfidenceSeq; got != 22 {
		t.Errorf("the old laptop's facet is %d observations behind, want 22 (its 2 + our 20 newer): "+
			"those were 20 chances to confirm it and none did", got)
	}
}

// Same key, independent histories: the copy with NEWER EVIDENCE keeps the key. The loser keeps its
// whole history at <key>__m1, archived into the winner, which gains its citations; the loser's own
// absorbed facets follow it.
func TestMergeSameKeyNewerEvidenceWins(t *testing.T) {
	t.Run("incoming newer", func(t *testing.T) {
		base := newArch(t)
		base.obs("ob1", "s1", "default", "2026-02-01T00:00:00Z")
		child := mf("thinking", "child", "a narrower view", 0.5, 1, "ob1")
		child.ArchivedAt, child.MergedInto = "2026-02-02T00:00:00Z", "thinking/k"
		base.facets(mf("thinking", "k", "the February view", 0.9, 1, "ob1"), child)

		inc := newArch(t)
		inc.obs("oi1", "s9", "default", "2026-08-01T00:00:00Z")
		inc.facets(mf("thinking", "k", "the August view", 0.7, 1, "oi1"))

		rep := base.merge(inc.export(), true)
		if len(rep.Facets.SameKey) != 1 || !rep.Facets.SameKey[0].IncomingWon {
			t.Fatalf("want one same-key collision won by the incoming copy, got %+v", rep.Facets.SameKey)
		}
		k := base.facet("default", "thinking", "k")
		if k == nil || !k.Active() || k.Current().Value != "the August view" {
			t.Fatalf("the key must carry the newer-evidenced value, got %+v", k)
		}
		if !containsID(k.Current().BecauseOf, "ob1") || !containsID(k.Current().BecauseOf, "oi1") {
			t.Errorf("the winner must cite both copies' evidence, got %v", k.Current().BecauseOf)
		}
		loser := base.facet("default", "thinking", "k__m1")
		if loser == nil || loser.Active() || loser.MergedInto != "thinking/k" || loser.Current().Value != "the February view" {
			t.Fatalf("the February copy must survive at k__m1, archived into k, got %+v", loser)
		}
		if c := base.facet("default", "thinking", "child"); c.MergedInto != "thinking/k__m1" {
			t.Errorf("our child must follow OUR copy to k__m1, got %q", c.MergedInto)
		}
	})
	t.Run("ours newer", func(t *testing.T) {
		base := newArch(t)
		base.obs("ob1", "s1", "default", "2026-08-01T00:00:00Z")
		base.facets(mf("thinking", "k", "the August view", 0.9, 1, "ob1"))

		inc := newArch(t)
		inc.obs("oi1", "s9", "default", "2026-02-01T00:00:00Z")
		child := mf("thinking", "child", "their narrower view", 0.5, 1, "oi1")
		child.ArchivedAt, child.MergedInto = "2026-02-02T00:00:00Z", "thinking/k"
		inc.facets(mf("thinking", "k", "the February view", 0.7, 1, "oi1"), child)

		rep := base.merge(inc.export(), true)
		if len(rep.Facets.SameKey) != 1 || rep.Facets.SameKey[0].IncomingWon {
			t.Fatalf("want one collision won by ours, got %+v", rep.Facets.SameKey)
		}
		if k := base.facet("default", "thinking", "k"); k.Current().Value != "the August view" {
			t.Errorf("ours must keep the key, got %q", k.Current().Value)
		}
		if c := base.facet("default", "thinking", "child"); c == nil || c.MergedInto != "thinking/k__m1" {
			t.Errorf("their child must follow THEIR copy to k__m1, got %+v", c)
		}
	})
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// Shared history — a restore, a re-run, a merge back — stores nothing twice. An equal chain is
// skipped; a longer incoming chain is adopted (they recorded a change we have not seen); archive
// state comes from whoever acted last; confidence and its clock travel as a PAIR from the side with
// newer evidence, so a downgrade made on the other machine is not lost (the #175 failure).
func TestMergeSharedHistory(t *testing.T) {
	v1 := FacetVersion{Value: "v1", ValidFrom: "2026-05-01T00:00:00Z", RecordedAt: "2026-05-01T00:00:00Z", Confidence: 0.9, ConfidenceSeq: 1, BecauseOf: []string{"ob"}}
	shared := Facet{Lens: "default", Dimension: "d", Key: "same", LastSeen: "2026-05-01T00:00:00Z", Versions: []FacetVersion{v1}}

	base := newArch(t)
	base.obs("ob", "s1", "default", "2026-05-01T00:00:00Z")
	ff := shared
	ff.Key = "ff"
	compacted := shared
	compacted.Key = "compacted"
	compacted.LastSeen = "2026-06-01T00:00:00Z"
	base.facets(shared, ff, compacted)

	inc := newArch(t)
	inc.obs("oi", "s9", "default", "2026-09-01T00:00:00Z")
	// same: equal chain, but re-judged DOWN on the other machine with newer evidence
	incSame := shared
	incSame.Versions = []FacetVersion{v1}
	incSame.Versions[0].Confidence, incSame.Versions[0].BecauseOf = 0.6, []string{"ob", "oi"}
	// ff: they closed v1 and opened v2
	incFF := ff
	closed := v1
	closed.ValidTo = "2026-09-01T00:00:00Z"
	incFF.Versions = []FacetVersion{closed, {Value: "v2", ValidFrom: "2026-09-01T00:00:00Z", RecordedAt: "2026-09-01T00:00:00Z", Confidence: 0.7, ConfidenceSeq: 1, BecauseOf: []string{"oi"}}}
	// compacted: they compacted it away AFTER we last saw it
	incC := compacted
	incC.Versions = []FacetVersion{v1}
	incC.ArchivedAt, incC.MergedInto = "2026-09-02T00:00:00Z", "d/same"
	inc.facets(incSame, incFF, incC)

	rep := base.merge(inc.export(), true)
	if rep.Facets.Shared != 2 || rep.Facets.FastForwarded != 1 || rep.Facets.New != 0 || len(rep.Facets.SameKey) != 0 {
		t.Fatalf("want 2 shared, 1 fast-forwarded, nothing new or colliding; got %+v", rep.Facets)
	}
	if s := base.facet("default", "d", "same"); len(s.Versions) != 1 || s.Current().Confidence != 0.6 {
		t.Errorf("the newer-evidenced downgrade to 0.6 must survive, got %+v", s.Versions)
	}
	f := base.facet("default", "d", "ff")
	if len(f.Versions) != 2 || f.Current().Value != "v2" || f.Versions[0].ValidTo == "" {
		t.Errorf("the longer incoming chain must be adopted, v1 closed, v2 current; got %+v", f.Versions)
	}
	if c := base.facet("default", "d", "compacted"); c.Active() || c.MergedInto != "d/same" {
		t.Errorf("their later compaction must stand, got active=%v merged_into=%q", c.Active(), c.MergedInto)
	}
	for _, k := range []string{"same__m1", "ff__m1", "compacted__m1"} {
		if base.facet("default", "d", k) != nil {
			t.Errorf("shared history was stored twice as %s", k)
		}
	}
}

// A session both machines mined must not be imported twice. Mining is not deterministic, so the
// incoming copy is a paraphrase with different obs_ids — union them and every moment is counted
// twice, permanently. A lens only the other side ran still comes across, and so do observations
// recorded in-session (they were never mined, so they are not paraphrases).
func TestMergeSkipsSessionsMinedOnBothSides(t *testing.T) {
	base := newArch(t)
	base.raw("s1", "user:hi", "assistant:hello")
	base.obs("bo1", "s1", "default", "2026-03-01T00:00:00Z")
	if err := base.st.MarkDistilled("s1", "default", 2); err != nil {
		t.Fatal(err)
	}

	inc := newArch(t)
	inc.raw("s1", "user:hi", "assistant:hello")
	inc.obs("io1", "s1", "default", "2026-03-01T00:00:00Z") // a paraphrase of bo1
	inc.obs("io2", "s1", "math", "2026-03-01T00:00:00Z")    // a lens we never ran
	if err := inc.st.AppendObservations([]Observation{{ID: "io3", Session: "s1", Lens: "default", Dimension: "d",
		Observation: "recorded in-session", Source: "active", TS: "2026-03-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"default", "math"} {
		if err := inc.st.MarkDistilled("s1", l, 2); err != nil {
			t.Fatal(err)
		}
	}

	rep := base.merge(inc.export(), true)
	if rep.Observations.SharedMined != 1 {
		t.Errorf("want 1 paraphrase skipped, got %d", rep.Observations.SharedMined)
	}
	have := map[string]bool{}
	all, _ := base.st.ReadObservations("default")
	for _, o := range all {
		have[o.ID] = true
	}
	math, _ := base.st.ReadObservations("math")
	for _, o := range math {
		have[o.ID] = true
	}
	if have["io1"] {
		t.Error("the incoming paraphrase of a session we already mined was imported: that moment now counts twice")
	}
	if !have["io2"] || !have["io3"] || !have["bo1"] {
		t.Errorf("lost data: have %v (want bo1, io2 and io3)", have)
	}
	if got := base.st.DistilledCount("s1", "math"); got != 2 {
		t.Errorf("math progress = %d, want 2: dropping it would re-mine the session at model cost", got)
	}
}

// Transcripts: new arrive whole; equal and shorter-there change nothing; a longer incoming
// transcript contributes only its tail; a DIVERGED one (same id, different content) is renamed so
// its transcript and what was mined from it arrive together. Importer keys must describe exactly
// the rows that end up here, or the next import replaces the session and re-mines it.
func TestMergeTranscriptKinds(t *testing.T) {
	base := newArch(t)
	base.raw("eq", "user:a", "assistant:b")
	base.raw("here", "user:a", "assistant:b", "user:c")
	base.raw("there", "user:a")
	base.raw("div", "user:a", "assistant:ours")
	_ = base.st.SetMetaString("file_import_keys:here", "k1,k2,k3")

	inc := newArch(t)
	inc.raw("eq", "user:a", "assistant:b")
	inc.raw("here", "user:a", "assistant:b")
	inc.raw("there", "user:a", "assistant:b", "user:c")
	inc.raw("div", "user:a", "assistant:theirs")
	inc.raw("new", "user:n")
	inc.obs("od1", "div", "default", "2026-03-01T00:00:00Z")
	_ = inc.st.SetMetaString("file_import_keys:there", "k1,k2,k3")
	_ = inc.st.SetMetaString("file_import_keys:here", "k1,k2")
	_ = inc.st.SetMetaString("file_import_keys:new", "n")
	_ = inc.st.SetMetaString("file_import_keys:div", "theirs")

	rep := base.merge(inc.export(), true)
	if rep.Sessions.New != 2 || rep.Sessions.Equal != 1 || rep.Sessions.LongerHere != 1 || rep.Sessions.LongerThere != 1 ||
		rep.Sessions.Appended != 2 || len(rep.Sessions.Diverged) != 1 {
		t.Fatalf("session report wrong: %+v", rep.Sessions)
	}
	texts := func(sess string) []string {
		recs, _ := base.st.ReadRaw(sess)
		var out []string
		for _, r := range recs {
			out = append(out, r.Text)
		}
		return out
	}
	if got := texts("there"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("longer-there tail not appended in order: %v", got)
	}
	if got := texts("here"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("our longer transcript was touched: %v", got)
	}
	if got := texts("div"); !reflect.DeepEqual(got, []string{"a", "ours"}) {
		t.Errorf("our diverged transcript must be kept: %v", got)
	}
	renamed := rep.Sessions.Diverged[0].To
	if !strings.HasPrefix(renamed, "div@") || !reflect.DeepEqual(texts(renamed), []string{"a", "theirs"}) {
		t.Errorf("their diverged transcript must arrive under %q, got %v", renamed, texts(renamed))
	}
	obs, _ := base.st.ReadObservations("default")
	for _, o := range obs {
		if o.ID == "od1" && o.Session != renamed {
			t.Errorf("their observation of the diverged session points at %q, not its own transcript %q", o.Session, renamed)
		}
	}
	for key, want := range map[string]string{
		"file_import_keys:there": "k1,k2,k3", // theirs: it now describes our appended rows too
		"file_import_keys:here":  "k1,k2,k3", // ours: theirs would cover fewer rows than we hold
		"file_import_keys:new":   "n",
		"file_import_keys:div":   "", // never applied to OUR diverged session
	} {
		if got := base.st.MetaString(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// A second run of the same merge — even against a fresh re-export, different bytes — changes
// nothing: not a row, not a clock, not a cursor.
func TestMergeIsIdempotentOnAReexport(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("s1", "user:a")
	base.raw("div", "user:a", "assistant:ours")
	base.obs("b1", "s1", "default", "2026-02-01T00:00:00Z")
	base.obs("b2", "s1", "default", "2026-02-02T00:00:00Z")
	base.cursor("default", 1)
	base.facets(mf("d", "k", "ours", 0.9, 1, "b1"))
	inc.raw("s2", "user:x")
	inc.raw("div", "user:a", "assistant:theirs")
	inc.obs("i1", "s2", "default", "2026-08-01T00:00:00Z")
	inc.obs("i2", "s2", "default", "2026-08-02T00:00:00Z")
	inc.cursor("default", 1)
	inc.facets(mf("d", "k", "theirs", 0.7, 1, "i1"), mf("d", "other", "only there", 0.5, 1, "i2"))

	base.merge(inc.export(), true)
	before := snapshotArchive(t, base.st)
	rep := base.merge(inc.export(), true) // a re-export: same content, new file
	if !rep.Empty() {
		t.Errorf("a second merge of the same archive must change nothing; report: %+v", rep)
	}
	if after := snapshotArchive(t, base.st); !reflect.DeepEqual(before, after) {
		t.Errorf("the second merge changed the archive:\nbefore %v\nafter  %v", before, after)
	}
}

// snapshotArchive is everything the merge can touch, in a comparable form.
func snapshotArchive(t *testing.T, st *Store) map[string]any {
	t.Helper()
	q := func(sql string) []string {
		rows, err := st.db.Query(sql)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			var b strings.Builder
			for _, v := range vals {
				if x, ok := v.([]byte); ok {
					b.WriteString(string(x))
				} else {
					fmt.Fprint(&b, v)
				}
				b.WriteString("|")
			}
			out = append(out, b.String())
		}
		return out
	}
	facets, _ := st.ReadFacets()
	return map[string]any{
		"raw":      q(`SELECT session, seq, role, text FROM raw ORDER BY id`),
		"obs":      q(`SELECT seq, obs_id, session, lens FROM observations ORDER BY seq`),
		"progress": q(`SELECT session, lens, distilled FROM progress ORDER BY session, lens`),
		"meta":     q(`SELECT key, value FROM meta WHERE key NOT LIKE 'worker%' ORDER BY key`),
		"staged":   q(`SELECT session, obs_id FROM staged ORDER BY id`),
		"facets":   facets,
	}
}

// A dry run is the same transaction rolled back: it writes nothing, and its report is the one the
// apply then produces.
func TestMergeDryRunWritesNothingAndMatchesTheApply(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("s1", "user:a")
	base.obs("b1", "s1", "default", "2026-02-01T00:00:00Z")
	base.facets(mf("d", "k", "ours", 0.9, 1, "b1"))
	inc.raw("s2", "user:x")
	inc.obs("i1", "s2", "default", "2026-08-01T00:00:00Z")
	inc.cursor("default", 1)
	inc.facets(mf("d", "k", "theirs", 0.7, 1, "i1"))
	path := inc.export()

	before := snapshotArchive(t, base.st)
	dry := base.merge(path, false)
	if after := snapshotArchive(t, base.st); !reflect.DeepEqual(before, after) {
		t.Fatalf("a dry run wrote to the archive")
	}
	if dry.Empty() {
		t.Fatal("the fixture must have something to merge, or this test proves nothing")
	}
	applied := base.merge(path, true)
	if !reflect.DeepEqual(dry, applied) {
		t.Errorf("dry run and apply disagree:\ndry   %+v\napply %+v", dry, applied)
	}
}

// Only the per-session importer keys and emerge_seen marks cross over. Everything else in the
// other archive's meta is the other machine's state, and copying it does damage.
func TestMergeLeavesTheOtherMachinesStateBehind(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	_ = base.st.SetMetaString("runner_bound", "")
	for k, v := range map[string]string{
		"runner_bound":                  "1",
		"opencode_sync_time_updated_ms": "99999999999",
		"worker_stop_requested":         "1",
		"worker_pid":                    "424242",
		"profile_sig:default":           "theirs",
		"review_ts":                     "2026-12-31T00:00:00Z",
		"emerge_seen:default:abc":       "1",
	} {
		_ = inc.st.SetMetaString(k, v)
	}
	inc.raw("s2", "user:x") // something to merge
	base.merge(inc.export(), true)
	for _, k := range []string{"runner_bound", "opencode_sync_time_updated_ms", "worker_stop_requested", "worker_pid",
		"profile_sig:default", "review_ts"} {
		if got := base.st.MetaString(k); got != "" {
			t.Errorf("%s crossed over from the other machine (%q)", k, got)
		}
	}
	if base.st.MetaString("emerge_seen:default:abc") != "1" {
		t.Error("content-keyed emerge_seen marks should be unioned")
	}
}

func TestMergeRefusals(t *testing.T) {
	base := newArch(t)

	inc := newArch(t)
	inc.raw("s", "user:x")
	path := inc.export()
	if err := os.WriteFile(path+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := base.st.Merge(context.Background(), path, true, mergeNow); err != ErrMergeLiveArchive {
		t.Errorf("a database with a -wal beside it must be refused as live, got %v", err)
	}
	os.Remove(path + "-wal")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := base.st.Merge(context.Background(), path, true, mergeNow); err == nil || !strings.Contains(err.Error(), "newer witness") {
		t.Errorf("a database from a newer witness must be refused, got %v", err)
	}

	junk := filepath.Join(t.TempDir(), "not-witness.db")
	jdb, _ := sql.Open("sqlite", junk)
	_, _ = jdb.Exec(`CREATE TABLE x(a)`)
	jdb.Close()
	if _, err := base.st.Merge(context.Background(), junk, true, mergeNow); err == nil {
		t.Error("a database that is not a witness archive must be refused")
	}
}

// Nothing is lost on the way: embeddings arrive byte-identical, every version of every facet
// survives, and observations recorded in-session but not yet drained come along (each is the only
// copy).
func TestMergeKeepsEmbeddingsVersionsAndStaged(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	emb := []float32{0.125, -0.5, 0.75}
	if err := inc.st.AppendObservations([]Observation{{ID: "e1", Session: "s2", Lens: "default", Dimension: "d",
		Observation: "with a vector", Source: "mined", TS: "2026-03-01T00:00:00Z", Embedding: emb}}); err != nil {
		t.Fatal(err)
	}
	two := mf("d", "two_versions", "first", 0.5, 1)
	two.Versions[0].ValidTo = "2026-07-01T00:00:00Z"
	two.Versions = append(two.Versions, FacetVersion{Value: "second", ValidFrom: "2026-07-01T00:00:00Z", RecordedAt: "2026-07-01T00:00:00Z", Confidence: 0.6, ConfidenceSeq: 1})
	inc.facets(two)
	if err := inc.st.StageObservation(Observation{ID: "st1", Session: "s2", Lens: "default", Dimension: "d", Observation: "staged", Source: "active"}); err != nil {
		t.Fatal(err)
	}

	rep := base.merge(inc.export(), true)
	if rep.Staged != 1 {
		t.Errorf("want 1 staged observation carried, got %d", rep.Staged)
	}
	if base.st.StagedCount("s2") != 1 {
		t.Error("the staged observation was lost; it is the only copy")
	}
	obs, _ := base.st.ReadObservations("default")
	if len(obs) != 1 || !reflect.DeepEqual(obs[0].Embedding, emb) {
		t.Errorf("embedding did not survive: %+v", obs)
	}
	if f := base.facet("default", "d", "two_versions"); f == nil || len(f.Versions) != 2 || f.Versions[0].Value != "first" {
		t.Errorf("a facet's history was truncated: %+v", f)
	}
}

// A row the incoming side already FOLDED, that we hold but have not reviewed yet (a restore taken
// while our review lagged), is already inside the incoming facets. Moving it above the cursor with
// our other unreviewed rows would fold it a second time.
func TestMergeDoesNotRefoldWhatTheOtherSideFolded(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.obs("r1", "s1", "default", "2026-03-01T00:00:01Z")
	base.obs("shared", "s1", "default", "2026-03-01T00:00:02Z") // unreviewed HERE
	base.cursor("default", 1)
	inc.obs("shared", "s1", "default", "2026-03-01T00:00:02Z") // folded THERE
	inc.obs("i2", "s2", "default", "2026-03-01T00:00:03Z")
	inc.cursor("default", 2)

	base.merge(inc.export(), true)
	got, _ := base.st.ReadObservationsSinceOrdered("default", base.st.ReviewRowid("default"))
	for _, o := range got {
		if o.ID == "shared" {
			t.Error("an observation the other side already folded into its facets is up for review again")
		}
	}
}

// Progress: where we hold a session as PENDING (a progress row at 0 — imported, not yet mined)
// and the other side already mined it, theirs is taken, together with its observations. Keeping
// our 0 would re-mine it at model cost and, mining being non-deterministic, mint a duplicate L1.
func TestMergeTakesTheirProgressWhereOursIsPending(t *testing.T) {
	base, inc := newArch(t), newArch(t)
	base.raw("s1", "user:a", "assistant:b")
	if err := base.st.MarkDistilled("s1", "default", 0); err != nil {
		t.Fatal(err)
	}
	inc.raw("s1", "user:a", "assistant:b")
	inc.obs("io1", "s1", "default", "2026-03-01T00:00:00Z")
	if err := inc.st.MarkDistilled("s1", "default", 2); err != nil {
		t.Fatal(err)
	}
	base.merge(inc.export(), true)
	if got := base.st.DistilledCount("s1", "default"); got != 2 {
		t.Errorf("progress = %d, want their 2: our pending 0 would re-mine the session", got)
	}
	obs, _ := base.st.ReadObservations("default")
	if len(obs) != 1 || obs[0].ID != "io1" {
		t.Errorf("their observations of a session we had not mined must come across, got %v", idsOf(obs))
	}
}

// Lens names arrive as DATA, not through RegisterLens, so they get its rule. A lens named
// "unified" shares the cross-lens portrait's signature key; one with a slash fails WriteProfile
// after the model call, so the portrait regenerates — and is paid for — on every read.
func TestMergeRefusesInvalidLensNames(t *testing.T) {
	for _, bad := range []string{"unified", "a/b", "has space"} {
		base, inc := newArch(t), newArch(t)
		inc.obs("x", "s1", bad, "2026-03-01T00:00:00Z")
		if _, err := base.st.Merge(context.Background(), inc.export(), true, mergeNow); err == nil {
			t.Errorf("lens %q must be refused", bad)
		}
		if n, _ := base.st.ReadObservations(bad); len(n) != 0 {
			t.Errorf("lens %q: data arrived despite the refusal", bad)
		}
	}
}
