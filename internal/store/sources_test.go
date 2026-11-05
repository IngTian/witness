package store

import (
	"context"
	"testing"
	"time"
)

func TestPortraitSourcesRoundTrip(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	src := PortraitSources{
		Sentences: []SourcedSentence{
			{Text: "Zeying loves distributed systems.", Links: []TraitLink{
				{Lens: "default", Dimension: "technical", Key: "distrib", Strength: LinkDirect},
			}},
			{Text: "He has quant experience.", Links: []TraitLink{
				{Lens: "default", Dimension: "experience", Key: "quant-trading", Strength: LinkPartial},
			}},
		},
		Keywords: []Keyword{
			{Word: "distributed", Sentences: []int{0}},
			{Word: "quant", Sentences: []int{1}},
		},
		Inherits: map[string]string{"work": "hash123"},
	}

	portrait := "unified"
	textHash := PortraitHash("# Zeying\nSome profile text")

	// Write
	if err := s.WritePortraitSources(portrait, textHash, src, now); err != nil {
		t.Fatalf("WritePortraitSources: %v", err)
	}

	// Read back
	got, gotHash, ok, err := s.ReadPortraitSources(portrait)
	if err != nil {
		t.Fatalf("ReadPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("ReadPortraitSources: expected ok=true")
	}
	if gotHash != textHash {
		t.Errorf("text_hash = %q, want %q", gotHash, textHash)
	}
	if len(got.Sentences) != 2 {
		t.Fatalf("len(Sentences) = %d, want 2", len(got.Sentences))
	}
	if got.Sentences[0].Text != src.Sentences[0].Text {
		t.Errorf("Sentences[0].Text = %q, want %q", got.Sentences[0].Text, src.Sentences[0].Text)
	}
	if len(got.Sentences[0].Links) != 1 {
		t.Fatalf("len(Sentences[0].Links) = %d, want 1", len(got.Sentences[0].Links))
	}
	if got.Sentences[0].Links[0].Strength != LinkDirect {
		t.Errorf("Sentences[0].Links[0].Strength = %q, want %q", got.Sentences[0].Links[0].Strength, LinkDirect)
	}
	if len(got.Keywords) != 2 {
		t.Fatalf("len(Keywords) = %d, want 2", len(got.Keywords))
	}
	if got.Keywords[0].Word != "distributed" {
		t.Errorf("Keywords[0].Word = %q, want %q", got.Keywords[0].Word, "distributed")
	}
	if len(got.Keywords[0].Sentences) != 1 || got.Keywords[0].Sentences[0] != 0 {
		t.Errorf("Keywords[0].Sentences = %v, want [0]", got.Keywords[0].Sentences)
	}
	if got.Inherits["work"] != "hash123" {
		t.Errorf("Inherits[work] = %q, want hash123", got.Inherits["work"])
	}

	// Second write replaces
	src2 := PortraitSources{
		Sentences: []SourcedSentence{
			{Text: "Updated sentence.", Links: []TraitLink{}},
		},
		Keywords: []Keyword{},
		Inherits: map[string]string{},
	}
	textHash2 := PortraitHash("Different text")
	if err := s.WritePortraitSources(portrait, textHash2, src2, now.Add(time.Hour)); err != nil {
		t.Fatalf("second WritePortraitSources: %v", err)
	}
	got2, gotHash2, ok2, err2 := s.ReadPortraitSources(portrait)
	if err2 != nil || !ok2 {
		t.Fatalf("ReadPortraitSources after replace: err=%v ok=%v", err2, ok2)
	}
	if gotHash2 != textHash2 {
		t.Errorf("after replace: text_hash = %q, want %q", gotHash2, textHash2)
	}
	if len(got2.Sentences) != 1 || got2.Sentences[0].Text != "Updated sentence." {
		t.Errorf("after replace: got %+v, want one sentence", got2)
	}

	// ListPortraitSources returns the names
	list, err := s.ListPortraitSources()
	if err != nil {
		t.Fatalf("ListPortraitSources: %v", err)
	}
	if len(list) != 1 || list[0] != portrait {
		t.Errorf("ListPortraitSources = %v, want [%s]", list, portrait)
	}

	// Delete removes one
	if err := s.DeletePortraitSources(portrait); err != nil {
		t.Fatalf("DeletePortraitSources: %v", err)
	}
	_, _, ok3, err3 := s.ReadPortraitSources(portrait)
	if err3 != nil {
		t.Fatalf("ReadPortraitSources after delete: %v", err3)
	}
	if ok3 {
		t.Error("ReadPortraitSources after delete: expected ok=false")
	}
	list2, _ := s.ListPortraitSources()
	if len(list2) != 0 {
		t.Errorf("ListPortraitSources after delete = %v, want []", list2)
	}

	// Read nonexistent returns ok=false
	_, _, ok4, err4 := s.ReadPortraitSources("nonexistent")
	if err4 != nil {
		t.Fatalf("ReadPortraitSources(nonexistent): %v", err4)
	}
	if ok4 {
		t.Error("ReadPortraitSources(nonexistent): expected ok=false")
	}
}

func TestMigrateV13CreatesPortraitSources(t *testing.T) {
	s := tempStore(t) // fully migrated to v14
	// Simulate a v13 DB by dropping the table and rolling back the version
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS portrait_sources`,
		`PRAGMA user_version=13`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed v13 schema (%q): %v", stmt, err)
		}
	}

	// Open (via migrate) brings it to v14
	if err := migrate(s.db); err != nil {
		t.Fatalf("migrate v13->v14: %v", err)
	}

	// Verify user_version=14
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 14 {
		t.Errorf("user_version = %d, want 14", v)
	}

	// Verify the table exists
	var hasTable int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='portrait_sources'`).Scan(&hasTable); err != nil {
		t.Fatal(err)
	}
	if hasTable != 1 {
		t.Errorf("portrait_sources table exists = %d, want 1", hasTable)
	}

	// A second open is a no-op
	if err := migrate(s.db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var v2 int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if v2 != 14 {
		t.Errorf("after second migrate: user_version = %d, want 14", v2)
	}
}

func TestMergeDoesNotCopyPortraitSources(t *testing.T) {
	base := newArch(t)
	inc := newArch(t)

	// Base has one portrait_sources row
	baseSrc := PortraitSources{
		Sentences: []SourcedSentence{{Text: "Base sentence.", Links: []TraitLink{}}},
		Keywords:  []Keyword{},
		Inherits:  map[string]string{},
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if err := base.st.WritePortraitSources("base-portrait", "base-hash", baseSrc, now); err != nil {
		t.Fatal(err)
	}

	// Incoming has one portrait_sources row
	incSrc := PortraitSources{
		Sentences: []SourcedSentence{{Text: "Incoming sentence.", Links: []TraitLink{}}},
		Keywords:  []Keyword{},
		Inherits:  map[string]string{},
	}
	if err := inc.st.WritePortraitSources("inc-portrait", "inc-hash", incSrc, now); err != nil {
		t.Fatal(err)
	}

	// Merge the incoming archive into base
	path := inc.export()
	if _, err := base.st.Merge(context.Background(), path, true, now); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Base still has only its own portrait_sources row
	list, err := base.st.ListPortraitSources()
	if err != nil {
		t.Fatalf("ListPortraitSources: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("after merge: ListPortraitSources = %v, want exactly 1 row", list)
	}
	if list[0] != "base-portrait" {
		t.Errorf("after merge: ListPortraitSources = %v, want [base-portrait]", list)
	}

	// The incoming portrait was not copied
	_, _, ok, err := base.st.ReadPortraitSources("inc-portrait")
	if err != nil {
		t.Fatalf("ReadPortraitSources(inc-portrait): %v", err)
	}
	if ok {
		t.Error("inc-portrait should not exist in base after merge")
	}
}

func TestCurrentPortraitSourcesRejectsStaleHash(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	portrait := "default"
	originalText := "# Original portrait text\n"
	originalHash := PortraitHash(originalText)

	// Write profile and sources
	if err := s.WriteProfile(portrait, originalText); err != nil {
		t.Fatal(err)
	}
	src := PortraitSources{
		Sentences: []SourcedSentence{{Text: "Zeying loves Go.", Links: []TraitLink{}}},
		Keywords:  []Keyword{},
		Inherits:  map[string]string{},
	}
	if err := s.WritePortraitSources(portrait, originalHash, src, now); err != nil {
		t.Fatal(err)
	}

	// Before change: sources are current
	_, ok, err := s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources before change: %v", err)
	}
	if !ok {
		t.Fatal("CurrentPortraitSources before change: expected ok=true")
	}

	// Change the portrait text
	newText := "# Different portrait text\n"
	if err := s.WriteProfile(portrait, newText); err != nil {
		t.Fatal(err)
	}

	// After change: sources are stale, ok=false
	_, ok, err = s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources after change: %v", err)
	}
	if ok {
		t.Error("CurrentPortraitSources after text change: expected ok=false for stale hash")
	}
}

func TestCurrentPortraitSourcesFollowsMerges(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	portrait := "default"
	text := "# Portrait text\n"
	textHash := PortraitHash(text)

	// Write profile
	if err := s.WriteProfile(portrait, text); err != nil {
		t.Fatal(err)
	}

	// Write facets: one archived with merged_into, one archived without, one active
	facets := []Facet{
		{
			Lens:       "default",
			Dimension:  "skills",
			Key:        "old-key",
			LastSeen:   "2026-11-01T00:00:00Z",
			ArchivedAt: "2026-11-02T00:00:00Z",
			MergedInto: "skills/survivor",
			Versions: []FacetVersion{
				{Value: "Old skill", Claim: "Had old skill.", Confidence: 0.8, ValidFrom: "2026-11-01T00:00:00Z"},
			},
		},
		{
			Lens:       "default",
			Dimension:  "habits",
			Key:        "retired-key",
			LastSeen:   "2026-11-01T00:00:00Z",
			ArchivedAt: "2026-11-02T00:00:00Z",
			MergedInto: "", // retired, not merged
			Versions: []FacetVersion{
				{Value: "Retired habit", Claim: "Had habit.", Confidence: 0.7, ValidFrom: "2026-11-01T00:00:00Z"},
			},
		},
		{
			Lens:      "default",
			Dimension: "skills",
			Key:       "survivor",
			LastSeen:  "2026-11-03T00:00:00Z",
			Versions: []FacetVersion{
				{Value: "Merged skill", Claim: "Has skill.", Confidence: 0.9, ValidFrom: "2026-11-02T00:00:00Z"},
			},
		},
	}
	if err := s.WriteFacets(facets); err != nil {
		t.Fatal(err)
	}

	// Write portrait sources with links to archived keys
	src := PortraitSources{
		Sentences: []SourcedSentence{
			{
				Text: "Sentence one.",
				Links: []TraitLink{
					{Lens: "default", Dimension: "skills", Key: "old-key", Strength: LinkDirect},
					{Lens: "default", Dimension: "habits", Key: "retired-key", Strength: LinkPartial},
				},
			},
		},
		Keywords: []Keyword{},
		Inherits: map[string]string{},
	}
	if err := s.WritePortraitSources(portrait, textHash, src, now); err != nil {
		t.Fatal(err)
	}

	// Read current sources
	got, ok, err := s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("CurrentPortraitSources: expected ok=true")
	}

	// Verify the merged link was rewritten, retired link dropped
	if len(got.Sentences) != 1 {
		t.Fatalf("len(Sentences) = %d, want 1", len(got.Sentences))
	}
	links := got.Sentences[0].Links
	if len(links) != 1 {
		t.Fatalf("len(Links) = %d, want 1 (merged link only)", len(links))
	}
	if links[0].Key != "survivor" {
		t.Errorf("Links[0].Key = %q, want %q (followed merge)", links[0].Key, "survivor")
	}
	if links[0].Dimension != "skills" {
		t.Errorf("Links[0].Dimension = %q, want skills", links[0].Dimension)
	}
	if links[0].Strength != LinkDirect {
		t.Errorf("Links[0].Strength = %q, want %q", links[0].Strength, LinkDirect)
	}
}

