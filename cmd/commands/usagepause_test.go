package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// mapMeta is a map-backed store.MetaKV. The gate reads the pause outside its own mutex, so this
// lock keeps those reads race-free against the test's writes.
type mapMeta struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *mapMeta) MetaString(k string) string { s.mu.Lock(); defer s.mu.Unlock(); return s.m[k] }
func (s *mapMeta) SetMetaString(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[k] = v
	return nil
}

// freeRunner has the SharedAllowance capability (its opencode/* models draw on it). Only the
// capability matters to the gate: the calls it guards are counted on the MineFunc it wraps.
type freeRunner struct{ limitedPreviewRunner }

func (freeRunner) OnSharedAllowance(m string) bool { return strings.HasPrefix(m, "opencode/") }

const freeModel = "opencode/glm-5.3-flash-free"

var gateNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func testGate(meta store.MetaKV) *usageGate {
	g := newUsageGate(meta)
	g.now = func() time.Time { return gateNow }
	return g
}

func armedMeta(until time.Time) *mapMeta {
	m := &mapMeta{}
	_ = m.SetMetaString(usagePauseKey("fakefree"), until.UTC().Format(time.RFC3339))
	return m
}

// counted returns a MineFunc that counts its calls and answers with fn.
func counted(n *atomic.Int32, fn func() (string, error)) distill.MineFunc {
	return func(context.Context, string, string, string) (string, error) {
		n.Add(1)
		return fn()
	}
}

func ok() (string, error) { return "[]", nil }

func TestAQuotaRefusalPausesUntilTheReset(t *testing.T) {
	meta := &mapMeta{}
	reset := gateNow.Add(time.Hour)
	var n atomic.Int32
	run := testGate(meta).wrap("fakefree", freeRunner{}, counted(&n, func() (string, error) {
		// A non-UTC zone: the stored value must still be UTC.
		return "", &platform.QuotaExhaustedError{ResetAt: reset.In(time.FixedZone("x", 3600)), Runtime: "fakefree"}
	}))
	_, err := run(context.Background(), freeModel, "p", "i")
	var qe *platform.QuotaExhaustedError
	if !errors.As(err, &qe) {
		t.Fatalf("the refusal must pass through as a QuotaExhaustedError, got %v", err)
	}
	if got, want := meta.MetaString(usagePauseKey("fakefree")), reset.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("pause = %q, want %q", got, want)
	}
}

// Review Focus: review, claims, compaction and profile ride the same MineFunc, so this is the
// whole of "nothing reaches that allowance until the reset".
func TestPausedFreeModelCallsNeverReachTheRunner(t *testing.T) {
	reset := gateNow.Add(time.Hour)
	var n atomic.Int32
	run := testGate(armedMeta(reset)).wrap("fakefree", freeRunner{}, counted(&n, ok))
	for i := range 5 {
		_, err := run(context.Background(), freeModel, "p", "i")
		var qe *platform.QuotaExhaustedError
		if !errors.As(err, &qe) || !qe.ResetAt.Equal(reset) || qe.Runtime != "fakefree" {
			t.Fatalf("call %d: want QuotaExhausted{%s, fakefree}, got %v", i, reset, err)
		}
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("%d call(s) reached the runner while paused", got)
	}
}

// After T nothing is held back: every call runs (no probe election parks the others), and the
// first refusal re-arms the pause with its own reset.
func TestAfterTheResetEveryCallRuns(t *testing.T) {
	meta, reset := armedMeta(gateNow.Add(-time.Second)), gateNow.Add(time.Hour)
	var arrived atomic.Int32
	allIn := make(chan struct{})
	run := testGate(meta).wrap("fakefree", freeRunner{}, func(context.Context, string, string, string) (string, error) {
		if arrived.Add(1) == 3 {
			close(allIn)
		}
		select {
		case <-allIn:
		case <-time.After(5 * time.Second):
			t.Error("the three calls never overlapped")
		}
		return "", &platform.QuotaExhaustedError{ResetAt: reset, Runtime: "fakefree"}
	})
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { _, _ = run(context.Background(), freeModel, "p", "i") })
	}
	wg.Wait()
	if got := arrived.Load(); got != 3 {
		t.Fatalf("%d of 3 calls reached the runner after T", got)
	}
	if got, want := meta.MetaString(usagePauseKey("fakefree")), reset.Format(time.RFC3339); got != want {
		t.Fatalf("re-arm = %q, want %q", got, want)
	}
}

func TestPauseLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Two calls in flight before the first refusal lands: both are refused with the same reset.
	reset := gateNow.Add(time.Hour)
	var arrived atomic.Int32
	bothIn := make(chan struct{})
	run := testGate(&mapMeta{}).wrap("fakefree", freeRunner{}, func(context.Context, string, string, string) (string, error) {
		if arrived.Add(1) == 2 {
			close(bothIn)
		}
		select {
		case <-bothIn:
		case <-time.After(5 * time.Second):
			t.Error("the two calls never overlapped")
		}
		return "", &platform.QuotaExhaustedError{ResetAt: reset, Runtime: "fakefree"}
	})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _, _ = run(context.Background(), freeModel, "p", "i") })
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "allowance is used up"); got != 1 {
		t.Fatalf("want one pause line, got %d:\n%s", got, buf.String())
	}
}

// F15: Go cannot compare funcs, so assert behaviour — with the pause armed, a model off the shared
// allowance and any model on a runtime without the capability (its own pause armed too) still
// reach the runner.
func TestNonFreeModelsIgnoreThePause(t *testing.T) {
	meta := armedMeta(gateNow.Add(time.Hour))
	_ = meta.SetMetaString(usagePauseKey("claude"), gateNow.Add(time.Hour).Format(time.RFC3339))
	g := testGate(meta)
	var paid, noCap, free atomic.Int32
	if _, err := g.wrap("fakefree", freeRunner{}, counted(&paid, ok))(context.Background(), "openai/x", "p", "i"); err != nil || paid.Load() != 1 {
		t.Fatalf("a paid model must ignore the pause: err %v, calls %d", err, paid.Load())
	}
	if _, err := g.wrap("claude", limitedPreviewRunner{}, counted(&noCap, ok))(context.Background(), "opencode/x", "p", "i"); err != nil || noCap.Load() != 1 {
		t.Fatalf("a runner without SharedAllowance must ignore the pause: err %v, calls %d", err, noCap.Load())
	}
	_, _ = g.wrap("fakefree", freeRunner{}, counted(&free, ok))(context.Background(), freeModel, "p", "i")
	if free.Load() != 0 {
		t.Fatal("the free model on the same gate must stay paused")
	}
}

func TestRetryWakeupIncludesTheFreeModelPause(t *testing.T) {
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	pause := func(at time.Time) {
		if err := st.SetMetaString(usagePauseKey("opencode"), at.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}

	pause(now.Add(time.Hour))
	if at, ok := retryWakeupAt(st, now); !ok || !at.Equal(now.Add(time.Hour)) {
		t.Fatalf("no backoff: wakeup = %s, %v; want the pause end", at, ok)
	}
	if err := st.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNextAttempt("s", "default", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if at, ok := retryWakeupAt(st, now); !ok || !at.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("an earlier backoff must win: wakeup = %s, %v", at, ok)
	}
	pause(now.Add(-time.Minute))
	if at, ok := retryWakeupAt(st, now); !ok || !at.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("a past pause must be ignored: wakeup = %s, %v", at, ok)
	}
}

// countingFreeRunner is a lens-try runner on a free model: it counts the calls that reach it.
type countingFreeRunner struct {
	freeRunner
	calls atomic.Int32
	err   error
}

