package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
)

// fakeMCPStore is a hand-written store.MCPStore with NO real *sql.DB behind it —
// proof that the MCP server depends only on the narrow store.MCPStore interface
// (issue #73-C1, Phase B), not the whole *store.Store god-object. It records the
// record-tool path and serves canned reads.
type fakeMCPStore struct {
	obs      []store.Observation
	facets   []store.Facet
	profiles map[string]string

	stagedCalls  int
	lastStaged   store.Observation
	deletedIDs   []string
	existsReturn bool
}

func (f *fakeMCPStore) ReadObservations(lens string) ([]store.Observation, error) {
	return f.obs, nil
}
func (f *fakeMCPStore) StageObservationCapped(o store.Observation, limit int) (bool, error) {
	f.stagedCalls++
	f.lastStaged = o
	return true, nil
}
func (f *fakeMCPStore) StagedExists(session, obsID string) bool { return f.existsReturn }
func (f *fakeMCPStore) ReadFacets() ([]store.Facet, error)      { return f.facets, nil }
func (f *fakeMCPStore) ReadProfile(lens string) (string, bool, error) {
	md, ok := f.profiles[lens]
	return md, ok, nil
}
func (f *fakeMCPStore) DeleteObservation(obsID string) (bool, error) {
	f.deletedIDs = append(f.deletedIDs, obsID)
	return true, nil
}

// compile-time proof the fake satisfies the interface the server accepts.
var _ store.MCPStore = (*fakeMCPStore)(nil)

// TestServerRunsAgainstFakeStore builds the MCP server with a fake MCPStore (no DB)
// and drives get_profile end-to-end over the in-memory transport, proving the server
// no longer needs a concrete *store.Store — the C1 decoupling goal for this consumer.
func TestServerRunsAgainstFakeStore(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMCPStore{profiles: map[string]string{"default": "# Profile\n\nfake-backed.\n"}}

	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := newServer(fake, fakeEmbedder{}, "v0.0.0-fake").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"lens": "default"},
	})
	if err != nil {
		t.Fatalf("call get_profile: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_profile returned tool error: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("get_profile returned no content")
	}
	// The fake served the canned profile — the server round-tripped a store.MCPStore
	// with no database.
	if tc, ok := res.Content[0].(*mcpsdk.TextContent); !ok || tc.Text == "" {
		t.Fatalf("expected non-empty text content, got %+v", res.Content[0])
	}
}
