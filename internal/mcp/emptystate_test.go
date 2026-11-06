package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
)

// testOptions injects a wire command no hardcoded string could contain, so a hint that names it
// proves it came from cmd (which derives it from the install targets) rather than from this package.
var testOptions = Options{Version: "v0.0.0-fake", Wire: "witness wire <a|b>"}

// callText drives one tool over the in-memory transport and returns its text content.
func callText(t *testing.T, fake *fakeMCPStore, opts Options, tool string, args map[string]any) string {
	t.Helper()
	res, err := connect(t, fake, opts).CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	return resultText(res)
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
			want:   testOptions.Wire,
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
			got := callText(t, fake, testOptions, "get_facets", map[string]any{"lens": "default"})
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
	got := callText(t, fake, testOptions, "search_observations", map[string]any{"query": "anything", "lens": "default"})
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
	got := callText(t, fake, testOptions, "get_profile", map[string]any{"lens": "default"})
	if !strings.Contains(got, testOptions.Wire) {
		t.Errorf("empty get_profile should name the capture fix, got: %s", got)
	}
}
