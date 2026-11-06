//go:build e2e

package view

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

var e2eOut = flag.String("out", "", "output directory for e2e fixtures; skipped if empty")

// TestWriteE2EFixtures builds four HTML files for the headless-Chrome check: full (every
// feature), full-changed (E2EStart = "changed"), empty (empty archive), and hostile (Review Focus
// 2 text in every string field). The fixtures are synthetic; they never contain archive text.
func TestWriteE2EFixtures(t *testing.T) {
	if *e2eOut == "" {
		t.Skip("-out not provided")
	}

	loc := time.UTC
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, loc)

	// Hostile text from Review Focus 2: </script>, HTML comment, bold, ampersand, Chinese,
	// U+2028 (line separator), U+2029 (paragraph separator), plus a unique probe tag.
	hostile := "</script></SCRIPT><!--<b>& 中文  <witness-probe>"

	// full.html: a synthetic archive with every feature.
	fullIn := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: "s1", TS: "2026-10-01T10:00:00Z"},
			{Session: "s1", TS: "2026-10-01T10:05:00Z"},
			{Session: "s2", TS: "2026-10-15T14:00:00Z"},
			{Session: "s2", TS: "2026-10-15T14:10:00Z"},
			{Session: "s3", TS: "2026-11-01T09:00:00Z"},
		},
		Notes: []store.ViewNote{
			{ID: "n1", TS: "2026-10-01T10:00:00Z", Session: "s1", Lens: "default", Dimension: "habits", Observation: "Observation one.", Evidence: "Evidence one.", Poignancy: 8},
			{ID: "n2", TS: "2026-10-01T10:05:00Z", Session: "s1", Lens: "default", Dimension: "habits", Observation: "Observation two.", Evidence: "Evidence two.", Poignancy: 6},
			{ID: "n3", TS: "2026-10-15T14:00:00Z", Session: "s2", Lens: "default", Dimension: "habits", Observation: "Observation three.", Evidence: "Evidence three.", Poignancy: 7},
			{ID: "n4", TS: "2026-10-15T14:10:00Z", Session: "file:ingested-2", Lens: "corpus", Dimension: "themes", Observation: "Corpus observation.", Evidence: "Corpus evidence.", Poignancy: 5},
			{ID: "n5", TS: "2026-11-01T09:00:00Z", Session: "s3", Lens: "default", Dimension: "biases", Observation: "Observation five.", Evidence: "Evidence five.", Poignancy: 9},
			{ID: "n6", TS: "2026-11-01T09:05:00Z", Session: "file:ingested", Lens: "corpus", Dimension: "themes", Observation: "File observation.", Evidence: "File evidence.", Poignancy: 4},
		},
		Facets: []store.Facet{
			{
				Lens: "default", Dimension: "habits", Key: "asks_direct_questions", LastSeen: "2026-10-15T14:10:00Z",
				Versions: []store.FacetVersion{
					{Value: "Old version.", ValidFrom: "2026-10-01T10:00:00Z", ValidTo: "2026-10-10T00:00:00Z", BecauseOf: []string{"n1"}, Confidence: 0.6},
					{Value: "New version.", Claim: "Asks direct questions.", ValidFrom: "2026-10-10T00:00:00Z", BecauseOf: []string{"n1", "n2", "n3"}, Confidence: 0.9, ConfidenceSeq: 1000},
				},
			},
			{
				Lens: "default", Dimension: "biases", Key: "confirmation_bias", LastSeen: "2026-11-01T09:00:00Z",
				Versions: []store.FacetVersion{
					{Value: "Confirmation bias trait.", Claim: "Looks for confirming evidence.", ValidFrom: "2026-11-01T09:00:00Z", BecauseOf: []string{"n5"}, Confidence: 0.85, ConfidenceSeq: 2000},
				},
			},
			{
				Lens: "default", Dimension: "habits", Key: "formed_trait", LastSeen: "2026-10-15T14:10:00Z",
				Versions: []store.FacetVersion{
					{Value: "Formed by compaction.", Claim: "Formed trait.", ValidFrom: "2026-10-01T10:00:00Z", BecauseOf: []string{"n2"}, Confidence: 0.8, ConfidenceSeq: 1500},
				},
			},
			{
				Lens: "default", Dimension: "habits", Key: "merged_into_formed", LastSeen: "2026-10-12T00:00:00Z", ArchivedAt: "2026-10-12T00:00:00Z", MergedInto: "formed_trait",
				Versions: []store.FacetVersion{
					{ValidFrom: "2026-10-01T10:00:00Z", Confidence: 0.5},
				},
			},
			{
				Lens: "corpus", Dimension: "themes", Key: "recurring_theme", LastSeen: "2026-11-01T09:05:00Z",
				Versions: []store.FacetVersion{
					{Value: "Corpus trait.", Claim: "A recurring theme.", ValidFrom: "2026-10-15T14:00:00Z", BecauseOf: []string{"n4", "n6"}, Confidence: 0.75, ConfidenceSeq: 1800},
				},
			},
			{
				Lens: "default", Dimension: "workstyle", Key: "no_summary_trait", LastSeen: "2026-11-01T09:00:00Z",
				Versions: []store.FacetVersion{
					{Value: "A trait with no summary.", Claim: "", ValidFrom: "2026-11-01T09:00:00Z", BecauseOf: []string{"n5"}, Confidence: 0.7, ConfidenceSeq: 1900},
				},
			},
		},
		Log: []store.ConfidenceEvent{
			{Lens: "default", Dimension: "habits", Key: "asks_direct_questions", At: "2026-10-20T12:00:00Z", From: 0.9, To: 0.85, Kind: "review"},
			{Lens: "default", Dimension: "habits", Key: "asks_direct_questions", At: "2026-10-25T12:00:00Z", From: 0.85, To: 0.9, Kind: "review"},
			{Lens: "default", Dimension: "biases", Key: "confirmation_bias", At: "2026-11-02T12:00:00Z", From: 0.85, To: 0.88, Kind: "review"},
		},
		HighWater: 3000,
	}

	// Three portraits: the overall one, `default` (sources not recorded) and `corpus`. Every
	// note of the corpus lens (n4, n6) comes from a file: session, so its portrait is kind
	// "other" and the page lists it under "Other notebooks", never as "who you are".
	fullPortraits := []PortraitFile{
		{
			ID:       store.ProfileUnified,
			Markdown: "# Overall Portrait\n\n**Lead sentence of the overall profile.** More about you. Another sentence.\n\n## A section\n\nAnother paragraph here.",
			Written:  now,
			Stale:    false,
			Sources: store.PortraitSources{
				Sentences: []store.SourcedSentence{
					{Text: "Lead sentence of the overall profile.", Links: []store.TraitLink{
						{Lens: "default", Dimension: "habits", Key: "asks_direct_questions", Strength: store.LinkDirect},
					}},
					{Text: "More about you."},
					{Text: "Another sentence.", Links: []store.TraitLink{
						{Lens: "default", Dimension: "biases", Key: "confirmation_bias", Strength: store.LinkPartial},
					}},
					{Text: "Another paragraph here."},
				},
				Keywords: []store.Keyword{
					{Word: "direct", Sentences: []int{0, 2}},
					{Word: "questions", Sentences: []int{0}},
				},
			},
			SourcesOK: true,
		},
		{
			ID:       "corpus",
			Markdown: "# Corpus Notebook\n\nA corpus notebook sentence.",
			Written:  now,
			Stale:    true,
			Sources: store.PortraitSources{
				Sentences: []store.SourcedSentence{
					{Text: "A corpus notebook sentence.", Links: []store.TraitLink{
						{Lens: "corpus", Dimension: "themes", Key: "recurring_theme", Strength: store.LinkDirect},
					}},
				},
			},
			SourcesOK: true,
		},
		{
			ID:        "default",
			Markdown:  "# Default Notebook\n\nA portrait without sources.",
			Written:   now,
			Stale:     false,
			Sources:   store.PortraitSources{},
			SourcesOK: false,
		},
	}

	// Build full.html
	fullModel := Build(fullIn, fullPortraits, now, loc)
	fullHTML, err := Render(fullModel)
	if err != nil {
		t.Fatalf("Render full: %v", err)
	}
	if err := os.WriteFile(filepath.Join(*e2eOut, "full.html"), fullHTML, 0644); err != nil {
		t.Fatalf("write full.html: %v", err)
	}

	// full-changed.html: same model but E2EStart = "changed"
	fullModel.E2EStart = "changed"
	fullChangedHTML, err := Render(fullModel)
	if err != nil {
		t.Fatalf("Render full-changed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(*e2eOut, "full-changed.html"), fullChangedHTML, 0644); err != nil {
		t.Fatalf("write full-changed.html: %v", err)
	}

	// empty.html: empty archive
	emptyModel := Build(&store.ViewInput{}, nil, now, loc)
	emptyHTML, err := Render(emptyModel)
	if err != nil {
		t.Fatalf("Render empty: %v", err)
	}
	if err := os.WriteFile(filepath.Join(*e2eOut, "empty.html"), emptyHTML, 0644); err != nil {
		t.Fatalf("write empty.html: %v", err)
	}

	// hostile.html: Review Focus 2 text in every string field
	hostileIn := &store.ViewInput{
		Turns: []store.ViewTurn{
			{Session: hostile, TS: "2026-11-01T10:00:00Z"},
		},
		Notes: []store.ViewNote{
			{ID: "h1", TS: "2026-11-01T10:00:00Z", Session: hostile, Lens: hostile, Dimension: hostile, Observation: hostile, Evidence: hostile, Poignancy: 5},
		},
		Facets: []store.Facet{
			{
				Lens: hostile, Dimension: hostile, Key: hostile, LastSeen: "2026-11-01T10:00:00Z",
				Versions: []store.FacetVersion{
					{Value: hostile, Claim: hostile, ValidFrom: "2026-11-01T10:00:00Z", BecauseOf: []string{"h1"}, Confidence: 0.8, ConfidenceSeq: 100},
				},
			},
		},
		HighWater: 100,
	}
	hostilePortrait := []PortraitFile{
		{
			ID:       hostile,
			Markdown: fmt.Sprintf("# %s\n\n%s", hostile, hostile),
			Written:  now,
			Stale:    false,
			Sources: store.PortraitSources{
				Sentences: []store.SourcedSentence{
					{Text: hostile, Links: []store.TraitLink{
						{Lens: hostile, Dimension: hostile, Key: hostile, Strength: store.LinkDirect},
					}},
				},
				Keywords: []store.Keyword{
					{Word: hostile, Sentences: []int{0}},
				},
			},
			SourcesOK: true,
		},
	}
	hostileModel := Build(hostileIn, hostilePortrait, now, loc)
	hostileHTML, err := Render(hostileModel)
	if err != nil {
		t.Fatalf("Render hostile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(*e2eOut, "hostile.html"), hostileHTML, 0644); err != nil {
		t.Fatalf("write hostile.html: %v", err)
	}

	// Verify distill.SplitSentences works the same way the test helpers expect.
	// This is defensive: if the sentence splitter changes, the e2e fixture must too.
	overallSentences := distill.SplitSentences(fullPortraits[0].Markdown)
	if len(overallSentences) != 4 {
		t.Errorf("overall portrait split into %d sentences, want 4", len(overallSentences))
	}

	t.Logf("wrote 4 e2e fixtures to %s", *e2eOut)
}
