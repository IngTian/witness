package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// Each tool reads and writes the lens it is given: a lens has one name, so the one an agent
// passes is the one its rows are tagged with.
func TestToolsReadAndWriteTheLensTheyAreGiven(t *testing.T) {
	fake := &fakeMCPStore{
		obs: []store.Observation{{ID: "o1", Lens: "regime", Dimension: "regimes",
			Observation: "turns when rates move", Embedding: []float32{0.1, 0.2, 0.3}}},
		facets: []store.Facet{{Lens: "regime", Dimension: "regimes", Key: "flips_on_rates",
			Versions: []store.FacetVersion{{Value: "turns when rates move", Confidence: 0.7}}}},
		profiles:     map[string]string{"regime": "# the regime narrative"},
		stagedReturn: true,
		whyEmpty:     store.NoFacets,
	}
	arg := map[string]any{"lens": "regime"}

	if got := callText(t, fake, testOptions, "search_observations", map[string]any{"query": "rates", "lens": "regime"}); !strings.Contains(got, "turns when rates move") {
		t.Errorf("search must find the lens's observations:\n%s", got)
	}
	// F140: one lens's rows, not every lens's embeddings filtered in Go.
	if fake.readLens != "regime" {
		t.Errorf("search must read only the lens it searches, read %q", fake.readLens)
	}
	if got := callText(t, fake, testOptions, "get_facets", arg); !strings.Contains(got, "flips_on_rates") {
		t.Errorf("get_facets must list the lens's facets:\n%s", got)
	}
	if got := callText(t, fake, testOptions, "get_profile", arg); !strings.Contains(got, "the regime narrative") {
		t.Errorf("get_profile must serve the lens's narrative:\n%s", got)
	}
	callText(t, fake, testOptions, "record_observation", map[string]any{"session": "s1", "lens": "regime",
		"dimension": "regimes", "observation": "noticed"})
	if fake.lastStaged.Lens != "regime" || fake.lastStaged.ID != store.ObservationID("s1", "regime", "noticed") {
		t.Errorf("record must stage under the name the review reads, got lens %q id %q", fake.lastStaged.Lens, fake.lastStaged.ID)
	}
}

// The empty hint must ask WhyEmpty about the lens it was given: asked about another name, a lens
// with observations but no facets (or no embeddings) is told nothing was mined and to re-mine it.
func TestEmptyHintAsksAboutTheLensItWasGiven(t *testing.T) {
	arg := map[string]any{"lens": "regime"}
	for tool, args := range map[string]map[string]any{
		"search_observations": {"query": "rates", "lens": "regime"},
		"get_facets":          arg,
		"get_profile":         arg,
	} {
		fake := &fakeMCPStore{whyEmpty: store.NoFacets}
		callText(t, fake, testOptions, tool, args)
		if fake.whyLens != "regime" {
			t.Errorf("%s asked why %q is empty, want \"regime\"", tool, fake.whyLens)
		}
	}
}

// The unified portrait is not a lens: nothing is mined into it, so `witness lens backfill unified`
// (a reserved name) is a command that can only fail.
func TestEmptyUnifiedProfileDoesNotSayBackfill(t *testing.T) {
	fake := &fakeMCPStore{whyEmpty: store.NoObservations}
	got := callText(t, fake, testOptions, "get_profile", map[string]any{})
	if strings.Contains(got, "backfill") || !strings.Contains(got, "enabled lenses' facets") {
		t.Errorf("an empty unified portrait must point at the lenses' facets, not a backfill:\n%s", got)
	}
}

// Lenses were never repo-scoped (AGENTS.md: lenses are global), and default has had no always-on
// privilege since #44 slice 1a. An agent reads these strings to decide what to call.
func TestToolTextMakesNoFalseLensClaims(t *testing.T) {
	res, err := connect(t, &fakeMCPStore{}, testOptions).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res.Tools) // descriptions AND every argument's schema text
	for _, claim := range []string{"repo", "always-on"} {
		if strings.Contains(string(raw), claim) {
			t.Errorf("tool text still says %q:\n%s", claim, raw)
		}
	}
}
