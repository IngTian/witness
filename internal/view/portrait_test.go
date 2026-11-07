package view

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/sentence"
	"github.com/IngTian/witness/internal/store"
)

// portraitInput is an archive whose active traits default/habits a..e have ids 0..4 (same
// arrival, so they sort by name), plus an archived trait "gone".
func portraitInput() *store.ViewInput {
	f := func(key, archived string) store.Facet {
		return store.Facet{Lens: "default", Dimension: "habits", Key: key, LastSeen: "2026-03-01T12:00:00Z",
			ArchivedAt: archived,
			Versions:   []store.FacetVersion{{ValidFrom: "2026-03-01T12:00:00Z", Confidence: 0.5}}}
	}
	return &store.ViewInput{Facets: []store.Facet{
		f("a", ""), f("b", ""), f("c", ""), f("d", ""), f("e", ""), f("gone", "2026-03-02T00:00:00Z"),
	}}
}

func link(key, strength string) store.TraitLink {
	return store.TraitLink{Lens: "default", Dimension: "habits", Key: key, Strength: strength}
}

// onePortrait builds a single portrait through Build, so it goes through the same filtering,
// trait-id mapping and ordering the page gets.
func onePortrait(t *testing.T, in *store.ViewInput, f PortraitFile) Portrait {
	t.Helper()
	m := Build(in, []PortraitFile{f}, time.Date(2026, 3, 31, 0, 0, 0, 0, est), est)
	if len(m.Portraits) != 1 {
		t.Fatalf("portraits: got %d, want 1", len(m.Portraits))
	}
	return m.Portraits[0]
}

// sentenceTexts reads the sentences back off the blocks in order, checking that N counts
// 0, 1, 2… across blocks and that Links has one entry per sentence.
func sentenceTexts(t *testing.T, p Portrait) []string {
	t.Helper()
	out := []string{}
	for _, b := range p.Blocks {
		for _, s := range b.Sentences {
			if s.N != len(out) {
				t.Errorf("sentence %+v: N = %d, want %d (numbering runs across blocks)", s.Runs, s.N, len(out))
			}
			var txt strings.Builder
			for _, r := range s.Runs {
				txt.WriteString(r.Text)
			}
			out = append(out, txt.String())
		}
	}
	if len(p.Links) != len(out) {
		t.Errorf("links: got %d entries, want one per sentence (%d)", len(p.Links), len(out))
	}
	return out
}

// wantSplit pins THE invariant: the page's sentences are exactly sentence.SplitSentences', in
// order, because the stored links are indexed by that split.
func wantSplit(t *testing.T, p Portrait, md string) {
	t.Helper()
	if got, want := sentenceTexts(t, p), sentence.SplitSentences(md); !reflect.DeepEqual(got, want) {
		t.Errorf("sentences:\n got  %q\n want %q", got, want)
	}
}

func wantBlocks(t *testing.T, got, want []Block) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("blocks:\n got  %s\n want %s", g, w)
	}
}

func TestPortraitBlocksAndBoldRuns(t *testing.T) {
	md := "# Title\n\n**Lead one.** Rest here. Two.\n\n- Item **x** y."
	p := onePortrait(t, portraitInput(), PortraitFile{ID: store.ProfileUnified, Markdown: md})
	if p.Title != "Title" {
		t.Errorf("Title: got %q, want %q", p.Title, "Title")
	}
	wantBlocks(t, p.Blocks, []Block{
		{Kind: "p", Sentences: []Sentence{
			{N: 0, Runs: []Run{{"Lead one.", true}}},
			{N: 1, Runs: []Run{{"Rest here.", false}}},
			{N: 2, Runs: []Run{{"Two.", false}}},
		}},
		{Kind: "li", Sentences: []Sentence{
			{N: 3, Runs: []Run{{"Item ", false}, {"x", true}, {" y.", false}}},
		}},
	})
	wantSplit(t, p, md)
}

// Every way a block could be re-read differently from SplitSentences' own pass: a bold span
// that, once its ** is gone, starts with "#"; list items whose text is itself a list marker
// or a heading; numbered and "*" lists; CRLF; a paragraph spanning lines; abbreviations and
// decimals; a paragraph whose only sentence is too short to keep.
func TestPortraitSentencesMatchSplitter(t *testing.T) {
	md := "# Me\r\n\r\n## **Bold** heading\n" +
		"**#1 priority.** Ships it, e.g. today. Score 97.7% now.\n" +
		"Continues here.\n\n" +
		"A.\n\n" +
		"- - Nested marker.\n" +
		"- # Not a heading.\n" +
		"* Star item. Two here.\n" +
		"12. Numbered item.\n" +
		"“Quoted start.” 中文句子。第二句。"
	p := onePortrait(t, portraitInput(), PortraitFile{ID: store.ProfileUnified, Markdown: md})
	wantSplit(t, p, md)
	kinds := []string{}
	for _, b := range p.Blocks {
		kinds = append(kinds, b.Kind)
	}
	if want := []string{"h", "p", "li", "li", "li", "li", "p"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("block kinds: got %v, want %v", kinds, want)
	}
	if p.Blocks[0].Text != "Bold heading" {
		t.Errorf("heading text: got %q, want %q", p.Blocks[0].Text, "Bold heading")
	}
	if got := p.Blocks[1].Sentences[0].Runs; !reflect.DeepEqual(got, []Run{{"#1 priority.", true}}) {
		t.Errorf("bold lead: got %+v", got)
	}
}

