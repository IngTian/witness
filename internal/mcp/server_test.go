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

// connect serves st over the in-memory transport and returns a connected client; both ends
// close when the test ends.
func connect(t *testing.T, st store.MCPStore, opts Options) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := newServer(st, fakeEmbedder{}, opts).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

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
	cs := connect(t, st, Options{Version: "v9.9.9-test"})

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

// The MCP initialize handshake must report the build version passed in from cmd
// (previously hardcoded "0.1.0"), and fall back to "dev" when empty. Registries
// and clients read this ServerInfo.Version (issue #45).
func TestServerReportsInjectedVersion(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	check := func(in, want string) {
		t.Helper()
		got := connect(t, st, Options{Version: in}).InitializeResult().ServerInfo.Version
		if got != want {
			t.Errorf("version=%q: handshake reported %q, want %q", in, got, want)
		}
	}
	check("v1.2.3", "v1.2.3")
	check("", "dev") // empty (bare `go build`) → dev, never a stale hardcoded number
}
