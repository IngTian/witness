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