func TestCurrentPortraitSourcesMissingPortrait(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	portrait := "default"
	text := "# Portrait text\n"
	textHash := PortraitHash(text)

	// Write profile
	if err := s.WriteProfile(portrait, text); err != nil {
		t.Fatal(err)
	}

	// Write sources
	src := PortraitSources{
		Sentences: []SourcedSentence{{Text: "Sentence.", Links: []TraitLink{}}},
		Keywords:  []Keyword{},
		Inherits:  map[string]string{},
	}
	if err := s.WritePortraitSources(portrait, textHash, src, now); err != nil {
		t.Fatal(err)
	}

	// Delete the profile file
	if err := s.DeleteProfile(portrait); err != nil {
		t.Fatal(err)
	}

	// CurrentPortraitSources returns ok=false
	_, ok, err := s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources after profile delete: %v", err)
	}
	if ok {
		t.Error("CurrentPortraitSources with missing profile: expected ok=false")
	}
}

func TestCurrentPortraitSourcesFollowsMergeChain(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	portrait := "default"
	text := "# Portrait\n"
	textHash := PortraitHash(text)

	if err := s.WriteProfile(portrait, text); err != nil {
		t.Fatal(err)
	}

	// Chain: A -> B -> C (C is active)
	facets := []Facet{
		{
			Lens:       "default",
			Dimension:  "skills",
			Key:        "A",
			LastSeen:   "2026-11-01T00:00:00Z",
			ArchivedAt: "2026-11-02T00:00:00Z",
			MergedInto: "skills/B",
			Versions:   []FacetVersion{{Value: "A", Claim: "A", Confidence: 0.8, ValidFrom: "2026-11-01T00:00:00Z"}},
		},
		{
			Lens:       "default",
			Dimension:  "skills",
			Key:        "B",
			LastSeen:   "2026-11-02T00:00:00Z",
			ArchivedAt: "2026-11-03T00:00:00Z",
			MergedInto: "skills/C",
			Versions:   []FacetVersion{{Value: "B", Claim: "B", Confidence: 0.8, ValidFrom: "2026-11-02T00:00:00Z"}},
		},
		{
			Lens:      "default",
			Dimension: "skills",
			Key:       "C",
			LastSeen:  "2026-11-03T00:00:00Z",
			Versions:  []FacetVersion{{Value: "C", Claim: "C", Confidence: 0.9, ValidFrom: "2026-11-03T00:00:00Z"}},
		},
	}
	if err := s.WriteFacets(facets); err != nil {
		t.Fatal(err)
	}

	// Link to A
	src := PortraitSources{
		Sentences: []SourcedSentence{
			{Text: "Sentence.", Links: []TraitLink{
				{Lens: "default", Dimension: "skills", Key: "A", Strength: LinkDirect},
			}},
		},
		Keywords: []Keyword{},
		Inherits: map[string]string{},
	}
	if err := s.WritePortraitSources(portrait, textHash, src, now); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}

	// Should follow chain to C
	links := got.Sentences[0].Links
	if len(links) != 1 {
		t.Fatalf("len(Links) = %d, want 1", len(links))
	}
	if links[0].Key != "C" {
		t.Errorf("Key = %q, want C (followed A->B->C)", links[0].Key)
	}
}