func TestPortraitBoldRunsChinese(t *testing.T) {
	md := "我喜欢**写代码**。然后**休息一下**，再继续。"
	p := onePortrait(t, portraitInput(), PortraitFile{ID: store.ProfileUnified, Markdown: md})
	wantBlocks(t, p.Blocks, []Block{{Kind: "p", Sentences: []Sentence{
		{N: 0, Runs: []Run{{"我喜欢", false}, {"写代码", true}, {"。", false}}},
		{N: 1, Runs: []Run{{"然后", false}, {"休息一下", true}, {"，再继续。", false}}},
	}}})
	wantSplit(t, p, md)
}

func TestPortraitBoldAcrossSentences(t *testing.T) {
	md := "Start **bold end. Bold start** rest. Plain."
	p := onePortrait(t, portraitInput(), PortraitFile{ID: store.ProfileUnified, Markdown: md})
	wantBlocks(t, p.Blocks, []Block{{Kind: "p", Sentences: []Sentence{
		{N: 0, Runs: []Run{{"Start ", false}, {"bold end.", true}}},
		{N: 1, Runs: []Run{{"Bold start", true}, {" rest.", false}}},
		{N: 2, Runs: []Run{{"Plain.", false}}},
	}}})
	wantSplit(t, p, md)
}

// The same words twice: the second is found AFTER the first, so it keeps its own (plain) runs.
func TestPortraitRepeatedSentence(t *testing.T) {
	md := "**Again.** Again."
	p := onePortrait(t, portraitInput(), PortraitFile{ID: store.ProfileUnified, Markdown: md})
	wantBlocks(t, p.Blocks, []Block{{Kind: "p", Sentences: []Sentence{
		{N: 0, Runs: []Run{{"Again.", true}}},
		{N: 1, Runs: []Run{{"Again.", false}}},
	}}})
	wantSplit(t, p, md)
}

// A sentence the splitter returns but that is not in the block text (it rebuilt an invalid
// UTF-8 byte as U+FFFD) still shows, as one plain run, and the next sentence keeps its bold:
// dropping it would shift every later sentence off its links.
func TestPortraitUnlocatableSentence(t *testing.T) {
	md := "Bad \xff byte here. **Next one.**"
	p := onePortrait(t, portraitInput(), PortraitFile{ID: store.ProfileUnified, Markdown: md})
	wantBlocks(t, p.Blocks, []Block{{Kind: "p", Sentences: []Sentence{
		{N: 0, Runs: []Run{{"Bad \uFFFD byte here.", false}}},
		{N: 1, Runs: []Run{{"Next one.", true}}},
	}}})
	wantSplit(t, p, md)
}

func TestPortraitLinksMapAndCap(t *testing.T) {
	md := "First sentence here. Second sentence here. Third sentence here. Fourth sentence here."
	p := onePortrait(t, portraitInput(), PortraitFile{ID: "default", Markdown: md, SourcesOK: true,
		Sources: store.PortraitSources{Sentences: []store.SourcedSentence{
			// The archived trait is dropped; the live direct c is shown, its partials are not.
			{Text: "First sentence here.", Links: []store.TraitLink{
				link("a", store.LinkPartial), link("gone", store.LinkDirect),
				link("b", store.LinkPartial), link("c", store.LinkDirect)}},
			// Two live directs: both, and none of the three partials.
			{Text: "Second sentence here.", Links: []store.TraitLink{
				link("a", store.LinkPartial), link("b", store.LinkPartial), link("c", store.LinkDirect),
				link("d", store.LinkDirect), link("e", store.LinkPartial)}},
			// No direct link: the live partials show, three of four (the dead one never).
			{Text: "Third sentence here.", Links: []store.TraitLink{
				link("a", store.LinkPartial), link("gone", store.LinkPartial), link("b", store.LinkPartial),
				link("d", store.LinkPartial), link("e", store.LinkPartial)}},
			// The only direct link is dead: it must not hide the live partials.
			{Text: "Fourth sentence here.", Links: []store.TraitLink{
				link("gone", store.LinkDirect), link("b", store.LinkPartial), link("e", store.LinkPartial)}},
		}}})
	if !p.Sources {
		t.Fatalf("Sources: got false, want true")
	}
	if want := [][]int{{2}, {2, 3}, {0, 1, 3}, {1, 4}}; !reflect.DeepEqual(p.Links, want) {
		t.Errorf("links: got %v, want %v", p.Links, want)
	}
}

