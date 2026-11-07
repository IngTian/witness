package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
)

// normalizeRecord is record_observation's trust boundary: the input comes from any MCP client.
// Every text field is trimmed — lens included: it was once the one field neither trimmed nor
// bounded, so `{"lens": "default "}` reported `recorded (default /...)` while the row landed
// under a lens no drain, review, or `witness facets` would ever query. Lengths are bounded,
// path-ish lens names refused (a lens name reaches a file path in profileFileName), and
// poignancy defaulted and clamped so one call cannot force a review.
func TestNormalizeRecord(t *testing.T) {
	in := func(lens string, poignancy int) recordInput {
		return recordInput{Session: "s", Observation: "o", Lens: lens, Poignancy: poignancy}
	}
	type tcase struct {
		name     string
		in, want recordInput
	}
	cases := []tcase{
		{"trims every field", recordInput{Session: " s ", Lens: "default ", Dimension: " d ", Observation: " o ", Evidence: " e ", Poignancy: 7},
			recordInput{Session: "s", Lens: "default", Dimension: "d", Observation: "o", Evidence: "e", Poignancy: 7}},
		{"leading lens whitespace", in("\t math\n", 7), in("math", 7)},
		{"empty lens defaults", in("", 7), in(store.LensDefault, 7)},
		{"blank lens defaults", in("   ", 7), in(store.LensDefault, 7)},
		{"tab-newline lens defaults", in("\t\n", 7), in(store.LensDefault, 7)},
		{"poignancy 0 defaults to 5", in("math", 0), in("math", 5)},
		{"poignancy clamps", in("math", 1000000), in("math", maxPoignancy)},
	}
	for _, name := range []string{"default", "math", "code-review", "my_lens", "lens.v2"} {
		cases = append(cases, tcase{"legitimate lens " + name, in(name, 7), in(name, 7)})
	}
	for _, tc := range cases {
		got, err := normalizeRecord(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: normalizeRecord(%+v) = %+v, %v; want %+v", tc.name, tc.in, got, err, tc.want)
		}
	}

	refused := map[string]recordInput{
		"missing session":       {Observation: "o"},
		"missing observation":   {Session: "s"},
		"whitespace-only":       {Session: " ", Observation: " "},
		"over-long observation": {Session: "s", Observation: strings.Repeat("x", maxObsLen+1)},
		"over-long evidence":    {Session: "s", Observation: "o", Evidence: strings.Repeat("x", maxEvidenceLen+1)},
		"over-long dimension":   {Session: "s", Observation: "o", Dimension: strings.Repeat("x", maxDimensionLen+1)},
		"over-long lens":        in(strings.Repeat("x", maxLensLen+1), 7),
	}
	for _, v := range []string{"../etc", "a/b", `a\b`, "..", "x/../y"} {
		refused["path-ish lens "+v] = in(v, 7)
	}
	for name, r := range refused {
		if _, err := normalizeRecord(r); err == nil {
			t.Errorf("%s: normalizeRecord(%+v) accepted it", name, r)
		}
	}
}

// search_observations must REFUSE an oversized query instead of handing it to the tokenizer.
//
// The tokenizer allocates per-byte alignment tables plus a per-byte ChangeMap, so a
// multi-megabyte query costs on the order of a gigabyte per megabyte of input — inside the
// LONG-LIVED MCP server process that serves every other tool. A confused agent pasting a whole
// file into search could therefore OOM the server rather than merely get a bad answer. Refusing
// early also skips the full-corpus read below it, so an oversized call costs nothing.
func TestSearchRefusesAnOversizedQuery(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMCPStore{
		obs: []store.Observation{{
			ID: "obs1", Lens: "default", Dimension: "growth",
			Observation: "canned obs", Embedding: []float32{0.1, 0.2, 0.3},
		}},
		profiles: map[string]string{},
	}
	cs := connect(t, fake, Options{Version: "v0.0.0-fake"})

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "search_observations",
		Arguments: map[string]any{"query": strings.Repeat("x", maxQueryLen+1)},
	})
	if err != nil {
		t.Fatalf("the tool call itself should succeed with an error RESULT: %v", err)
	}
	if !res.IsError {
		t.Fatal("an oversized query was accepted; it reaches the tokenizer and can exhaust the " +
			"long-lived MCP server's memory")
	}
	if txt := resultText(res); !strings.Contains(txt, "too long") {
		t.Errorf("the error should say the query is too long, got %q", txt)
	}
	// And it must short-circuit BEFORE the expensive full-corpus read.
	if fake.readCalls != 0 {
		t.Errorf("an oversized query still read the whole corpus (%d reads)", fake.readCalls)
	}
}

// An ordinary query must still work, and an empty one must be a clear error rather than an
// embedding of "".
func TestSearchStillAcceptsANormalQuery(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMCPStore{
		obs: []store.Observation{{
			ID: "obs1", Lens: "default", Dimension: "growth",
			Observation: "canned obs", Embedding: []float32{0.1, 0.2, 0.3},
		}},
		profiles: map[string]string{},
	}
	cs := connect(t, fake, Options{Version: "v0.0.0-fake"})

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "search_observations",
		Arguments: map[string]any{"query": "how do I handle uncertainty"},
	})
	if err != nil || res.IsError {
		t.Fatalf("a normal query must succeed: err=%v res=%+v", err, res)
	}
	if fake.readCalls == 0 {
		t.Error("a valid query should have read the corpus")
	}

	empty, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "search_observations",
		Arguments: map[string]any{"query": "   "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.IsError {
		t.Error("a blank query should be refused, not embedded as empty text")
	}
}

// record_observation must report the GLOBAL cap distinctly, so a client is not told to split
// work across sessions when the shared buffer is what is full (splitting would not help).
func TestRecordReportsTheGlobalCapDistinctly(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMCPStore{
		profiles:          map[string]string{},
		existsReturn:      false, // not a duplicate
		stagedReturn:      false, // the store declined the insert
		stagedTotalReturn: maxStagedTotal,
	}
	cs := connect(t, fake, Options{Version: "v0.0.0-fake"})

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "record_observation",
		Arguments: map[string]any{
			"session": "s1", "observation": "o", "dimension": "growth",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a declined stage must be an error result")
	}
	txt := resultText(res)
	if !strings.Contains(txt, "buffer is full") {
		t.Errorf("at the GLOBAL cap the message must name it, not blame the per-session limit; got %q", txt)
	}
	if strings.Contains(txt, "per session") {
		t.Errorf("the global-cap message must not tell the client to split sessions; got %q", txt)
	}
}

// Below the global cap, a declined stage must still report the PER-SESSION limit.
func TestRecordStillReportsThePerSessionCap(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMCPStore{
		profiles:          map[string]string{},
		existsReturn:      false,
		stagedReturn:      false,
		stagedTotalReturn: 1, // nowhere near the global cap
	}
	cs := connect(t, fake, Options{Version: "v0.0.0-fake"})

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "record_observation",
		Arguments: map[string]any{
			"session": "s1", "observation": "o", "dimension": "growth",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a declined stage must be an error result")
	}
	if txt := resultText(res); !strings.Contains(txt, "per session") {
		t.Errorf("below the global cap the per-session limit must be named; got %q", txt)
	}
}

func resultText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