func TestCurrentPortraitSourcesRejectsEmptyRow(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	portrait := "unified"
	text := "# Unified portrait\n"
	textHash := PortraitHash(text)

	// Write profile
	if err := s.WriteProfile(portrait, text); err != nil {
		t.Fatal(err)
	}

	// Write empty sources row (zero sentences) - this happens when unified is written
	// but no notebooks have sources yet.
	src := PortraitSources{
		Sentences: []SourcedSentence{},
		Keywords:  []Keyword{},
		Inherits:  map[string]string{},
	}
	if err := s.WritePortraitSources(portrait, textHash, src, now); err != nil {
		t.Fatal(err)
	}

	// CurrentPortraitSources should return ok=false for empty row.
	_, ok, err := s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources: %v", err)
	}
	if ok {
		t.Error("CurrentPortraitSources with zero sentences: expected ok=false, got true")
	}
}

func TestDedupeLinksStableOrder(t *testing.T) {
	// Two partial links should produce a deterministic order.
	links := []TraitLink{
		{Lens: "default", Dimension: "skills", Key: "go", Strength: LinkPartial},
		{Lens: "default", Dimension: "skills", Key: "python", Strength: LinkPartial},
		{Lens: "default", Dimension: "habits", Key: "reviews", Strength: LinkPartial},
	}
	got := DedupeLinks(links)
	// All partial, so order should be preserved.
	if len(got) != 3 {
		t.Fatalf("DedupeLinks returned %d links, want 3", len(got))
	}
	if got[0].Key != "go" {
		t.Errorf("got[0].Key = %q, want go", got[0].Key)
	}
	if got[1].Key != "python" {
		t.Errorf("got[1].Key = %q, want python", got[1].Key)
	}
	if got[2].Key != "reviews" {
		t.Errorf("got[2].Key = %q, want reviews", got[2].Key)
	}

	// Run multiple times to verify determinism.
	for i := 0; i < 10; i++ {
		got := DedupeLinks(links)
		if len(got) != 3 || got[0].Key != "go" || got[1].Key != "python" || got[2].Key != "reviews" {
			t.Errorf("run %d: DedupeLinks order changed", i)
		}
	}
}

