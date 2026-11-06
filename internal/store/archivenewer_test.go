package store

import (
	"errors"
	"fmt"
	"testing"
)

// An older binary opening a NEWER archive must keep capturing but refuse to rewrite L2.
//
// Reproduced before the guard: a v0.8.0 tree folded once over a v15 archive and writeFacetsTx's
// DELETE + INSERT (with that release's column list) un-archived the archived facet and blanked
// claim and confidence_seq, with no error. migrate() returns nil when user_version >=
// schemaVersion, so nothing stopped it.
func TestNewerArchiveRefusesFacetRewrite(t *testing.T) {
	s := tempStore(t)
	seed := []Facet{
		{Lens: "default", Dimension: "d", Key: "live", Versions: []FacetVersion{{Value: "v", Claim: "c", ConfidenceSeq: 3}}},
		{Lens: "default", Dimension: "d", Key: "gone", ArchivedAt: "2026-11-01T00:00:00Z", Versions: []FacetVersion{{Value: "v"}}},
	}
	if err := s.WriteFacets(seed); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveNewer(); err != nil {
		t.Fatalf("an archive at this binary's own schema was flagged newer: %v", err)
	}

	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveNewer(); !errors.Is(err, ErrArchiveNewer) {
		t.Fatalf("ArchiveNewer on a v%d archive = %v, want ErrArchiveNewer", schemaVersion+1, err)
	}
	if a, b := s.SchemaVersions(); a != schemaVersion+1 || b != schemaVersion {
		t.Errorf("SchemaVersions = (%d, %d), want (%d, %d)", a, b, schemaVersion+1, schemaVersion)
	}

	// The last line of defence: the REPLACE itself refuses, and the table is untouched.
	if err := s.WriteFacets([]Facet{{Lens: "default", Dimension: "d", Key: "live", Versions: []FacetVersion{{Value: "v"}}}}); !errors.Is(err, ErrArchiveNewer) {
		t.Fatalf("WriteFacets on a newer archive = %v, want ErrArchiveNewer", err)
	}
	got, err := s.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("a refused write changed the facet table: %d facets, want 2", len(got))
	}

	// Reopening still works — capture must keep running on a newer archive — and the version
	// the newer binary wrote is not stamped back down.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open()
	if err != nil {
		t.Fatalf("Open refused a newer archive; capture would stop: %v", err)
	}
	defer s2.Close()
	if err := s2.ArchiveNewer(); !errors.Is(err, ErrArchiveNewer) {
		t.Fatalf("after reopen ArchiveNewer = %v, want ErrArchiveNewer", err)
	}
}
