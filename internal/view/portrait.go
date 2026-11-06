package view

import (
	"sort"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// buildPortraits turns the portrait files into reading columns. A notebook portrait is shown
// only when its notebook has an active trait, so every sentence has something it could cite;
// the overall one only when some notebook is shown, since it is written from them. Order is
// the left menu's: overall, notebooks by name, then corpus notebooks ("Other notebooks").
func buildPortraits(files []PortraitFile, notebooks []Notebook, traits []Trait, loc *time.Location) []Portrait {
	ids := make(map[facetID]int, len(traits))
	for id, t := range traits {
		ids[facetID{t.Lens, t.Dimension, t.Key}] = id
	}
	corpus := make(map[string]bool, len(notebooks))
	for _, nb := range notebooks {
		corpus[nb.Name] = nb.Corpus
	}

	rank := map[string]int{"overall": 0, "notebook": 1, "other": 2}
	out := []Portrait{}
	for _, f := range files {
		kind := "overall"
		if f.ID != store.ProfileUnified {
			c, ok := corpus[f.ID]
			if !ok {
				continue
			}
			kind = "notebook"
			if c {
				kind = "other"
			}
		} else if len(notebooks) == 0 {
			continue
		}
		out = append(out, buildPortrait(f, kind, ids, loc))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func buildPortrait(f PortraitFile, kind string, ids map[facetID]int, loc *time.Location) Portrait {
	blocks, n := portraitBlocks(f.Markdown)
	p := Portrait{
		ID: f.ID, Kind: kind, Title: f.ID,
		Written: f.Written.In(loc).Format(time.DateOnly), Stale: f.Stale,
		Blocks: blocks, Links: make([][]int, n), Keywords: []Keyword{},
	}
	// Only a LEADING heading is the portrait's title (both prompts open with one). The
	// overall's becomes Title; a notebook's is dropped, since the page gives it a plain heading
	// of its name (spec §4.2). A heading anywhere later is a section and stays in the column.
	if kind == "overall" {
		p.Title = ""
	}
	if len(blocks) > 0 && blocks[0].Kind == "h" {
		if kind == "overall" {
			p.Title = blocks[0].Text
		}
		p.Blocks = blocks[1:]
	}

	// The hash in CurrentPortraitSources proves the sources describe THIS text; the count
	// proves they were numbered by THIS splitter. A splitter change between the resolver run
	// and now would shift every link onto the wrong sentence, which is worse than none.
	p.Sources = f.SourcesOK && len(f.Sources.Sentences) == n
	for i := range p.Links {
		p.Links[i] = []int{}
		if p.Sources {
			p.Links[i] = linkIDs(f.Sources.Sentences[i].Links, ids)
		}
	}
	if p.Sources {
		for _, k := range f.Sources.Keywords[:min(8, len(f.Sources.Keywords))] {
			kw := Keyword{Word: k.Word, Sentences: []int{}}
			for _, s := range k.Sentences {
				if s >= 0 && s < n {
					kw.Sentences = append(kw.Sentences, s)
				}
			}
			p.Keywords = append(p.Keywords, kw)
		}
	}
	return p
}

// linkIDs maps one sentence's links to active trait ids, deduped by DedupeLinks (a key cited
// both ways counts as direct). It returns the live direct links, or, only when none is live,
// the partial ones (spec §7.2: partials are look-alike padding next to a real support), at
// most three. Dead keys are dropped FIRST, so a direct link to a retired trait never hides
// the live partial ones.
func linkIDs(links []store.TraitLink, ids map[facetID]int) []int {
	var direct, partial []int
	for _, l := range store.DedupeLinks(links) {
		id, ok := ids[facetID{l.Lens, l.Dimension, l.Key}]
		switch {
		case !ok:
		case l.Strength == store.LinkDirect:
			direct = append(direct, id)
		default:
			partial = append(partial, id)
		}
	}
	if len(direct) == 0 {
		direct = partial
	}
	return append([]int{}, direct[:min(3, len(direct))]...)
}

// portraitBlocks rebuilds the headings, paragraphs and list items that SplitSentences reads,
// keeping what it throws away (headings, bold), and returns them with the sentence count.
// The line loop mirrors SplitSentences' block pass exactly; TestPortraitSentencesMatchSplitter
// pins that the sentences come out identical, in order, since stored links index them.
func portraitBlocks(md string) ([]Block, int) {
	blocks := []Block{}
	n := 0
	var para []string
	emit := func(kind, raw string) {
		// SplitSentences picks block kinds from the text, so a block passed in bare could be
		// re-read as something else: "**#1** first" is a heading once its ** is gone, "- x" a
		// nested list item. "- "+raw is always ONE list item whose text is exactly raw, which
		// is precisely the split SplitSentences(md) made of this block.
		sents := distill.SplitSentences("- " + raw)
		if len(sents) == 0 {
			return // every sentence too short to keep: nothing to number or click
		}
		plain, bold := unbold(raw)
		b := Block{Kind: kind, Sentences: make([]Sentence, 0, len(sents))}
		at := 0
		for _, s := range sents {
			b.Sentences = append(b.Sentences, Sentence{N: n, Runs: runs(plain, bold, s, &at)})
			n++
		}
		blocks = append(blocks, b)
	}
	flush := func() {
		if len(para) > 0 {
			emit("p", strings.Join(para, " "))
			para = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			flush()
		case strings.HasPrefix(t, "#"):
			flush()
			text := strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(t, "#"), "**", ""))
			blocks = append(blocks, Block{Kind: "h", Text: text, Sentences: []Sentence{}})
		case distill.IsListItem(t):
			flush()
			emit("li", distill.StripListMarker(t))
		default:
			para = append(para, t)
		}
	}
	flush()
	return blocks, n
}

// unbold removes ** the way SplitSentences does (strings.ReplaceAll: left to right, never
// overlapping, so "***" leaves one "*") and marks each remaining byte bold or not. Bold
// toggles only between ASCII bytes, so a multi-byte rune is never split across runs.
func unbold(raw string) (string, []bool) {
	var b strings.Builder
	mask := make([]bool, 0, len(raw))
	on := false
	for i := 0; i < len(raw); i++ {
		if strings.HasPrefix(raw[i:], "**") {
			on = !on
			i++
			continue
		}
		b.WriteByte(raw[i])
		mask = append(mask, on)
	}
	return b.String(), mask
}

// runs finds sentence s in plain at or after *at (sentences come in order, so a cursor stops
// an earlier repeat of the same words from matching) and cuts it where bold changes. A
// sentence that is not there — the splitter rebuilt an invalid UTF-8 byte as U+FFFD — still
// shows, as one plain run: dropping it would shift every later sentence off its links.
func runs(plain string, bold []bool, s string, at *int) []Run {
	i := strings.Index(plain[*at:], s)
	if i < 0 {
		return []Run{{Text: s}}
	}
	start, end := *at+i, *at+i+len(s)
	*at = end
	out := []Run{}
	for j := start; j < end; {
		k := j
		for k < end && bold[k] == bold[j] {
			k++
		}
		out = append(out, Run{Text: plain[j:k], Bold: bold[j]})
		j = k
	}
	return out
}
