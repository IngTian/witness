package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
)

// The lens field must get the SAME trim + bound as every other recordInput field.
//
// It was the one field neither trimmed nor length-checked, so `{"lens": "default "}` — exactly
// the trailing-whitespace noise the other four are trimmed for — was stored as a DISTINCT lens:
// the call reported `recorded (default /...)` while the observation landed under a lens name no
// drain, review, or `witness facets` invocation would ever query. Silently orphaned data from a
// success response.
func TestNormalizeRecordTrimsAndBoundsLens(t *testing.T) {
	base := recordInput{Session: "s", Observation: "o", Dimension: "d"}

	t.Run("trailing whitespace is trimmed, not stored as a new lens", func(t *testing.T) {
		in := base
		in.Lens = "default "
		got, err := normalizeRecord(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Lens != "default" {
			t.Errorf("lens = %q, want %q — an untrimmed lens orphans the observation under a "+
				"name nothing queries", got.Lens, "default")
		}
	})

	t.Run("leading whitespace too", func(t *testing.T) {
		in := base
		in.Lens = "\t math\n"
		got, err := normalizeRecord(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Lens != "math" {
			t.Errorf("lens = %q, want %q", got.Lens, "math")
		}
	})

	t.Run("empty still defaults", func(t *testing.T) {
		for _, v := range []string{"", "   ", "\t\n"} {
			in := base
			in.Lens = v
			got, err := normalizeRecord(in)
			if err != nil {
				t.Fatalf("lens %q: %v", v, err)
			}
			if got.Lens != store.LensDefault {
				t.Errorf("lens %q => %q, want the default lens", v, got.Lens)
			}
		}
	})

	t.Run("absurdly long is rejected", func(t *testing.T) {
		in := base
		in.Lens = strings.Repeat("x", maxLensLen+1)
		if _, err := normalizeRecord(in); err == nil {
			t.Error("an over-long lens name must be rejected like every other field")
		}
	})

	t.Run("path-ish names are refused at the boundary", func(t *testing.T) {
		// No known escape exists (profileFileName rejects these downstream), but a lens name
		// DOES reach a file path there, and this is agent-supplied input — so refusing it here
		// means a future writer cannot inherit a hole.
		for _, v := range []string{"../etc", "a/b", `a\b`, "..", "x/../y"} {
			in := base
			in.Lens = v
			if _, err := normalizeRecord(in); err == nil {
				t.Errorf("lens %q must be refused", v)
			}
		}
	})

	t.Run("a legitimate name is untouched", func(t *testing.T) {
		for _, v := range []string{"default", "math", "code-review", "my_lens", "lens.v2"} {
			in := base
			in.Lens = v
			got, err := normalizeRecord(in)
			if err != nil {
				t.Fatalf("lens %q was rejected: %v", v, err)
			}
			if got.Lens != v {
				t.Errorf("lens %q was altered to %q", v, got.Lens)
			}
		}
	})
}

// The other fields must keep their existing behavior — the lens change must not perturb them.
func TestNormalizeRecordStillBoundsTheOtherFields(t *testing.T) {
	ok := recordInput{Session: " s ", Observation: " o ", Dimension: " d ", Evidence: " e "}
	got, err := normalizeRecord(ok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != "s" || got.Observation != "o" || got.Dimension != "d" || got.Evidence != "e" {
		t.Errorf("trimming regressed: %+v", got)
	}
	if got.Poignancy != 5 {
		t.Errorf("default poignancy = %d, want 5", got.Poignancy)
	}

	tooLong := recordInput{Session: "s", Observation: strings.Repeat("x", maxObsLen+1)}
	if _, err := normalizeRecord(tooLong); err == nil {
		t.Error("an over-long observation must still be rejected")
	}
	clamped := recordInput{Session: "s", Observation: "o", Poignancy: 999}
	got, err = normalizeRecord(clamped)
	if err != nil {
		t.Fatal(err)
	}
	if got.Poignancy != maxPoignancy {
		t.Errorf("poignancy clamp regressed: %d", got.Poignancy)
	}
	// A missing session or observation is still an error.
	if _, err := normalizeRecord(recordInput{Observation: "o"}); err == nil {
		t.Error("a missing session must be rejected")
	}
	if _, err := normalizeRecord(recordInput{Session: "s"}); err == nil {
		t.Error("a missing observation must be rejected")
	}
}

// The staged buffer needs a TOTAL bound, because the per-session one is keyed on an
// AGENT-SUPPLIED session id. A runaway agent that varies the id (per-turn ids, a hallucinated
// id, an incrementing counter) gets a fresh per-session budget on every call, so the
// per-session cap alone bounds nothing — and each row is up to 2000 chars of observation plus
// 2000 of evidence, durable in L1 and fed to the review model.
func TestStagedTotalCapCannotBeEvadedByRotatingTheSession(t *testing.T) {
	s := newStoreForBounds(t)

	const perSession, total = 3, 7
	staged := 0
	for i := 0; i < 50; i++ { // 50 DISTINCT sessions, so the per-session cap never bites
		ob := store.Observation{
			ID:          "obs-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Session:     "rotating-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Observation: "o",
		}
		inserted, err := s.StageObservationCapped(ob, perSession, total)
		if err != nil {
			t.Fatal(err)
		}
		if inserted {
			staged++
		}
	}
	if staged != total {
		t.Errorf("rotating the session id staged %d rows against a total cap of %d — the cap is "+
			"evadable", staged, total)
	}
	if got := s.StagedTotal(); got != total {
		t.Errorf("StagedTotal() = %d, want %d", got, total)
	}
}

// The per-session cap must still work on its own, and a disabled (<=0) total must not bound.
func TestStagedPerSessionCapStillApplies(t *testing.T) {
	s := newStoreForBounds(t)

	staged := 0
	for i := 0; i < 10; i++ {
		ob := store.Observation{ID: "o" + string(rune('0'+i)), Session: "one", Observation: "x"}
		inserted, err := s.StageObservationCapped(ob, 4, 0 /* total disabled */)
		if err != nil {
			t.Fatal(err)
		}
		if inserted {
			staged++
		}
	}
	if staged != 4 {
		t.Errorf("per-session cap staged %d, want 4", staged)
	}
}

// Both caps disabled means unlimited — StageObservation relies on that.
func TestStagedCapsDisabledMeansUnlimited(t *testing.T) {
	s := newStoreForBounds(t)
	for i := 0; i < 25; i++ {
		ob := store.Observation{ID: "o" + string(rune('a'+i)), Session: "one", Observation: "x"}
		inserted, err := s.StageObservationCapped(ob, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !inserted {
			t.Fatalf("insert %d declined with both caps disabled", i)
		}
	}
	if got := s.StagedTotal(); got != 25 {
		t.Errorf("StagedTotal() = %d, want 25", got)
	}
}

// A duplicate must still be a no-op rather than burning quota, under BOTH caps.
func TestStagedDuplicateDoesNotBurnEitherCap(t *testing.T) {
	s := newStoreForBounds(t)
	ob := store.Observation{ID: "same", Session: "one", Observation: "x"}
	for i := 0; i < 5; i++ {
		if _, err := s.StageObservationCapped(ob, 3, 10); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.StagedTotal(); got != 1 {
		t.Errorf("a re-recorded observation staged %d rows, want 1", got)
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
	cs, cleanup := connectBounds(t, ctx, fake)
	defer cleanup()

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
	cs, cleanup := connectBounds(t, ctx, fake)
	defer cleanup()

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
	cs, cleanup := connectBounds(t, ctx, fake)
	defer cleanup()

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
	cs, cleanup := connectBounds(t, ctx, fake)
	defer cleanup()

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

func connectBounds(t *testing.T, ctx context.Context, fake *fakeMCPStore) (*mcpsdk.ClientSession, func()) {
	t.Helper()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := newServer(fake, fakeEmbedder{}, "v0.0.0-fake").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		_ = ss.Close()
		t.Fatalf("client connect: %v", err)
	}
	return cs, func() { _ = cs.Close(); _ = ss.Close() }
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

func newStoreForBounds(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
