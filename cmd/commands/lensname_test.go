package commands

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// A lens registered as corpus-example mines as "regime" (its lens.json name). Every command takes
// the registered name, so each one must map it to the mined name before touching data: compact
// used the registered name as given, and so refused.

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
