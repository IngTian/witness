package commands

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// hintArchive is an archive with one enabled lens, registered as `registered`, that mines under
// `data` (its lens.json name), holding `active` active facets and `archived` archived ones.
func hintArchive(t *testing.T, registered, data string, active, archived int) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.RegisterLens(registered, writeLensDir(t, data, "Extract prompt.", "Review prompt.")); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens(registered); err != nil {
		t.Fatal(err)
	}
	var fs []store.Facet
	for i := 0; i < active+archived; i++ {
		f := store.Facet{Lens: data, Dimension: "traits", Key: fmt.Sprintf("k%d", i),
			Versions: []store.FacetVersion{{Value: "v", Confidence: 0.5}}}
		if i >= active {
			f.ArchivedAt = "2026-11-01T00:00:00Z"
		}
		fs = append(fs, f)
	}
	if err := st.WriteFacets(fs); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCompactionNudges(t *testing.T) {
	cases := []struct {
		name             string
		active, archived int
		baseline         int // 0 = no baseline key
		want             string
	}{
		{"never compacted, big enough", 879, 0, 0, "lens: 879 active facets, never compacted"},
		{"never compacted, small", compactHintMin - 1, 0, 0, ""},
		// Compacted before baselines existed: archived facets, no key. Saying "never compacted" would be false.
		{"compacted before the baseline existed", 1750, 870, 0, ""},
		{"grown, under 1.5x", 599, 10, 400, ""},
		{"grown to 1.5x", 600, 10, 400, "lens: 600 active facets, up from 400 after the last compaction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := hintArchive(t, "lens", "lens", tc.active, tc.archived)
			if tc.baseline > 0 {
				if err := st.SetMetaString(store.CompactBaselineKey("lens"), strconv.Itoa(tc.baseline)); err != nil {
					t.Fatal(err)
				}
			}
			got := compactionNudges(st)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no hint, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].text() != tc.want || got[0].command() != "witness compact --lens lens" {
				t.Fatalf("got %+v, want one hint %q", got, tc.want)
			}
		})
	}
}

// A lens can mine under a different name than it is registered as (corpus-example → regime): the
// counts come from the data name, and the hint names the registered one, which compact --lens takes.
func TestCompactionNudgeUsesTheRegisteredName(t *testing.T) {
	st := hintArchive(t, "corpus", "regime", compactHintMin, 0)
	got := compactionNudges(st)
	if len(got) != 1 || got[0].Lens != "corpus" || got[0].command() != "witness compact --lens corpus" {
		t.Fatalf("got %+v, want a hint for the registered name corpus", got)
	}
}

// After a compaction the baseline is the active count it left, so the hint goes quiet until the
// lens grows again; `lens backfill --fresh` (DeleteLensData) clears it with the lens's data.
func TestCompactBaselineIsStampedAndClearedWithTheLensData(t *testing.T) {
	st := hintArchive(t, "lens", "lens", 900, 0)
	stampCompactBaselines(st, activeLenses(st))
	if got := st.MetaString(store.CompactBaselineKey("lens")); got != "900" {
		t.Fatalf("baseline = %q, want 900", got)
	}
	if got := compactionNudges(st); len(got) != 0 {
		t.Fatalf("a just-compacted lens must not be nudged: %+v", got)
	}
	if _, _, err := st.DeleteLensData("lens"); err != nil {
		t.Fatal(err)
	}
	if got := st.MetaString(store.CompactBaselineKey("lens")); got != "" {
		t.Errorf("DeleteLensData left the compaction baseline behind: %q", got)
	}
}

func TestStatusShowsTheCompactionHint(t *testing.T) {
	hintArchive(t, "lens", "lens", 879, 0)
	out := captureStdout(t, func() {
		if err := cmdDistillStatus(false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "witness compact --lens lens") || !strings.Contains(out, "never compacted") {
		t.Errorf("status did not point at compaction:\n%s", out)
	}
}