func (r *countingFreeRunner) Run(context.Context, string, string, string) (string, error) {
	r.calls.Add(1)
	if r.err != nil {
		return "", r.err
	}
	return "[]", nil
}

// F20: `lens try` opens its own runner rather than a runnerSet, so it carries the gate itself —
// a manual preview on a paused free model sends nothing, and a refusal there arms the pause.
func TestLensTryHonoursTheFreeModelPause(t *testing.T) {
	try := func(t *testing.T, r *countingFreeRunner) *store.Store {
		s := seedTryStore(t, store.RunnerOpenCode)
		previous := runnerForName
		runnerForName = func(string, store.Config) (platform.Runner, error) { return r, nil }
		t.Cleanup(func() { runnerForName = previous })
		return s
	}
	run := func(t *testing.T) {
		_ = captureStdout(t, func() { _ = cmdLensTry(validLensDir(t), lensTryOpts{nSessions: 1, model: freeModel}) })
	}

	t.Run("paused", func(t *testing.T) {
		r := &countingFreeRunner{freeRunner: freeRunner{limitedPreviewRunner{opened: new(bool)}}}
		s := try(t, r)
		if err := s.SetMetaString(usagePauseKey("opencode"), time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		run(t)
		if got := r.calls.Load(); got != 0 {
			t.Fatalf("lens try sent %d call(s) to a paused free model", got)
		}
	})
	t.Run("refusal arms", func(t *testing.T) {
		reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		r := &countingFreeRunner{freeRunner: freeRunner{limitedPreviewRunner{opened: new(bool)}},
			err: &platform.QuotaExhaustedError{ResetAt: reset, Runtime: "opencode"}}
		s := try(t, r)
		run(t)
		if got, want := s.MetaString(usagePauseKey("opencode")), reset.Format(time.RFC3339); got != want {
			t.Fatalf("a refusal in lens try must arm the pause: %q, want %q", got, want)
		}
	})
}

// The M13 skip predicate: true only when every active lens's extract and review calls sit on a
// runtime whose shared allowance is paused, and nothing staged waits for a commit.
func TestAllLensesPaused(t *testing.T) {
	registerFreeFake.Do(func() { platform.Register(freeFakePlatform{}) })
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	cfg := store.Config{Runner: "fakefree", TriageModel: freeModel}
	arm := func(st *store.Store, rt string, until time.Time) {
		if err := st.SetMetaString(usagePauseKey(rt), until.UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	one := []*lens.Lens{{Name: "a"}}
	for _, tc := range []struct {
		name   string
		lenses []*lens.Lens
		setup  func(st *store.Store)
		want   bool
	}{
		{"every lens paused", one, nil, true},
		{"no lenses", nil, nil, false},
		{"fakefree unarmed", one, func(st *store.Store) { _ = st.SetMetaString(usagePauseKey("fakefree"), "") }, false},
		{"fakefree reset passed", one, func(st *store.Store) { arm(st, "fakefree", time.Now().Add(-time.Minute)) }, false},
		{"paid extract model", []*lens.Lens{{Name: "a", ExtractModel: "openai/x"}}, nil, false},
		{"paid review model", []*lens.Lens{{Name: "a", ReviewModel: "openai/x"}}, nil, false},
		{"a staged observation", one, func(st *store.Store) {
			if err := st.StageObservation(store.Observation{ID: "o1", Session: "mcp:x"}); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a lens on a runtime without the capability", []*lens.Lens{{Name: "a"}, {Name: "b", Runner: "fakeleak"}}, nil, false},
	} {
		st := openTestStore(t)
		arm(st, "fakefree", time.Now().Add(time.Hour))
		arm(st, "fakeleak", time.Now().Add(time.Hour))
		if tc.setup != nil {
			tc.setup(st)
		}
		if got := allLensesPaused(st, cfg, tc.lenses, time.Now()); got != tc.want {
			t.Fatalf("%s: allLensesPaused = %v, want %v", tc.name, got, tc.want)
		}
	}
}
