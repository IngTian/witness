package mcp

import (
	"context"
	"strings"
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

	stagedCalls       int
	stagedTotalReturn int
	lastStaged        store.Observation
	deletedIDs        []string
	existsReturn      bool
	// readCalls counts full-corpus reads, so a test can prove search short-circuits an
	// oversized query BEFORE paying for one.
	readCalls int
	// stagedReturn is what StageObservationCapped reports. Defaults to false via the zero
	// value, so tests that need a successful stage set it explicitly; the pre-existing
	// TestServerRecordDeleteSearch sets it to true.
	stagedReturn bool
}

func (f *fakeMCPStore) ReadObservations(lens string) ([]store.Observation, error) {
	f.readCalls++
	return f.obs, nil
}

// stagedTotalReturn lets a test drive the global-cap branch without staging 5000 rows.
func (f *fakeMCPStore) StagedTotal() int { return f.stagedTotalReturn }

func (f *fakeMCPStore) StageObservationCapped(o store.Observation, limit, totalLimit int) (bool, error) {
	f.stagedCalls++
	f.lastStaged = o
	return f.stagedReturn, nil
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
	knownProfile := "# Profile\n\nA KNOWN fake-backed profile for default.\n"
	fake := &fakeMCPStore{profiles: map[string]string{"default": knownProfile}}

	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := newServer(fake, fakeEmbedder{}, "v0.0.0-fake", nil).Connect(ctx, serverT, nil)
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
	// with no database. Assert the returned text CONTAINS the known profile content
	// (proving it read the fake, not the server's not-found fallback).
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok || tc.Text == "" {
		t.Fatalf("expected non-empty text content, got %+v", res.Content[0])
	}
	if !strings.Contains(tc.Text, "KNOWN fake-backed profile") {
		t.Fatalf("returned profile missing known content (got server fallback, not fake): %s", tc.Text)
	}
}

// TestServerRecordDeleteSearch exercises the MCP server's record_observation,
// delete_observation, and search_observations against the fake store, proving the
// fake's recorders work and those tools are wired correctly (issue #97).
func TestServerRecordDeleteSearch(t *testing.T) {
	ctx := context.Background()
	// The obs needs an embedding for vector search; fakeEmbedder returns [0.1,0.2,0.3].
	fake := &fakeMCPStore{
		obs: []store.Observation{{
			ID: "obs1", Lens: "default", Dimension: "growth",
			Observation: "canned obs", Embedding: []float32{0.1, 0.2, 0.3},
		}},
		profiles:     map[string]string{},
		existsReturn: false, // dedup: not already staged
		stagedReturn: true,  // the store accepts the insert
	}

	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := newServer(fake, fakeEmbedder{}, "v0.0.0-fake", nil).Connect(ctx, serverT, nil)
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

	// record_observation: stage a new obs.
	recRes, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "record_observation",
		Arguments: map[string]any{
			"session":     "sess1",
			"lens":        "default",
			"dimension":   "growth",
			"observation": "learned something",
			"poignancy":   7,
		},
	})
	if err != nil || recRes.IsError {
		t.Fatalf("record_observation failed: err=%v, res=%+v", err, recRes)
	}
	// Verify the fake recorded it.
	if fake.stagedCalls != 1 {
		t.Fatalf("fake.stagedCalls: want 1, got %d", fake.stagedCalls)
	}
	if fake.lastStaged.Session != "sess1" || fake.lastStaged.Observation != "learned something" {
		t.Fatalf("fake.lastStaged wrong: %+v", fake.lastStaged)
	}

	// delete_observation: delete an obs.
	delRes, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "delete_observation",
		Arguments: map[string]any{"obs_id": "obs1"},
	})
	if err != nil || delRes.IsError {
		t.Fatalf("delete_observation failed: err=%v, res=%+v", err, delRes)
	}
	// Verify the fake recorded the delete.
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "obs1" {
		t.Fatalf("fake.deletedIDs: want [obs1], got %v", fake.deletedIDs)
	}

	// search_observations: search returns the canned obs.
	searchRes, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "search_observations",
		Arguments: map[string]any{"query": "growth", "lens": "default", "k": 5},
	})
	if err != nil || searchRes.IsError {
		t.Fatalf("search_observations failed: err=%v, res=%+v", err, searchRes)
	}
	if len(searchRes.Content) == 0 {
		t.Fatal("search_observations returned no content")
	}
	tc, ok := searchRes.Content[0].(*mcpsdk.TextContent)
	if !ok || tc.Text == "" {
		t.Fatalf("search_observations: expected text content, got %+v", searchRes.Content[0])
	}
	// The fake's canned obs should appear in the search result.
	if !strings.Contains(tc.Text, "canned obs") {
		t.Fatalf("search result missing the fake's canned obs: %s", tc.Text)
	}
}