func TestPortraitWithoutSources(t *testing.T) {
	// A mid-portrait heading (a leading one is dropped) so the JSON shows a heading's [].
	md := "First sentence here.\n\n## Later\n\nSecond sentence here."
	full := store.PortraitSources{
		Sentences: []store.SourcedSentence{
			{Text: "First sentence here.", Links: []store.TraitLink{link("a", store.LinkDirect)}},
			{Text: "Second sentence here.", Links: []store.TraitLink{link("b", store.LinkDirect)}},
		},
		Keywords: []store.Keyword{{Word: "first", Sentences: []int{0}}},
	}
	// Both written against a different split of this text: one fewer sentence, one more.
	short, long := full, full
	short.Sentences = full.Sentences[:1]
	long.Sentences = append(append([]store.SourcedSentence{}, full.Sentences...),
		store.SourcedSentence{Text: "Third.", Links: []store.TraitLink{link("c", store.LinkDirect)}})
	for _, c := range []struct {
		name string
		src  store.PortraitSources
		ok   bool
	}{
		{"not recorded", full, false},
		{"fewer sentences stored", short, true},
		{"more sentences stored", long, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := onePortrait(t, portraitInput(), PortraitFile{ID: "default", Markdown: md, Sources: c.src, SourcesOK: c.ok})
			if p.Sources {
				t.Errorf("Sources: got true, want false")
			}
			if want := [][]int{{}, {}}; !reflect.DeepEqual(p.Links, want) {
				t.Errorf("links: got %v, want %v", p.Links, want)
			}
			if len(p.Keywords) != 0 {
				t.Errorf("keywords: got %v, want none", p.Keywords)
			}
			b, _ := json.Marshal(p)
			if s := string(b); strings.Contains(s, "null") || !strings.Contains(s, `"links":[[],[]]`) ||
				!strings.Contains(s, `"keywords":[]`) || !strings.Contains(s, `"sentences":[]`) {
				t.Errorf("JSON must use [] for every empty list: %s", s)
			}
		})
	}
}

// Only a LEADING heading is a title: a later one is a section and stays, for every kind.
func TestPortraitLeadingHeading(t *testing.T) {
	h := func(text string) Block { return Block{Kind: "h", Text: text, Sentences: []Sentence{}} }
	para := func(n int, text string) Block {
		return Block{Kind: "p", Sentences: []Sentence{{N: n, Runs: []Run{{text, false}}}}}
	}
	for _, c := range []struct {
		name, id, md, title string
		blocks              []Block
	}{
		{"notebook drops its leading heading", "default",
			"# Default lens\n\nText here.\n\n## Section\n\nMore here.",
			"default", []Block{para(0, "Text here."), h("Section"), para(1, "More here.")}},
		{"overall opening with a paragraph keeps its later heading", store.ProfileUnified,
			"Opening words here.\n\n# Later heading\n\nMore here.",
			"", []Block{para(0, "Opening words here."), h("Later heading"), para(1, "More here.")}},
		{"overall's leading heading is its title", store.ProfileUnified,
			"# Who you are\n\nText here.\n\n## Section\n\nMore here.",
			"Who you are", []Block{para(0, "Text here."), h("Section"), para(1, "More here.")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := onePortrait(t, portraitInput(), PortraitFile{ID: c.id, Markdown: c.md})
			if p.Title != c.title {
				t.Errorf("Title: got %q, want %q", p.Title, c.title)
			}
			wantBlocks(t, p.Blocks, c.blocks)
			wantSplit(t, p, c.md)
		})
	}
}

// A single-digit numbered item straight after a paragraph line starts its own block, exactly
// as SplitSentences reads it (the view walks the splitter's own blocks, not a copy).
func TestPortraitNumberedItemAfterParagraph(t *testing.T) {
	md := "Intro line\n1. Item."
	p := onePortrait(t, portraitInput(), PortraitFile{ID: "default", Markdown: md})
	wantSplit(t, p, md)
	if len(p.Blocks) != 2 || p.Blocks[0].Kind != "p" || p.Blocks[1].Kind != "li" {
		t.Errorf("blocks: got %+v, want a paragraph then a list item", p.Blocks)
	}
}

