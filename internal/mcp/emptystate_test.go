package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
)

// callText drives one tool over the in-memory transport and returns its text content.
func callText(t *testing.T, fake *fakeMCPStore, tool string, args map[string]any) string {
	t.Helper()
	ctx := context.Background()
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

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// An empty answer must name the layer that is actually missing. Before #166 every empty
// get_facets said "the reviewer may not have run yet" — which, for an archive nothing is
// feeding, describes a fix that will never arrive and makes a never-filling install look
// like a working one. Wiring only the MCP server is easy to do by accident: every
// marketplace and host config generator emits an MCP-server entry and none mention capture.
func TestEmptyStateNamesTheMissingLayer(t *testing.T) {
	cases := []struct {
		name   string
		reason store.EmptyReason
		// want appears in the answer; absent must NOT, so each branch is pinned against
		// the wording of the other branches rather than merely being non-empty.
		want, absent string
	}{
		{
			name:   "no capture wired",
			reason: store.NoCapture,
			want:   "witness wire claude",
			absent: "reviewer",
		},
		{
			name:   "capturing but nothing mined for this lens",
			reason: store.NoObservations,
			want:   "lens backfill",
			absent: "reviewer",
		},
		{
			name:   "observations exist, reviewer has not folded them",
			reason: store.NoFacets,
			want:   "worker review",
			absent: "nothing is capturing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeMCPStore{whyEmpty: tc.reason}
			got := callText(t, fake, "get_facets", map[string]any{"lens": "default"})
			if !strings.Contains(got, tc.want) {
				t.Errorf("get_facets empty answer should mention %q, got: %s", tc.want, got)
			}
			if strings.Contains(got, tc.absent) {
				t.Errorf("get_facets empty answer must not mention %q for this cause, got: %s", tc.absent, got)
			}
		})
	}
}

// The search surface needs a different third branch: vector.Search is top-k with no score
// threshold, so zero hits never means "your query missed" — with observations present it
// means none of them carry embeddings. Telling that user to run the reviewer would be
// wrong twice over.
func TestEmptySearchBlamesEmbeddingsNotTheReviewer(t *testing.T) {
	fake := &fakeMCPStore{whyEmpty: store.NoFacets}
	got := callText(t, fake, "search_observations", map[string]any{"query": "anything", "lens": "default"})
	if !strings.Contains(got, "embeddings") {
		t.Errorf("empty search with observations present should blame embeddings, got: %s", got)
	}
	if strings.Contains(got, "worker review") {
		t.Errorf("empty search must not send the user to the reviewer, got: %s", got)
	}
}

// get_profile shares the facets hint, so it inherits the same three-way diagnosis rather than
// the old flat "nothing has been distilled for it".
func TestEmptyProfileNamesTheMissingLayer(t *testing.T) {
	fake := &fakeMCPStore{whyEmpty: store.NoCapture}
	got := callText(t, fake, "get_profile", map[string]any{"lens": "default"})
	if !strings.Contains(got, "witness wire claude") {
		t.Errorf("empty get_profile should name the capture fix, got: %s", got)
	}
}
