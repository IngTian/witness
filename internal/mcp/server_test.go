package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
)

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(string) ([]float32, error) { return []float32{0.1, 0.2, 0.3}, nil }

// TestNoStructuredOutput is the regression for the silent "every tool returns {}"
// bug: the go-sdk derives an output schema from a non-`any` handler return type
// and then emits `structuredContent` on every call result. A spec-compliant
// client (Claude Code) reads structuredContent and IGNORES the text Content, so a
// typed-but-empty return (`empty{}`) made every tool look empty even though the
// payload was in Content. Handlers must return `any`/nil so no structuredContent
// is emitted and clients use Content. This drives a real client<->server round
// trip over the in-memory transport — a Content-only unit check would not catch it.
func TestNoStructuredOutput(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteFacets([]store.Facet{{
		Lens: "math", Dimension: "resilience", Key: "trip_wire",
		Versions: []store.FacetVersion{{Value: "recovers fast", Confidence: 0.9}},
	}}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := newServer(st, fakeEmbedder{}).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	// get_facets returns the structured facets in Content, with NO structuredContent.
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "get_facets",
		Arguments: map[string]any{"lens": "math"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuredContent != nil {
		t.Fatalf("tool must not emit structuredContent (clients read it and ignore text Content); got %v", res.StructuredContent)
	}
	if len(res.Content) == 0 {
		t.Fatal("expected text content with the facets")
	}
	if txt, ok := res.Content[0].(*mcpsdk.TextContent); !ok || !strings.Contains(txt.Text, "trip_wire") {
		t.Fatalf("expected facet text in Content, got %#v", res.Content)
	}

	// get_profile returns the L4 narrative markdown, also with NO structuredContent.
	if err := st.WriteProfile("math", "# Math\n\nrecovers from spirals.\n"); err != nil {
		t.Fatal(err)
	}
	pres, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"lens": "math"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pres.StructuredContent != nil {
		t.Fatalf("get_profile must not emit structuredContent; got %v", pres.StructuredContent)
	}
	if txt, ok := pres.Content[0].(*mcpsdk.TextContent); !ok || !strings.Contains(txt.Text, "recovers from spirals") {
		t.Fatalf("expected narrative markdown in Content, got %#v", pres.Content)
	}
}

func TestNormalizeRecordValidation(t *testing.T) {
	if _, err := normalizeRecord(recordInput{Observation: "x"}); err == nil {
		t.Error("missing session should error")
	}
	if _, err := normalizeRecord(recordInput{Session: "s"}); err == nil {
		t.Error("missing observation should error")
	}
	if _, err := normalizeRecord(recordInput{Session: " ", Observation: " "}); err == nil {
		t.Error("whitespace-only should error")
	}
	long := strings.Repeat("a", maxObsLen+1)
	if _, err := normalizeRecord(recordInput{Session: "s", Observation: long}); err == nil {
		t.Error("over-long observation should error")
	}
}

func TestNormalizeRecordClampsAndDefaults(t *testing.T) {
	got, err := normalizeRecord(recordInput{Session: "s", Observation: "o", Poignancy: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lens != store.LensDefault {
		t.Errorf("lens should default to %q, got %q", store.LensDefault, got.Lens)
	}
	if got.Poignancy != 5 {
		t.Errorf("poignancy 0 should default to 5, got %d", got.Poignancy)
	}
	// An absurd poignancy must be clamped so it can't force a review.
	hi, _ := normalizeRecord(recordInput{Session: "s", Observation: "o", Poignancy: 1000000})
	if hi.Poignancy != maxPoignancy {
		t.Errorf("poignancy should clamp to %d, got %d", maxPoignancy, hi.Poignancy)
	}
	// A normal value is preserved.
	ok, _ := normalizeRecord(recordInput{Session: "s", Observation: "o", Poignancy: 7})
	if ok.Poignancy != 7 {
		t.Errorf("valid poignancy should be preserved, got %d", ok.Poignancy)
	}
}
