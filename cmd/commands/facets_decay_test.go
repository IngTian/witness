package commands

import (
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
		renderCurrentFacets("default", []store.Facet{f}, 100+int64(store.FacetDecayHalfLifeObs),
			true, true, func() store.EmptyReason { return store.NoCapture })
	})
	if !strings.Contains(out, "(confidence 0.45)") || strings.Contains(out, "(confidence 0.90)") {
		t.Errorf("`witness facets` must show the decayed 0.45, not the stored 0.90:\n%s", out)
	}
}
