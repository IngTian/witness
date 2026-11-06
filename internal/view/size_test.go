package view

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// TestBuildSizeAt75kNotes builds the snapshot of an archive the size of the largest one witness
// is tuned for (a friend's: ~3,000 sessions, ~75k notes) and checks that the JSON the page
// embeds stays under the 50 MiB `witness view` warns at. Everything per-note stays out of the
// snapshot except three notes per trait, so the size should track traits and log rows, not
// notes; this is the test that would catch a field that starts carrying every note.
func TestBuildSizeAt75kNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 75k-note snapshot")
	}
	const (
		sessions, turnsPer = 3000, 20
		notes, traits      = 75000, 3000
		logRows            = 60000
		days               = 730
	)
	start := time.Date(2024, 11, 5, 0, 0, 0, 0, time.UTC)
	at := func(i, n int) time.Time {
		return start.Add(time.Duration(i) * (days * 24 * time.Hour / time.Duration(n)))
	}
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }
	lenses := []string{"default", "health", "math", "money", "work"}
	prose := func(n int) string { return strings.Repeat("A realistic sentence of archive prose. ", n) }

	in := &store.ViewInput{HighWater: notes}
	for i := 0; i < sessions*turnsPer; i++ {
		in.Turns = append(in.Turns, store.ViewTurn{Session: fmt.Sprintf("s%d", i/turnsPer), TS: rfc(at(i, sessions*turnsPer))})
	}
	for i := 0; i < notes; i++ {
		in.Notes = append(in.Notes, store.ViewNote{
			ID: fmt.Sprintf("obs-%06d", i), TS: rfc(at(i, notes)), Session: fmt.Sprintf("s%d", i*sessions/notes),
			Lens: lenses[i%len(lenses)], Dimension: "d", Observation: prose(5), Evidence: prose(4), Poignancy: i % 10,
		})
	}
	// Trait i cites notes i, i+3000, … i+24·3000 across three versions (10, 8, 7): every note
	// is cited once, each trait's notes span the whole two years (25 distinct weeks, the most
	// Weeks it can carry), and every trait has two closed versions to draw as spans.
	cite := func(i, k int) int { return i + k*traits }
	for i := 0; i < traits; i++ {
		f := store.Facet{Lens: lenses[i%len(lenses)], Dimension: fmt.Sprintf("dimension_%d", i%12),
			Key: fmt.Sprintf("a_trait_named_by_its_key_%d", i), LastSeen: rfc(at(cite(i, 24), notes))}
		cuts := []int{0, 10, 18, 25}
		for v := 0; v < 3; v++ {
			ver := store.FacetVersion{Value: prose(30), Claim: prose(3), ValidFrom: rfc(at(cite(i, cuts[v]), notes)),
				Confidence: 0.5 + float64(v)/10, ConfidenceSeq: int64(cite(i, cuts[v+1]-1))}
			if v < 2 {
				ver.ValidTo = rfc(at(cite(i, cuts[v+1]), notes))
			}
			for k := cuts[v]; k < cuts[v+1]; k++ {
				ver.BecauseOf = append(ver.BecauseOf, fmt.Sprintf("obs-%06d", cite(i, k)))
			}
			f.Versions = append(f.Versions, ver)
		}
		in.Facets = append(in.Facets, f)
	}
	for i := 0; i < logRows; i++ {
		f := &in.Facets[i%traits]
		in.Log = append(in.Log, store.ConfidenceEvent{Lens: f.Lens, Dimension: f.Dimension, Key: f.Key,
			At: rfc(at(i, logRows)), From: 0.5, To: float64(50+i%40) / 100, Kind: "review"})
	}

	// Six portraits (overall + one per notebook) of 40 sentences, each sentence citing three
	// traits of its notebook and the overall citing across all of them, with eight keywords.
	var files []PortraitFile
	for pi, id := range append([]string{store.ProfileUnified}, lenses...) {
		var md strings.Builder
		md.WriteString("# A portrait title\n")
		for s := 0; s < 40; s++ {
			if s%4 == 0 {
				fmt.Fprintf(&md, "\n**Lead sentence %d of this paragraph.**", s)
			} else {
				fmt.Fprintf(&md, " Then a plain sentence %d that says a little more about you.", s)
			}
		}
		src := store.PortraitSources{}
		for s, text := range distill.SplitSentences(md.String()) {
			ss := store.SourcedSentence{Text: text}
			for l := 0; l < 3; l++ {
				f := &in.Facets[(pi*500+s*3+l)%traits]
				if id != store.ProfileUnified {
					f = &in.Facets[(pi-1+(s*3+l)*len(lenses))%traits] // same lens as the portrait
				}
				ss.Links = append(ss.Links, store.TraitLink{Lens: f.Lens, Dimension: f.Dimension, Key: f.Key, Strength: store.LinkDirect})
			}
			src.Sentences = append(src.Sentences, ss)
		}
		for k := 0; k < 8; k++ {
			src.Keywords = append(src.Keywords, store.Keyword{Word: fmt.Sprintf("keyword%d", k), Sentences: []int{k, k + 8, k + 16, k + 24, k + 32}})
		}
		files = append(files, PortraitFile{ID: id, Markdown: md.String(), Written: start, Sources: src, SourcesOK: true})
	}

	began := time.Now()
	m := Build(in, files, start.Add(days*24*time.Hour), time.UTC)
	built := time.Since(began)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	// The fixture must actually be this big, or the size below proves nothing.
	if len(m.Traits) != traits || len(m.Portraits) != 6 {
		t.Fatalf("fixture: %d traits, %d portraits; want %d, 6", len(m.Traits), len(m.Portraits), traits)
	}
	for _, p := range m.Portraits {
		if !p.Sources || len(p.Links) != 40 || len(p.Links[39]) != 3 || len(p.Keywords) != 8 {
			t.Fatalf("portrait %s: sources %v, %d sentences; want every sentence linked", p.ID, p.Sources, len(p.Links))
		}
	}
	t.Logf("75k notes: JSON %d bytes (%.1f MiB), Build %v, Marshal %v", len(b), float64(len(b))/(1<<20), built, time.Since(began)-built)
	if len(b) > 50<<20 {
		t.Errorf("snapshot JSON is %d bytes, over the 50 MiB witness view warns at", len(b))
	}
}

// TestBuildNeverSetsE2EStart pins that Build never sets E2EStart; only the e2e test sets it.
func TestBuildNeverSetsE2EStart(t *testing.T) {
	in := &store.ViewInput{}
	m := Build(in, nil, time.Now(), time.UTC)
	if m.E2EStart != "" {
		t.Errorf("Build set E2EStart = %q, want empty", m.E2EStart)
	}
}
