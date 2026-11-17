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
	out := renderFacets("default", []store.Facet{f}, store.ContiguousClock(100+int64(store.FacetDecayHalfLifeObs)), func() string { return "" })
	if !strings.Contains(out, "(confidence 0.45)") || strings.Contains(out, "(confidence 0.90)") {
		t.Errorf("get_facets must report the decayed 0.45, not the stored 0.90:\n%s", out)
	}
}

// A math facet decays on math's observations, not on default's, which would read 0.45.
func TestGetFacetsDecaysOnItsOwnClock(t *testing.T) {
	fake := &fakeMCPStore{
		facets: []store.Facet{{Lens: "math", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9, ConfidenceSeq: 1}}}},
		clocks: map[string]store.LensClock{"math": store.ContiguousClock(1501), "default": store.ContiguousClock(3001)},
	}
	out := callText(t, fake, testOptions, "get_facets", map[string]any{"lens": "math"})
	if !strings.Contains(out, "(confidence 0.64)") || strings.Contains(out, "(confidence 0.45)") || strings.Contains(out, "(confidence 0.90)") {
		t.Errorf("get_facets must report math's own-clock 0.64, not default's 0.45 or the stored 0.90:\n%s", out)
	}
}
