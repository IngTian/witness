package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// (waitReady's own-budget bound and its accept/refuse cases live in serve_test.go.)

// A serve that DIES during the wait must be reported as such, not as a timeout — that distinction
// is what surfaces a real crash cause instead of burying it.
func TestWaitReadyReportsAnExitedServe(t *testing.T) {
	withShortTimeouts(t, 5*time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	s, url := readyServe(ts.URL)
	close(s.waitDone) // the child already exited
	err := s.waitReady(context.Background(), url)
	if err == nil {
		t.Fatal("an exited serve reported ready")
	}
	if !strings.Contains(err.Error(), "exited before") {
		t.Errorf("want the exit reported explicitly, got %v", err)
	}
}

// The caller's cancellation must still abort the wait promptly (a `distill stop` during Open).
func TestWaitReadyHonorsCallerCancellation(t *testing.T) {
	withShortTimeouts(t, 30*time.Second) // long, so only the cancel can end this
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer ts.Close()
	defer close(release)
	s, url := readyServe(ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.waitReady(ctx, url) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled wait reported ready")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("caller cancellation no longer aborts waitReady")
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

	srv := newOpenCodeServer(ts.URL, "", "", ts.Client())

	done := make(chan error, 1)
	go func() {
		_, err := srv.doJSON(context.Background(), http.MethodGet, "/api/session/x/message", nil, http.StatusOK)
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

	srv := newOpenCodeServer(ts.URL, "", "", ts.Client())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := srv.doJSON(ctx, http.MethodGet, "/api/session/x/message", nil, http.StatusOK)
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

	srv := newOpenCodeServer(ts.URL, "", "", ts.Client())
	data, err := srv.doJSON(context.Background(), http.MethodGet, "/api/session/x/message", nil, http.StatusOK)
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
	openCodeVersionCommand = func(ctx context.Context, _ string) ([]byte, error) {
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
	openCodeVersionCommand = func(ctx context.Context, _ string) ([]byte, error) {
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

	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("2.2.7\n"), nil }
	if err := ValidateOpenCodeCapability(context.Background()); err != nil {
		t.Errorf("2.2.7 must be accepted, got %v", err)
	}

	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("2.2.6\n"), nil }
	err := ValidateOpenCodeCapability(context.Background())
	if !errors.Is(err, ErrOpenCodeTooOld) {
		t.Fatalf("2.2.6 error = %v, want ErrOpenCodeTooOld", err)
	}
	if strings.Contains(err.Error(), "deadline") {
		t.Errorf("an old version was rejected by TIMEOUT instead of by version check: %v", err)
	}

	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("1.19.5\n"), nil }
	err = ValidateOpenCodeCapability(context.Background())
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Fatalf("1.19.5 error = %v, want ErrOpenCodeV1", err)
	}
}
