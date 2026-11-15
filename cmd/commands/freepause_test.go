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
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// mapMeta is a map-backed store.MetaKV. The gate touches it only under its own mutex; this one
// keeps the test's own reads race-free too.
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

// freeRunner has the FreeAllowance capability (its opencode/* models are free). Only the
// capability matters to the gate: the calls it guards are counted on the MineFunc it wraps.
type freeRunner struct{ limitedPreviewRunner }

func (freeRunner) OnFreeModel(m string) bool { return strings.HasPrefix(m, "opencode/") }

const freeModel = "opencode/glm-5.3-flash-free"

var gateNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func testGate(meta store.MetaKV) *freeGate {
	g := newFreeGate(meta)
	g.now = func() time.Time { return gateNow }
	return g
}

func armedMeta(until time.Time) *mapMeta {
	m := &mapMeta{}
	_ = m.SetMetaString(freeModelsPausedKey, until.UTC().Format(time.RFC3339))
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
	if got, want := meta.MetaString(freeModelsPausedKey), reset.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("pause = %q, want %q", got, want)
	}
	if until, ok := freeModelsPausedUntil(meta); !ok || !until.Equal(reset) {
		t.Fatalf("freeModelsPausedUntil = %s, %v; want %s", until, ok, reset)
	}
}

// Review Focus: review, claims, compaction and profile ride the same MineFunc, so this is the
// whole of "nothing reaches that free model until the reset".
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

// recv waits for ch, failing (not hanging) on a regression.
func recv[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// stillWaiting fails if ch delivers within a short window: its call must be parked on the probe.
func stillWaiting(t *testing.T, ch chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s returned (%v) while the probe was still in flight", what, err)
	case <-time.After(100 * time.Millisecond):
	}
}

// heldProbe is a pause past its T whose probe is parked inside the runner until release; the
// probe then answers verdict, and every later runner call answers "[]".
type heldProbe struct {
	run     distill.MineFunc
	meta    *mapMeta
	calls   atomic.Int32
	probe   chan error
	release func()
}

func startHeldProbe(t *testing.T, verdict error) *heldProbe {
	h := &heldProbe{meta: armedMeta(gateNow.Add(-time.Second)), probe: make(chan error, 1)}
	entered, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.release = func() { once.Do(func() { close(held) }) }
	t.Cleanup(h.release) // a failed test must not leave the probe parked
	h.run = testGate(h.meta).wrap("fakefree", freeRunner{}, func(context.Context, string, string, string) (string, error) {
		if h.calls.Add(1) == 1 {
			close(entered)
			<-held
			if verdict != nil {
				return "", verdict
			}
		}
		return "[]", nil
	})
	go func() { _, err := h.run(context.Background(), freeModel, "p", "i"); h.probe <- err }()
	recv(t, entered, "the probe to reach the runner")
	return h
}

// call starts one more gated call; its error arrives on the returned channel.
func (h *heldProbe) call(ctx context.Context) chan error {
	ch := make(chan error, 1)
	go func() { _, err := h.run(ctx, freeModel, "p", "i"); ch <- err }()
	return ch
}

// After T exactly one call probes. A call arriving meanwhile does not reach the runner until the
// probe settles, and after a successful probe it runs — refused, it would skip its session for the
// rest of the drain, and a cleared pause arms no wakeup to bring it back.
func TestAfterThePauseOneCallProbes(t *testing.T) {
	h := startHeldProbe(t, nil)
	second := h.call(context.Background())
	stillWaiting(t, second, "a call during the probe")
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("%d calls reached the runner while one probe was in flight, want 1", got)
	}
	h.release()
	if err := recv(t, h.probe, "the probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := recv(t, second, "the waiting call"); err != nil || h.calls.Load() != 2 {
		t.Fatalf("after a good probe the waiting call runs: err %v, runner calls %d", err, h.calls.Load())
	}
	if got := h.meta.MetaString(freeModelsPausedKey); got != "" {
		t.Fatalf("a successful probe must clear the pause, still %q", got)
	}
}

func TestAWaiterOnARefusedProbeGetsTheNewReset(t *testing.T) {
	reset2 := gateNow.Add(2 * time.Hour)
	h := startHeldProbe(t, &platform.QuotaExhaustedError{ResetAt: reset2, Runtime: "fakefree"})
	waiter := h.call(context.Background())
	stillWaiting(t, waiter, "a call during the probe")
	h.release()
	err := recv(t, waiter, "the waiting call")
	var qe *platform.QuotaExhaustedError
	if !errors.As(err, &qe) || !qe.ResetAt.Equal(reset2) {
		t.Fatalf("a waiter on a refused probe must be refused with the re-armed %s, got %v", reset2, err)
	}
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("only the probe may reach the runner, got %d calls", got)
	}
}

func TestAWaiterHonoursItsContext(t *testing.T) {
	h := startHeldProbe(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	waiter := h.call(ctx)
	stillWaiting(t, waiter, "a call during the probe")
	cancel()
	if err := recv(t, waiter, "the cancelled call"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled waiter must return context.Canceled, got %v", err)
	}
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("a cancelled waiter must not reach the runner, got %d calls", got)
	}
	// Settle the probe here, not at Cleanup: its late "allowance is back" line would land in the
	// next test's log buffer under -shuffle.
	h.release()
	recv(t, h.probe, "the probe")
}