func TestDedupeLinksDirectFirst(t *testing.T) {
	// Mix of direct and partial.
	links := []TraitLink{
		{Lens: "default", Dimension: "skills", Key: "go", Strength: LinkPartial},
		{Lens: "default", Dimension: "skills", Key: "python", Strength: LinkDirect},
		{Lens: "default", Dimension: "habits", Key: "reviews", Strength: LinkPartial},
	}
	got := DedupeLinks(links)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Direct should come first.
	if got[0].Strength != LinkDirect {
		t.Errorf("got[0].Strength = %q, want direct", got[0].Strength)
	}
	if got[0].Key != "python" {
		t.Errorf("got[0].Key = %q, want python", got[0].Key)
	}
	// Partials follow in order.
	if got[1].Key != "go" || got[2].Key != "reviews" {
		t.Errorf("partial link order wrong: %q, %q", got[1].Key, got[2].Key)
	}
}

func TestDedupeLinksKeepsStronger(t *testing.T) {
	// Duplicate key with different strengths.
	links := []TraitLink{
		{Lens: "default", Dimension: "skills", Key: "go", Strength: LinkPartial},
		{Lens: "default", Dimension: "skills", Key: "go", Strength: LinkDirect},
	}
	got := DedupeLinks(links)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (deduped)", len(got))
	}
	if got[0].Strength != LinkDirect {
		t.Errorf("Strength = %q, want direct (kept stronger)", got[0].Strength)
	}
}

