package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// These bounds are liveness backstops, and an untested one is indistinguishable from an
// absent one. The hazard: the drain's ctx is signal-cancellable but carries NO deadline, the
// shared http.Client has no Timeout, and exec.CommandContext only kills on ctx cancellation.
// So a serve process that accepts a connection and then wedges — or an `opencode` CLI that
// hangs — blocked the worker forever while holding the machine-WIDE WorkerLock. That also
// stops `capture` from ever draining, so the whole tool goes silent with no error anywhere.

// withShortTimeouts shrinks the bounds so a test can prove they fire without waiting a
// real minute, restoring them afterwards.
func withShortTimeouts(t *testing.T, d time.Duration) {
	t.Helper()
	oldReq, oldProbe, oldHealth := openCodeRequestTimeout, openCodeProbeTimeout, healthCheckBudget
	openCodeRequestTimeout, openCodeProbeTimeout, healthCheckBudget = d, d, d
	t.Cleanup(func() {
		openCodeRequestTimeout, openCodeProbeTimeout, healthCheckBudget = oldReq, oldProbe, oldHealth
	})
}

// waitHealthy must honor its OWN budget even when a probe never answers.
//
// It runs during Open — i.e. FIRST, before doJSON is ever reached — and calls s.client.Do
// directly, so the per-request bound in doJSON does NOT cover it. Its loop condition
// (`for time.Now().Before(deadline)`) is only evaluated BETWEEN iterations, so one Do() that
// never returns was unbounded: measured before this fix, still blocked past 40s despite the
// documented 30s gate. The caller holds the machine-WIDE WorkerLock at that point, so
// `distill start` (and `doctor`, via validateRuntimeModels) hangs and `capture` never drains —
// the whole tool goes silent with no error anywhere.
func TestWaitHealthyBoundsAWedgedProbe(t *testing.T) {
	withShortTimeouts(t, 400*time.Millisecond)

	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // accept the connection, then never respond
	}))
	defer ts.Close()
	defer close(release)

	srv := &OpenCodeServer{
		baseURL:  ts.URL,
		client:   ts.Client(),
		waitDone: make(chan struct{}),
		logs:     &safeBuffer{},
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- srv.waitHealthy(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a server that never answered reported healthy")
		}
		// It must give up at ITS OWN budget, not at some larger per-request timeout.
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("waitHealthy took %s against a 400ms budget — the bound is not derived "+
				"from its own deadline", elapsed.Truncate(time.Millisecond))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("waitHealthy never returned: a wedged serve pins the machine-wide WorkerLock, " +
			"and capture stops draining with no error anywhere")
	}
}

// A healthy server must still be accepted promptly — the bound is a backstop, not a race.
func TestWaitHealthyAcceptsAHealthyServer(t *testing.T) {
	withShortTimeouts(t, 5*time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	srv := &OpenCodeServer{
		baseURL:  ts.URL,
		client:   ts.Client(),
		waitDone: make(chan struct{}),
		logs:     &safeBuffer{},
	}
	if err := srv.waitHealthy(context.Background()); err != nil {
		t.Fatalf("a healthy server was rejected: %v", err)
	}
}

// A serve process that DIES during the wait must be reported as such, not as a timeout —
// that distinction is what surfaces a real crash cause instead of burying it.
func TestWaitHealthyReportsAnExitedServe(t *testing.T) {
	withShortTimeouts(t, 5*time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	waitDone := make(chan struct{})
	close(waitDone) // the child already exited
	srv := &OpenCodeServer{
		baseURL:  ts.URL,
		client:   ts.Client(),
		waitDone: waitDone,
		logs:     &safeBuffer{},
	}
	err := srv.waitHealthy(context.Background())
	if err == nil {
		t.Fatal("an exited serve reported healthy")
	}
	if !strings.Contains(err.Error(), "exited before health check") {
		t.Errorf("want the exit reported explicitly, got %v", err)
	}
}

// The caller's cancellation must still abort the wait promptly (a `distill stop` during Open).
func TestWaitHealthyHonorsCallerCancellation(t *testing.T) {
	withShortTimeouts(t, 30*time.Second) // long, so only the cancel can end this
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer ts.Close()
	defer close(release)
	srv := &OpenCodeServer{
		baseURL:  ts.URL,
		client:   ts.Client(),
		waitDone: make(chan struct{}),
		logs:     &safeBuffer{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.waitHealthy(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled wait reported healthy")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("caller cancellation no longer aborts waitHealthy")
	}
}

// doJSON must give up on a server that accepts the request and never answers.
func TestDoJSONBoundsAWedgedRequest(t *testing.T) {
	withShortTimeouts(t, 150*time.Millisecond)

	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // accept, then never respond — a wedged serve process
	}))
	defer ts.Close()
	defer close(release)

	srv := &OpenCodeServer{baseURL: ts.URL, client: ts.Client()}

	done := make(chan error, 1)
	go func() {
		_, err := srv.doJSON(context.Background(), http.MethodGet, "/session/x/message", nil, http.StatusOK)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request that was never answered returned success")
		}
		// It must be the DEADLINE, not some other transport error.
		if !strings.Contains(err.Error(), "deadline exceeded") && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("want a deadline error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("doJSON never returned — the per-request bound is not effective; in production " +
			"this pins the machine-wide WorkerLock and the whole tool goes quiet")
	}
}

