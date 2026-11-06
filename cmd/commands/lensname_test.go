package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// A lens registered as corpus-example mines as "regime" (its lens.json name). Every command takes
// the registered name, so each one must map it to the mined name before touching data: these
// three used the registered name as given, and so refused, found nothing, or filed an
// observation under a name no review reads.

// compact --lens corpus-example said "not enabled" and exited 1 for an enabled lens.
func TestCompactTakesTheRegisteredName(t *testing.T) {
	registerCompactFake.Do(func() { platform.Register(compactFakePlatform{}) })
	t.Setenv(noDefaultLensEnv, "1")
	t.Setenv("WITNESS_HOME", t.TempDir())
	st := openBase(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	if err := st.EnableLens("corpus-example"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfigString("runner", "fakecompact"); err != nil {
		t.Fatal(err)
	}
	var fs []store.Facet
	for _, k := range []string{"a", "b", "c"} {
		fs = append(fs, store.Facet{Lens: "regime", Dimension: "thinking", Key: k, LastSeen: "2026-10-01T00:00:00Z",
			Versions: []store.FacetVersion{{Value: "value " + k, Confidence: 0.7, RecordedAt: "2026-10-01T00:00:00Z",
				ValidFrom: "2026-10-01T00:00:00Z", BecauseOf: []string{"obs-" + k}}}})
	}
	if err := st.WriteFacets(fs); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() { err = cmdCompact(true, "corpus-example", nil) })
	if err != nil {
		t.Fatalf("compact --lens corpus-example: %v\n%s", err, out)
	}
	if a, ar := st.LensFacetCounts("regime"); a != 1 || ar != 2 {
		t.Fatalf("the plan must run over the lens's facets: %d active, %d archived\n%s", a, ar, out)
	}
}

// worker candidates corpus-example reported "0 observations" for a lens that had them.
func TestCandidatesTakesTheRegisteredName(t *testing.T) {
	st := readFixture(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	if err := st.AppendObservations([]store.Observation{
		{ID: "o1", Session: "s1", Lens: "regime", Dimension: "regimes", Observation: "one"},
		{ID: "o2", Session: "s2", Lens: "regime", Dimension: "regimes", Observation: "two"},
	}); err != nil {
		t.Fatal(err)
	}
	out, _, err := captureBoth(t, func() error { return cmdCandidates("corpus-example") })
	if err != nil || !strings.Contains(out, "lens corpus-example: 2 observations") {
		t.Fatalf("candidates must cluster the lens's own observations (err %v):\n%s", err, out)
	}
}

// observations record --lens corpus-example staged a row that drained into L1 under
// "corpus-example" — orphaned, since the review reads the lens under its mined name.
func TestObservationRecordReachesTheReview(t *testing.T) {
	t.Setenv(noDefaultLensEnv, "1")
	st := readFixture(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	if err := st.EnableLens("corpus-example"); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdoutErr(t, func() error {
		return cmdObservationRecord("s1", "corpus-example", "regimes", "turns when rates move", "", 6)
	}); err != nil {
		t.Fatal(err)
	}

	// Drain it the way the worker does. The session has no raw turns, so nothing is mined and
	// the runner must never be called; only the staged row moves into L1.
	lenses := activeLenses(st)
	if len(lenses) != 1 {
		t.Fatalf("active lenses %v", lenses)
	}
	w := &distill.Worker{Store: st, Embedder: fakeTestEmbedder{}, Lenses: lenses, Config: st.LoadConfig(),
		Run: func(context.Context, string, string, string) (string, error) {
			t.Fatal("a staged-only drain called the runner")
			return "", nil
		}}
	m, err := w.MineSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var existing []store.Observation
	if err := w.CommitMining(m, &existing); err != nil {
		t.Fatal(err)
	}

	// What the reviewer folds (reviewer.go: ReadObservationsSinceOrdered(ln.Name, ReviewRowid)).
	ln := lenses[0]
	obs, err := st.ReadObservationsSinceOrdered(ln.Name, st.ReviewRowid(ln.Name))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Observation != "turns when rates move" {
		t.Fatalf("the review of %q must see the recorded observation, got %+v", ln.Name, obs)
	}
}

// The MCP tools apply whatever mapping cmd injects (internal/mcp tests drive that with a fake);
// this pins that cmd injects the real one, and the wire command the CLI prints.
func TestMCPGetsTheLensMappingAndTheWireCommand(t *testing.T) {
	st := readFixture(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	opts := mcpOptions(st)
	if got := opts.DataName("corpus-example"); got != "regime" {
		t.Errorf("DataName(corpus-example) = %q, want regime", got)
	}
	if opts.Wire != wireCommand() || opts.Refresh == nil {
		t.Errorf("wire %q (want %q), refresh set %v", opts.Wire, wireCommand(), opts.Refresh != nil)
	}
}

type fakeTestEmbedder struct{}

func (fakeTestEmbedder) Embed(string) ([]float32, error) { return []float32{0.1, 0.2, 0.3}, nil }
