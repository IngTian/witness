package commands

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// `witness facets` is a claim about the person. Compaction archives rather than deletes, so
// the listing has to distinguish "what witness currently says" from "what it used to say" —
// otherwise a consolidated facet reads as live and the compaction is invisible.

func archivedFixture(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	mk := func(dim, key, value string) store.Facet {
		return store.Facet{
			Lens: store.LensDefault, Dimension: dim, Key: key, LastSeen: "2026-10-01T00:00:00Z",
			Versions: []store.FacetVersion{{
				Value: value, ValidFrom: "2026-10-01T00:00:00Z",
				RecordedAt: "2026-10-01T00:00:00Z", Confidence: 0.7,
			}},
		}
	}
	absorbed := mk("thinking", "absorbed_one", "the old narrow wording")
	absorbed.ArchivedAt = "2026-10-02T00:00:00Z"
	absorbed.MergedInto = "thinking/survivor"
	retired := mk("habits", "retired_one", "a one-off incident")
	retired.ArchivedAt = "2026-10-02T00:00:00Z"

	if err := st.WriteFacets([]store.Facet{
		mk("thinking", "survivor", "the consolidated wording"), absorbed, retired,
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFacetsListingHidesArchivedByDefault(t *testing.T) {
	archivedFixture(t)
	out := captureStdout(t, func() {
		if err := cmdFacets(nil, false, false); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "absorbed_one") || strings.Contains(out, "retired_one") {
		t.Errorf("archived facets must not be listed as live claims:\n%s", out)
	}
	if !strings.Contains(out, "survivor") {
		t.Errorf("the active facet should be listed:\n%s", out)
	}
	// The count in the header is part of the claim too.
	if !strings.Contains(out, "- 1\n") {
		t.Errorf("the header count should be 1, not 3:\n%s", out)
	}
}

// --all is for auditing what compaction did, so each archived facet has to say WHERE it went;
// a bare list would not distinguish "merged into X" from "retired".
func TestFacetsAllMarksWhereEachArchivedFacetWent(t *testing.T) {
	archivedFixture(t)
	out := captureStdout(t, func() {
		if err := cmdFacets(nil, false, true); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"absorbed_one", "retired_one", "survivor",
		"merged into thinking/survivor", "retired"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all output should contain %q:\n%s", want, out)
		}
	}
}

func TestExpandUnfoldsTheMergeTrajectory(t *testing.T) {
	archivedFixture(t)
	out := captureStdout(t, func() {
		if err := cmdFacetTrajectory(nil, "thinking/survivor", false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "absorbed_one") {
		t.Errorf("expansion must show what was merged in:\n%s", out)
	}
	if !strings.Contains(out, "the old narrow wording") {
		t.Errorf("expansion must show the absorbed facet's ORIGINAL value — that is the point:\n%s", out)
	}
	if strings.Contains(out, "retired_one") {
		t.Errorf("an unrelated retired facet must not appear in this trajectory:\n%s", out)
	}
}

func TestExpandRejectsAMalformedRef(t *testing.T) {
	archivedFixture(t)
	if err := cmdFacetTrajectory(nil, "notaref", false); err == nil {
		t.Error("a ref without dimension/key should be rejected, not silently matched")
	}
	if err := cmdFacetTrajectory(nil, "thinking/nosuchfacet", false); err == nil {
		t.Error("an unknown facet should be reported, not printed as empty")
	}
}
