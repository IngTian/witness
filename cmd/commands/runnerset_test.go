package commands

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// leakFakeCloses counts Close() calls on the leak-test fake runner (package-global so the
// registered platform can reach it; the test name-spaces the runtime as "fakeleak").
var leakFakeCloses int32

// leakFakeOpens counts Open() on this runner and on every fake that embeds it without
// overriding Open (compactFakeRunner, downFakeRunner): the newer-archive test asserts it stays
// flat, i.e. no runner set was opened before the refusal.
var leakFakeOpens int32

// registerLeakFake guards the registration: a second platform.Register panics, so a bare call
// died under -count=2.
var registerLeakFake sync.Once

// leakFakePlatform is a minimal registered platform whose runner Opens successfully and
// counts its Closes — used to prove newRunnerSet closes an already-opened non-default
// runner when the default runner is the one that fails (the 61d00fc leak fix).
type leakFakePlatform struct{}

func (leakFakePlatform) Name() string                                          { return "fakeleak" }
func (leakFakePlatform) SessionPrefix() string                                 { return "fakeleak:" }
func (leakFakePlatform) Capture(*store.Store, []byte, time.Time) (bool, error) { return false, nil }
func (leakFakePlatform) NewRunner(store.Config) platform.Runner                { return leakFakeRunner{} }

type leakFakeRunner struct{}

func (leakFakeRunner) Open(context.Context) error { atomic.AddInt32(&leakFakeOpens, 1); return nil }
func (leakFakeRunner) Close() error               { atomic.AddInt32(&leakFakeCloses, 1); return nil }
func (leakFakeRunner) Run(context.Context, string, string, string) (string, error) {
	return "[]", nil
}
func (leakFakeRunner) ValidateModels(context.Context, ...string) error { return nil }
func (leakFakeRunner) InvocationHint() string                          { return "fakeleak" }

// A lens routed to an UNKNOWN runner is circuit-broken (its RunFor returns a failing
// MineFunc) while lenses on the healthy default runner get a working one — the per-runtime
// isolation of #75 slice 2. An unknown runner name resolves-fail without spawning any
// process, and the default runtime is the in-process fake, so no real runtime is touched.
func TestRunnerSetCircuitBreaksUnknownRunner(t *testing.T) {
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := store.Config{Runner: "fakeleak"} // default runner: the fake, which answers "[]"
	lenses := []*lens.Lens{
		{Name: "default"},                 // default runner → healthy
		{Name: "bogus", Runner: "nosuch"}, // unknown runner → circuit-broken
	}
	rs, err := newRunnerSet(context.Background(), st, cfg, lenses)
	if err != nil {
		t.Fatalf("newRunnerSet must NOT fail just because a per-lens runner is broken: %v", err)
	}
	defer rs.Close()

	// The broken lens's MineFunc must return an error without any real call.
	brokenFn := rs.RunFor(lenses[1])
	if _, err := brokenFn(context.Background(), "m", "p", "in"); err == nil {
		t.Fatalf("a lens on an unknown runner must get a failing MineFunc (circuit-broken)")
	}

	// And the breaker must not spread: the lens on the healthy runtime still MINES.
	if _, err := rs.RunFor(lenses[0])(context.Background(), "m", "p", "in"); err != nil {
		t.Fatalf("a broken per-lens runner circuit-broke the healthy default runtime too: %v", err)
	}
}

// If the default runner itself is unresolvable (a config typo), newRunnerSet fails closed —
// the drain can't proceed because the default lens + unified summary ride the default.
func TestRunnerSetFailsOnBrokenDefaultRunner(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := store.Config{Runner: "nosuch-default"} // the default runner is bogus
	_, err = newRunnerSet(context.Background(), st, cfg, []*lens.Lens{{Name: "default"}})
	if err == nil {
		t.Fatalf("an unresolvable default runner must fail newRunnerSet closed")
	}
	if !strings.Contains(err.Error(), "nosuch-default") {
		t.Fatalf("the error should name the broken default runner, got: %v", err)
	}
}

// When the default runner fails but a non-default per-lens runtime opened successfully,
// newRunnerSet must CLOSE that opened runner before returning the fatal error — else the
// caller's `defer rs.Close()` never runs (rs is nil) and e.g. an opencode serve leaks.
// Regression for the 61d00fc leak fix; fails if runnerset.go's rs.Close()-on-fatal is removed.
func TestRunnerSetClosesOpenedRunnersWhenDefaultFails(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	atomic.StoreInt32(&leakFakeCloses, 0)

	cfg := store.Config{Runner: "nosuch-default"}           // default runner is bogus → fatal
	lenses := []*lens.Lens{{Name: "x", Runner: "fakeleak"}} // routes to the fake, which OPENS ok
	_, err = newRunnerSet(context.Background(), st, cfg, lenses)
	if err == nil {
		t.Fatalf("a broken default runner must fail newRunnerSet")
	}
	if got := atomic.LoadInt32(&leakFakeCloses); got != 1 {
		t.Fatalf("the successfully-opened non-default runner must be Closed exactly once on the fatal path, got %d Close() calls", got)
	}
}
