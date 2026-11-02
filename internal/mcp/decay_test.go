package mcp

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// get_facets reports CURRENT confidence. A 0.90 facet one half-life past its last confirmation
// reads 0.45; shown 0.90, an agent takes a trait nothing has evidenced in thousands of
// observations as a certainty. Decay is derived on read (#173), so a reader that does not
// derive it simply never shows any.
func TestGetFacetsReportsDecayedConfidence(t *testing.T) {
	f := store.Facet{Lens: "default", Dimension: "traits", Key: "satisfices",
		Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9, ConfidenceSeq: 100}}}
	out := renderFacets("default", []store.Facet{f}, 100+int64(store.FacetDecayHalfLifeObs), func() string { return "" })
	if !strings.Contains(out, "(confidence 0.45)") || strings.Contains(out, "(confidence 0.90)") {
		t.Errorf("get_facets must report the decayed 0.45, not the stored 0.90:\n%s", out)
	}
}