// The caller's ctx must still win when IT is cancelled first — the per-request bound layers
// on top of cancellation, it must not replace it (a `distill stop` has to tear down promptly).
func TestDoJSONStillHonorsCallerCancellation(t *testing.T) {
	withShortTimeouts(t, 30*time.Second) // long, so only the caller's cancel can end this

	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer ts.Close()
	defer close(release)

	srv := &OpenCodeServer{baseURL: ts.URL, client: ts.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := srv.doJSON(ctx, http.MethodGet, "/session/x/message", nil, http.StatusOK)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled request returned success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("caller cancellation no longer aborts a request — `distill stop` would hang")
	}
}

// A healthy request must NOT be aborted. The bound is a backstop, not a latency budget: if
// it were tight enough to kill normal work, it would silently break distillation.
func TestDoJSONDoesNotAbortAHealthyRequest(t *testing.T) {
	withShortTimeouts(t, 5*time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond) // slow but healthy
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	srv := &OpenCodeServer{baseURL: ts.URL, client: ts.Client()}
	data, err := srv.doJSON(context.Background(), http.MethodGet, "/session/x/message", nil, http.StatusOK)
	if err != nil {
		t.Fatalf("a healthy slow request was aborted: %v", err)
	}
	if !strings.Contains(string(data), "ok") {
		t.Errorf("body not returned: %q", data)
	}
}

// The default values must stay generous enough for real local work, and finite.
func TestTimeoutDefaultsAreFiniteAndGenerous(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"openCodeRequestTimeout": openCodeRequestTimeout,
		"openCodeProbeTimeout":   openCodeProbeTimeout,
	} {
		if d <= 0 {
			t.Errorf("%s = %s: an unbounded wait is the bug this guards", name, d)
		}
		if d < 10*time.Second {
			t.Errorf("%s = %s is tight enough to abort healthy local work", name, d)
		}
		if d > 5*time.Minute {
			t.Errorf("%s = %s is too long to be a liveness bound", name, d)
		}
	}
}

// ValidateOpenCodeCapability must give up on a hung `opencode --version` rather than block
// Open (and doctor) for as long as the deadline-less drain ctx lives.
func TestValidateOpenCodeCapabilityBoundsAHungVersionProbe(t *testing.T) {
	withShortTimeouts(t, 150*time.Millisecond)

	old := openCodeVersionCommand
	defer func() { openCodeVersionCommand = old }()
	// Faithful to exec.CommandContext's contract: it returns only when the ctx is done.
	openCodeVersionCommand = func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan error, 1)
	go func() { done <- ValidateOpenCodeCapability(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hung version probe reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ValidateOpenCodeCapability never returned — a hung `opencode --version` " +
			"blocks Open while holding the machine-wide WorkerLock")
	}
}

