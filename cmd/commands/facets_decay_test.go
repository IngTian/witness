package commands

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// `witness facets` shows CURRENT confidence. docs/how-it-works.md tells the user that "a
// confidence you saw last month may read lower today ... that is working correctly", and points
// them at this command to see it — but it printed the value stored at the facet's last
// confirmation, which only moves when a review cites the facet. So the documented behaviour was
// unobservable from the one place the docs said to look.
func TestFacetsListingShowsDecayedConfidence(t *testing.T) {
	f := store.Facet{Lens: "default", Dimension: "traits", Key: "satisfices",
		Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9, ConfidenceSeq: 100}}}
	out := captureStdout(t, func() {
		renderCurrentFacets("default", []store.Facet{f}, store.ContiguousClock(100+int64(store.FacetDecayHalfLifeObs)),
			true, true, func() store.EmptyReason { return store.NoCapture })
	})
	// The confidence column leads each facet row: "    0.45  satisfices  …".
	if !strings.Contains(out, "    0.45  satisfices") || strings.Contains(out, "0.90") {
		t.Errorf("`witness facets` must show the decayed 0.45, not the stored 0.90:\n%s", out)
	}
}

// A math facet decays on math's observations (a third of this archive): 0.64, where default's
// would read 0.45, the global seq 0.32, and no clock the stored 0.90.
func TestFacetsListingDecaysOnItsOwnClock(t *testing.T) {
	st := readFixture(t)
	var obs []store.Observation
	for i, l := range append([]string{"math"}, slices.Repeat([]string{"default", "default", "math"}, 1500)...) {
		obs = append(obs, store.Observation{ID: fmt.Sprintf("o%d", i+1), Lens: l, Observation: "seen"})
	}
	if err := st.AppendObservations(obs); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFacets([]store.Facet{{Lens: "math", Dimension: "traits", Key: "satisfices",
		Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9, ConfidenceSeq: 1}}}}); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "facets", "math")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "    0.64  satisfices") {
		t.Errorf("`witness facets math` must show math's own-clock 0.64:\n%s", out)
	}
	for _, wrong := range []string{"0.45", "0.32", "0.90"} {
		if strings.Contains(out, wrong) {
			t.Errorf("`witness facets math` shows %s (another lens's clock, the global one, or none):\n%s", wrong, out)
		}
	}
}