// The drain recovers a miner's panic, so a probe that panics must still settle: otherwise every
// later free call waits on it for the rest of the run.
func TestAPanickingProbeStillSettles(t *testing.T) {
	past := gateNow.Add(-time.Second)
	meta := armedMeta(past)
	var n atomic.Int32
	run := testGate(meta).wrap("fakefree", freeRunner{}, func(context.Context, string, string, string) (string, error) {
		if n.Add(1) == 1 {
			panic("boom")
		}
		return "[]", nil
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the probe's panic must propagate")
			}
		}()
		_, _ = run(context.Background(), freeModel, "p", "i")
	}()
	if got, want := meta.MetaString(freeModelsPausedKey), past.Format(time.RFC3339); got != want {
		t.Fatalf("a panicked probe must leave the pause as it was: %q, want %q", got, want)
	}
	next := make(chan error, 1)
	go func() { _, err := run(context.Background(), freeModel, "p", "i"); next <- err }()
	if err := recv(t, next, "the call after a panicked probe"); err != nil || n.Load() != 2 {
		t.Fatalf("the next call must probe: err %v, runner calls %d", err, n.Load())
	}
}

func TestAProbeRefusalRearmsThePause(t *testing.T) {
	meta := armedMeta(gateNow.Add(-time.Second))
	reset2 := gateNow.Add(2 * time.Hour)
	var n atomic.Int32
	run := testGate(meta).wrap("fakefree", freeRunner{}, counted(&n, func() (string, error) {
		return "", &platform.QuotaExhaustedError{ResetAt: reset2, Runtime: "fakefree"}
	}))
	_, _ = run(context.Background(), freeModel, "p", "i")
	if got, want := meta.MetaString(freeModelsPausedKey), reset2.Format(time.RFC3339); got != want {
		t.Fatalf("pause = %q, want the re-armed %q", got, want)
	}
	_, _ = run(context.Background(), freeModel, "p", "i")
	if got := n.Load(); got != 1 {
		t.Fatalf("after the re-arm only the probe may have reached the runner, got %d calls", got)
	}
}

func TestAProbeOtherFailureKeepsThePause(t *testing.T) {
	past := gateNow.Add(-time.Second)
	meta := armedMeta(past)
	boom := errors.New("x")
	var n atomic.Int32
	run := testGate(meta).wrap("fakefree", freeRunner{}, counted(&n, func() (string, error) { return "", boom }))
	if _, err := run(context.Background(), freeModel, "p", "i"); !errors.Is(err, boom) {
		t.Fatalf("the probe's own error must pass through, got %v", err)
	}
	if got, want := meta.MetaString(freeModelsPausedKey), past.Format(time.RFC3339); got != want {
		t.Fatalf("a non-quota failure must leave the pause as it was: %q, want %q", got, want)
	}
	_, _ = run(context.Background(), freeModel, "p", "i")
	if got := n.Load(); got != 2 {
		t.Fatalf("the probing flag must clear so the next call probes again; runner calls %d, want 2", got)
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
	if got := strings.Count(buf.String(), "pausing every free-model call"); got != 1 {
		t.Fatalf("want one pause line, got %d:\n%s", got, buf.String())
	}
}

// clearFails is meta whose clear (an empty write) fails.
type clearFails struct{ mapMeta }

func (s *clearFails) SetMetaString(k, v string) error {
	if v == "" {
		return errors.New("disk full")
	}
	return s.mapMeta.SetMetaString(k, v)
}

// A probe that succeeds but cannot clear the pause says so, and does not claim calls resume.
func TestAFailedClearIsLoggedNotAnnounced(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	meta := &clearFails{}
	_ = meta.SetMetaString(freeModelsPausedKey, gateNow.Add(-time.Second).Format(time.RFC3339))
	var n atomic.Int32
	_, _ = testGate(meta).wrap("fakefree", freeRunner{}, counted(&n, ok))(context.Background(), freeModel, "p", "i")
	if out := buf.String(); !strings.Contains(out, "could not clear the free-model pause") || strings.Contains(out, "allowance is back") {
		t.Fatalf("want the clear failure logged and no resume line:\n%s", out)
	}
}

// F15: Go cannot compare funcs, so assert behaviour — with the pause armed, a paid model on a
// free-capable runtime and any model on a runtime without the capability still reach the runner.
func TestNonFreeModelsIgnoreThePause(t *testing.T) {
	meta := armedMeta(gateNow.Add(time.Hour))
	g := testGate(meta)
	var paid, noCap, free atomic.Int32
	if _, err := g.wrap("fakefree", freeRunner{}, counted(&paid, ok))(context.Background(), "openai/x", "p", "i"); err != nil || paid.Load() != 1 {
		t.Fatalf("a paid model must ignore the pause: err %v, calls %d", err, paid.Load())
	}
	if _, err := g.wrap("claude", limitedPreviewRunner{}, counted(&noCap, ok))(context.Background(), "opencode/x", "p", "i"); err != nil || noCap.Load() != 1 {
		t.Fatalf("a runner without FreeAllowance must ignore the pause: err %v, calls %d", err, noCap.Load())
	}
	_, _ = g.wrap("fakefree", freeRunner{}, counted(&free, ok))(context.Background(), freeModel, "p", "i")
	if free.Load() != 0 {
		t.Fatal("the free model on the same gate must stay paused")
	}
}

func TestRetryWakeupIncludesTheFreeModelPause(t *testing.T) {
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st := wakeupStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	pause := func(at time.Time) {
		if err := st.SetMetaString(freeModelsPausedKey, at.Format(time.RFC3339)); err != nil {
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
		if err := s.SetMetaString(freeModelsPausedKey, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
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
		if got, want := s.MetaString(freeModelsPausedKey), reset.Format(time.RFC3339); got != want {
			t.Fatalf("a refusal in lens try must arm the pause: %q, want %q", got, want)
		}
	})
}