// The probe deadline must be derived from the caller's ctx, so an ALREADY-cancelled caller
// aborts immediately instead of waiting out the full bound.
func TestProbeDeadlineDerivesFromTheCallerContext(t *testing.T) {
	withShortTimeouts(t, 30*time.Second)

	old := openCodeVersionCommand
	defer func() { openCodeVersionCommand = old }()
	openCodeVersionCommand = func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead
	done := make(chan error, 1)
	go func() { done <- ValidateOpenCodeCapability(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled caller reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe ignored the caller's cancellation — it must derive from it, " +
			"not use context.Background()")
	}
}

// A healthy version probe still succeeds (the bound must not break the normal path), and a
// too-old version is still rejected on its merits rather than by timeout.
func TestValidateOpenCodeCapabilityStillJudgesVersionsOnTheirMerits(t *testing.T) {
	withShortTimeouts(t, 5*time.Second)
	old := openCodeVersionCommand
	defer func() { openCodeVersionCommand = old }()

	openCodeVersionCommand = func(context.Context) ([]byte, error) { return []byte("1.18.5\n"), nil }
	if err := ValidateOpenCodeCapability(context.Background()); err != nil {
		t.Errorf("1.18.5 must be accepted, got %v", err)
	}
	openCodeVersionCommand = func(context.Context) ([]byte, error) { return []byte("1.17.9\n"), nil }
	err := ValidateOpenCodeCapability(context.Background())
	if err == nil {
		t.Fatal("1.17.9 must be rejected")
	}
	if strings.Contains(err.Error(), "deadline") {
		t.Errorf("an old version was rejected by TIMEOUT instead of by version check: %v", err)
	}
}

// loadOpenCodeModels bounds its probe too. It is exercised through ValidateOpenCodeModelsIn
// with a cached provider so no `opencode` process is ever spawned — the point is that the
// cache short-circuit still works and the deadline plumbing did not break the happy path.
func TestLoadOpenCodeModelsUsesTheCacheWithoutSpawning(t *testing.T) {
	withShortTimeouts(t, 5*time.Second)
	old := openCodeVersionCommand
	defer func() { openCodeVersionCommand = old }()
	openCodeVersionCommand = func(context.Context) ([]byte, error) { return []byte("1.18.5\n"), nil }

	// Seed the cache for a provider name no real install would have, and remove it after so
	// the sync.Map does not leak into other tests in this package.
	const provider = "witness-test-provider"
	openCodeModelsCache.Store(provider, openCodeModelList{
		set: map[string]bool{provider + "/m1": true}, list: []string{provider + "/m1"},
	})
	t.Cleanup(func() { openCodeModelsCache.Delete(provider) })

	if err := ValidateOpenCodeModelsIn(context.Background(), "", provider+"/m1"); err != nil {
		t.Errorf("a cached model must validate without spawning `opencode models`: %v", err)
	}
	err := ValidateOpenCodeModelsIn(context.Background(), "", provider+"/absent")
	if err == nil {
		t.Fatal("a model absent from the cached list must be rejected")
	}
	if strings.Contains(err.Error(), "deadline") {
		t.Errorf("rejected by timeout rather than by availability: %v", err)
	}
}

// The source must keep every `opencode` spawn on a context, and the probe deadline must
// derive from the caller's ctx. A bare exec.Command here is the unreapable-orphan shape that
// leaked 366 `claude` children elsewhere in this repo.
func TestEveryOpenCodeSpawnIsContextBound(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for n, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, `exec.Command("opencode"`) {
			t.Errorf("server.go:%d spawns opencode WITHOUT a context: %s", n+1, trimmed)
		}
	}
	// loadOpenCodeModels must apply the probe bound (it builds its command inline).
	i := strings.Index(string(src), "func loadOpenCodeModels(")
	if i < 0 {
		t.Fatal("loadOpenCodeModels not found")
	}
	end := strings.Index(string(src)[i:], "\n}\n")
	fn := string(src)[i : i+end]
	if !strings.Contains(fn, "openCodeProbeTimeout") {
		t.Error("loadOpenCodeModels must bound its `opencode models` probe: a hung one blocks " +
			"Open/doctor while holding the machine-wide WorkerLock")
	}
	if !strings.Contains(fn, "context.WithTimeout(ctx") {
		t.Error("loadOpenCodeModels's deadline must derive from the CALLER's ctx, so a " +
			"cancelled drain aborts it promptly")
	}
}