func TestPortraitKeywords(t *testing.T) {
	md := "First sentence here. Second sentence here."
	kws := []store.Keyword{{Word: "edge", Sentences: []int{-1, 0, 2, 1}}}
	for _, w := range []string{"b", "c", "d", "e", "f", "g", "h", "ninth"} {
		kws = append(kws, store.Keyword{Word: w, Sentences: []int{1}})
	}
	p := onePortrait(t, portraitInput(), PortraitFile{ID: "default", Markdown: md, SourcesOK: true,
		Sources: store.PortraitSources{
			Sentences: []store.SourcedSentence{{Text: "First sentence here."}, {Text: "Second sentence here."}},
			Keywords:  kws,
		}})
	if len(p.Keywords) != 8 || p.Keywords[7].Word != "h" {
		t.Fatalf("keywords: got %v, want the first 8", p.Keywords)
	}
	if got := p.Keywords[0]; !reflect.DeepEqual(got, Keyword{"edge", []int{0, 1}}) {
		t.Errorf("out-of-range sentence numbers: got %+v, want [0 1] kept", got)
	}
}

func TestPortraitSourcesTextMismatch(t *testing.T) {
	md := "First sentence here. Second sentence here."
	// Count matches (2 sentences), but the stored text differs from the view's split
	p := onePortrait(t, portraitInput(), PortraitFile{
		ID: "default", Markdown: md, SourcesOK: true,
		Sources: store.PortraitSources{
			Sentences: []store.SourcedSentence{
				{Text: "First sentence here.", Links: []store.TraitLink{link("a", store.LinkDirect)}},
				{Text: "Different text here.", Links: []store.TraitLink{link("b", store.LinkDirect)}}, // mismatch
			},
		}})
	if p.Sources {
		t.Errorf("Sources: got true, want false (text mismatch)")
	}
	if want := [][]int{{}, {}}; !reflect.DeepEqual(p.Links, want) {
		t.Errorf("links: got %v, want %v (no links when Sources false)", p.Links, want)
	}
}

func TestPortraitKinds(t *testing.T) {
	in := &store.ViewInput{
		Notes: []store.ViewNote{
			{ID: "n1", TS: "2026-03-01T10:00:00Z", Session: "s1", Lens: "default"},
			{ID: "n2", TS: "2026-03-01T10:00:00Z", Session: "file:/feed.md", Lens: "market"},
			{ID: "n3", TS: "2026-03-01T10:00:00Z", Session: "s1", Lens: "zeta"},
		},
	}
	for _, l := range []struct{ lens, note string }{{"default", "n1"}, {"market", "n2"}, {"zeta", "n3"}} {
		in.Facets = append(in.Facets, store.Facet{Lens: l.lens, Dimension: "habits", Key: "k", LastSeen: "2026-03-01T12:00:00Z",
			Versions: []store.FacetVersion{{ValidFrom: "2026-03-01T12:00:00Z", BecauseOf: []string{l.note}, Confidence: 0.5}}})
	}
	// 03:00 UTC is still the previous evening in EST: Written is the LOCAL date of the mtime.
	mtime := time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC)
	files := []PortraitFile{
		{ID: "market", Markdown: "# Market\n\nPrices rose."},
		{ID: "zeta", Markdown: "# Zeta\n\nZeta text."},
		{ID: "orphan", Markdown: "# Orphan\n\nNo traits back this."}, // no active trait: not shown
		{ID: store.ProfileUnified, Markdown: "# Who you are\n\nYou ship.", Written: mtime, Stale: true},
		{ID: "default", Markdown: "# Default lens\n\nDefault text."},
	}
	m := Build(in, files, time.Date(2026, 3, 31, 0, 0, 0, 0, est), est)
	type row struct{ ID, Kind, Title string }
	got := []row{}
	for _, p := range m.Portraits {
		got = append(got, row{p.ID, p.Kind, p.Title})
	}
	want := []row{
		{store.ProfileUnified, "overall", "Who you are"},
		{"default", "notebook", "default"},
		{"zeta", "notebook", "zeta"},
		{"market", "other", "market"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("portraits:\n got  %+v\n want %+v", got, want)
	}
	if u := m.Portraits[0]; u.Written != "2026-03-04" || !u.Stale || len(u.Blocks) != 1 || u.Blocks[0].Kind != "p" {
		t.Errorf("overall: written %q stale %v blocks %+v; want 2026-03-04, true, its heading lifted into Title", u.Written, u.Stale, u.Blocks)
	}
	// Every kind loses its leading heading: notebooks and corpora get a plain one of their name.
	for _, p := range m.Portraits {
		if len(p.Blocks) != 1 || p.Blocks[0].Kind != "p" {
			t.Errorf("%s blocks: got %+v, want only its paragraph", p.ID, p.Blocks)
		}
	}

	// The overall portrait is written from notebook portraits: with no notebook, it backs nothing.
	if m := Build(&store.ViewInput{}, files[3:4], time.Date(2026, 3, 31, 0, 0, 0, 0, est), est); len(m.Portraits) != 0 {
		t.Errorf("no notebooks: got %d portraits, want 0", len(m.Portraits))
	}
}
