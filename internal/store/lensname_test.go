package store

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// registryLens writes a lens straight into the registry, bypassing RegisterLens: an archive
// registered by an older witness.
func registryLens(t *testing.T, s *Store, name, lensJSON string) {
	t.Helper()
	dir := filepath.Join(s.LensesDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LensExtractFile), []byte("mine "+name), 0o600); err != nil {
		t.Fatal(err)
	}
	if lensJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, LensConfigFile), []byte(lensJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// reopenAsUpgrade clears the one-shot marker and reopens, so the next Open is the first
// one after an upgrade.
func reopenAsUpgrade(t *testing.T, s *Store) *Store {
	t.Helper()
	if err := s.SetMetaString(lensNamesAdoptedKey, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	return s2
}

// The real case: corpus-example registered from a pre-F153 checkout mined as "regime". It
// becomes "regime", and the data it mined under that name is its own again.
func TestOpenRenamesALensToItsDataName(t *testing.T) {
	s := tempStore(t)
	registryLens(t, s, "corpus-example", `{"name":"regime","dimensions":["rates"]}`)
	if err := s.EnableLens("corpus-example"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendObservations([]Observation{{ID: "o1", Session: "s1", Lens: "regime", Dimension: "rates", Observation: "o"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFacets([]Facet{{Lens: "regime", Dimension: "rates", Key: "k", Versions: []FacetVersion{{Value: "v", Confidence: 0.5}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteProfile("regime", "# regime"); err != nil {
		t.Fatal(err)
	}
	s = reopenAsUpgrade(t, s)

	if got := s.RegisteredLenses(); !slices.Equal(got, []string{"regime"}) {
		t.Fatalf("registered = %v, want [regime]", got)
	}
	if _, err := os.Stat(filepath.Join(s.LensesDir(), "corpus-example")); !os.IsNotExist(err) {
		t.Fatalf("the old folder must be gone, stat err = %v", err)
	}
	if got := s.LoadConfig().EnabledLenses; !slices.Equal(got, []string{"regime"}) {
		t.Fatalf("enabled = %v, want [regime]", got)
	}
	if obs, facets := s.LensDataCounts("regime"); obs != 1 || facets != 1 {
		t.Fatalf("regime data = %d obs, %d facets; want 1, 1", obs, facets)
	}
	if md, ok, _ := s.ReadProfile("regime"); !ok || !strings.Contains(md, "# regime") {
		t.Fatalf("regime profile = %q, %v", md, ok)
	}
	if got := s.IgnoredLensNames(); len(got) != 0 {
		t.Fatalf("a renamed lens has nothing left to ignore, got %v", got)
	}
	if s.MetaString(lensNamesAdoptedKey) == "" {
		t.Fatal("the marker must be stamped")
	}
	logHas(t, s, `"level":"INFO"`, `"from":"corpus-example","to":"regime"`)
}

// logHas fails unless witness.log holds every want: the rename pass's only record.
func logHas(t *testing.T, s *Store, want ...string) {
	t.Helper()
	b, _ := os.ReadFile(s.LogPath())
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Errorf("witness.log lacks %s:\n%s", w, b)
		}
	}
}

// A data name another lens already holds (exactly, or only by case on a case-insensitive
// filesystem) renames nothing: both lenses keep their folders and config, and doctor reports it.
func TestOpenLeavesACollidingLensAlone(t *testing.T) {
	s := tempStore(t)
	registryLens(t, s, "regime", "")
	registryLens(t, s, "corpus-example", `{"name":"regime"}`)
	registryLens(t, s, "Rates", "")
	registryLens(t, s, "probe", `{"name":"rates"}`)
	if err := s.EnableLens("corpus-example"); err != nil {
		t.Fatal(err)
	}
	s = reopenAsUpgrade(t, s)

	if got := s.RegisteredLenses(); !slices.Equal(got, []string{"Rates", "corpus-example", "probe", "regime"}) {
		t.Fatalf("registered = %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(s.LensesDir(), "regime", LensExtractFile)); string(b) != "mine regime" {
		t.Fatalf("regime's extract.md = %q, want it untouched", b)
	}
	if got := s.LoadConfig().EnabledLenses; !slices.Equal(got, []string{"corpus-example"}) {
		t.Fatalf("enabled = %v, want [corpus-example]", got)
	}
	if got, want := s.IgnoredLensNames(), map[string]string{"corpus-example": "regime", "probe": "rates"}; !maps.Equal(got, want) {
		t.Fatalf("ignored = %v, want %v", got, want)
	}
	if s.MetaString(lensNamesAdoptedKey) == "" {
		t.Fatal("a collision is final: the marker must be stamped")
	}
	logHas(t, s, `"level":"WARN"`,
		`"lens":"corpus-example","name":"regime","reason":"lens regime already exists","retried":false`,
		`"lens":"probe","name":"rates","reason":"lens Rates already exists","retried":false`)
}

func TestOpenRenameIsANoOpWhenNamesMatch(t *testing.T) {
	s := tempStore(t)
	mathJSON := `{"name":"math","dimensions":["speed"]}`
	registryLens(t, s, "math", mathJSON)
	registryLens(t, s, "plain", "")
	registryLens(t, s, "blank", `{"name":"  "}`)
	registryLens(t, s, "broken", `{not json`)
	if err := s.EnableLens("math"); err != nil {
		t.Fatal(err)
	}
	s = reopenAsUpgrade(t, s)

	if got := s.RegisteredLenses(); !slices.Equal(got, []string{"blank", "broken", "math", "plain"}) {
		t.Fatalf("registered = %v", got)
	}
	if got := s.LoadConfig().EnabledLenses; !slices.Equal(got, []string{"math"}) {
		t.Fatalf("enabled = %v, want [math]", got)
	}
	if got := s.IgnoredLensNames(); len(got) != 0 {
		t.Fatalf("ignored = %v, want none", got)
	}
	if b, _ := os.ReadFile(filepath.Join(s.LensesDir(), "math", LensConfigFile)); string(b) != mathJSON {
		t.Fatalf("math's lens.json = %q, want it unchanged", b)
	}
	if s.MetaString(lensNamesAdoptedKey) == "" {
		t.Fatal("the marker must be stamped")
	}
}

// A data name registration would refuse (the reserved "unified", case-folded, or a non-slug)
// is never a rename target.
func TestOpenNeverRenamesToAReservedName(t *testing.T) {
	s := tempStore(t)
	registryLens(t, s, "probe", `{"name":"Unified"}`)
	registryLens(t, s, "spaced", `{"name":"my lens"}`)
	s = reopenAsUpgrade(t, s)

	if got := s.RegisteredLenses(); !slices.Equal(got, []string{"probe", "spaced"}) {
		t.Fatalf("registered = %v", got)
	}
	for _, n := range []string{"unified", "Unified"} {
		if _, err := os.Stat(filepath.Join(s.LensesDir(), n)); !os.IsNotExist(err) {
			t.Fatalf("lenses/%s must not exist, stat err = %v", n, err)
		}
	}
	if got, want := s.IgnoredLensNames(), map[string]string{"probe": "Unified", "spaced": "my lens"}; !maps.Equal(got, want) {
		t.Fatalf("ignored = %v, want %v", got, want)
	}
}

// The rename runs once per archive: a lens registered after the marker keeps its name, whatever
// its lens.json says.
func TestOpenRenamesOnlyOnce(t *testing.T) {
	s := tempStore(t)
	registryLens(t, s, "late", `{"name":"other"}`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if got := s2.RegisteredLenses(); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("registered = %v, want [late]", got)
	}
	if got := s2.IgnoredLensNames()["late"]; got != "other" {
		t.Fatalf("ignored[late] = %q, want other", got)
	}
}

// A read that fails at the one Open after the upgrade is not an answer: the marker stays
// unstamped, so the next Open, with the file readable again, still renames the lens. Stamped, the
// skip would be permanent and the lens would re-mine its history under the wrong name.
func TestOpenRetriesTheRenameAfterAFailedRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a permission bit that makes a file unreadable (not Windows, not root)")
	}
	for _, tc := range []struct {
		name       string
		path       func(s *Store) string
		mode, back os.FileMode
	}{
		{"unreadable lens.json", func(s *Store) string { return filepath.Join(s.LensesDir(), "corpus-example", LensConfigFile) }, 0, 0o600},
		{"unlistable registry", func(s *Store) string { return s.LensesDir() }, 0o300, 0o700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tempStore(t)
			registryLens(t, s, "corpus-example", `{"name":"regime"}`)
			if err := s.EnableLens("corpus-example"); err != nil {
				t.Fatal(err)
			}
			path := tc.path(s)
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, tc.back) })
			s = reopenAsUpgrade(t, s)
			if s.LensNamesAdopted() {
				t.Fatal("a failed read must leave the marker unstamped")
			}
			if tc.mode == 0 {
				logHas(t, s, `"lens":"corpus-example"`, "permission denied", `"retried":true`)
			}
			if err := os.Chmod(path, tc.back); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2, err := Open()
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s2.Close() })
			if got := s2.RegisteredLenses(); !slices.Equal(got, []string{"regime"}) {
				t.Fatalf("registered = %v, want [regime] once the file reads again", got)
			}
			if got := s2.LoadConfig().EnabledLenses; !slices.Equal(got, []string{"regime"}) {
				t.Fatalf("enabled = %v, want [regime]", got)
			}
			if !s2.LensNamesAdopted() {
				t.Fatal("the marker must be stamped once the rename is done")
			}
		})
	}
}