func TestCurrentPortraitSourcesDedupeAfterRemap(t *testing.T) {
	s := tempStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	portrait := "default"
	text := "# Portrait\n"
	textHash := PortraitHash(text)

	if err := s.WriteProfile(portrait, text); err != nil {
		t.Fatal(err)
	}

	// Two archived keys both merge to the same survivor
	facets := []Facet{
		{
			Lens:       "default",
			Dimension:  "skills",
			Key:        "old1",
			ArchivedAt: "2026-11-02T00:00:00Z",
			MergedInto: "skills/survivor",
			LastSeen:   "2026-11-01T00:00:00Z",
			Versions:   []FacetVersion{{Value: "Old1", Claim: "1", Confidence: 0.7, ValidFrom: "2026-11-01T00:00:00Z"}},
		},
		{
			Lens:       "default",
			Dimension:  "skills",
			Key:        "old2",
			ArchivedAt: "2026-11-02T00:00:00Z",
			MergedInto: "skills/survivor",
			LastSeen:   "2026-11-01T00:00:00Z",
			Versions:   []FacetVersion{{Value: "Old2", Claim: "2", Confidence: 0.6, ValidFrom: "2026-11-01T00:00:00Z"}},
		},
		{
			Lens:      "default",
			Dimension: "skills",
			Key:       "survivor",
			LastSeen:  "2026-11-03T00:00:00Z",
			Versions:  []FacetVersion{{Value: "Survivor", Claim: "S", Confidence: 0.9, ValidFrom: "2026-11-02T00:00:00Z"}},
		},
	}
	if err := s.WriteFacets(facets); err != nil {
		t.Fatal(err)
	}

	// Links to both old1 (direct) and old2 (partial)
	src := PortraitSources{
		Sentences: []SourcedSentence{
			{Text: "Sentence.", Links: []TraitLink{
				{Lens: "default", Dimension: "skills", Key: "old1", Strength: LinkDirect},
				{Lens: "default", Dimension: "skills", Key: "old2", Strength: LinkPartial},
			}},
		},
		Keywords: []Keyword{},
		Inherits: map[string]string{},
	}
	if err := s.WritePortraitSources(portrait, textHash, src, now); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.CurrentPortraitSources(portrait)
	if err != nil {
		t.Fatalf("CurrentPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}

	// Should dedupe to one link to survivor, keeping the stronger strength (direct)
	links := got.Sentences[0].Links
	if len(links) != 1 {
		t.Fatalf("len(Links) = %d, want 1 (deduped)", len(links))
	}
	if links[0].Key != "survivor" {
		t.Errorf("Key = %q, want survivor", links[0].Key)
	}
	if links[0].Strength != LinkDirect {
		t.Errorf("Strength = %q, want %q (kept stronger)", links[0].Strength, LinkDirect)
	}
}
