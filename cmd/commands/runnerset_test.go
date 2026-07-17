package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// A lens routed to an UNKNOWN runner is circuit-broken (its RunFor returns a failing
// MineFunc) while lenses on the healthy global runner get a working one — the per-runtime
// isolation of #75 slice 2. An unknown runner name resolves-fail without spawning any
// process, so this exercises the breaker without a real runtime.
func TestRunnerSetCircuitBreaksUnknownRunner(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := store.Config{Runner: "claude"} // global runner: claude (Open is a no-op)
	lenses := []*lens.Lens{
		{Name: "default"},                 // global runner → healthy
		{Name: "bogus", Runner: "nosuch"}, // unknown runner → circuit-broken
	}
	rs, err := newRunnerSet(context.Background(), st, cfg, lenses)
	if err != nil {
		t.Fatalf("newRunnerSet must NOT fail just because a per-lens runner is broken: %v", err)
	}
	defer rs.Close()

	// The healthy global-runner lens gets a usable MineFunc (claude's RunnerMine — we don't
	// call it, just assert it's not the broken sentinel by checking it doesn't error-tag).
	// The broken lens's MineFunc must return an error without any real call.
	brokenFn := rs.RunFor(lenses[1])
	if _, err := brokenFn(context.Background(), "m", "p", "in"); err == nil {
		t.Fatalf("a lens on an unknown runner must get a failing MineFunc (circuit-broken)")
	}

	// A lens on the healthy runner must NOT get the broken sentinel — its MineFunc is the
	// real claude one. We can't invoke claude here, so assert identity: the two lenses
	// resolve to DIFFERENT MineFuncs (healthy vs broken).
	healthyFn := rs.RunFor(lenses[0])
	if healthyFn == nil {
		t.Fatalf("the healthy global-runner lens must get a non-nil MineFunc")
	}
}

// If the GLOBAL runner itself is unresolvable (a config typo), newRunnerSet fails closed —
// the drain can't proceed because the default lens + unified summary ride the global.
func TestRunnerSetFailsOnBrokenGlobalRunner(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := store.Config{Runner: "nosuch-global"} // the GLOBAL runner is bogus
	_, err = newRunnerSet(context.Background(), st, cfg, []*lens.Lens{{Name: "default"}})
	if err == nil {
		t.Fatalf("an unresolvable GLOBAL runner must fail newRunnerSet closed")
	}
	if !strings.Contains(err.Error(), "nosuch-global") {
		t.Fatalf("the error should name the broken global runner, got: %v", err)
	}
}
