package opencode

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPrivateServeHelperProcess is the fake `opencode` every serve test spawns: this test binary,
// re-run through the execCommandContext seam (useFakeServe), so no real opencode ever runs. It is
// a no-op in the normal suite. Each mode is bounded: the lease modes end on stdin EOF, and the
// AfterFunc below ends ANY helper within a minute and a half, so one can never outlive a broken
// test for long (a witness test once leaked 366 unkillable `claude` children).
func TestPrivateServeHelperProcess(t *testing.T) {
	mode := os.Getenv("WITNESS_TEST_SERVE_MODE")
	if mode == "" {
		return
	}
	time.AfterFunc(90*time.Second, func() { os.Exit(2) })
	args := os.Args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[i+1:]
	}
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("opencode v2.3.0")
		os.Exit(0)
	}
	switch mode {
	case "exit":
		fmt.Fprint(os.Stderr, "boom")
		os.Exit(3)
	case "v1":
		fmt.Println("opencode server listening on http://127.0.0.1:1")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(4)
	}
	auth := "Basic " + basicAuthToken("opencode", os.Getenv("OPENCODE_PASSWORD"))
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != auth {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/api/info":
				_, _ = w.Write([]byte(`{"version":"2.3.0"}`))
			case "/api/model":
				_, _ = w.Write([]byte(`[]`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}()
	fmt.Printf("{\"url\":\"http://%s\"}\n", ln.Addr())
	_, _ = io.Copy(io.Discard, os.Stdin) // the lease: EOF is the only shutdown signal
	if mode == "ignore-eof" {
		time.Sleep(60 * time.Second)
	}
	os.Exit(0)
}

// useFakeServe routes every opencode spawn to TestPrivateServeHelperProcess in mode.
func useFakeServe(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("WITNESS_TEST_SERVE_MODE", mode)
	prev := execCommandContext
	t.Cleanup(func() { execCommandContext = prev })
	execCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return prev(ctx, os.Args[0], append([]string{"-test.run=^TestPrivateServeHelperProcess$", "--"}, args...)...)
	}
}

// startFake starts a lease serve on the helper and closes it at cleanup, so a failing assertion
// never leaves the helper behind.
func startFake(t *testing.T, root string) *privateServe {
	t.Helper()
	s, err := startPrivateServe(context.Background(), root, false)
	if err != nil {
		t.Fatalf("startPrivateServe: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if _, dup := m[k]; dup {
			t.Fatalf("duplicate env key %q", k)
		}
		m[k] = v
	}
	return m
}

func TestBuildServeCmdIsTheV2LeaseServe(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	// Inherited server credentials must never reach the private serve.
	t.Setenv("OPENCODE_SERVER_USERNAME", "user")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "user-pw")
	cmd := buildServeCmd(context.Background(), root, dir, "pw", false)
	want := []string{"serve", "--stdio", "--port", "0", "--hostname", "127.0.0.1", "--log-level", "error"}
	if !slices.Equal(cmd.Args[1:], want) {
		t.Fatalf("args = %q, want %q", cmd.Args[1:], want)
	}
	env := envMap(t, cmd.Env)
	for k, v := range map[string]string{
		"OPENCODE_PASSWORD":               "pw",
		"OPENCODE_CONFIG_DIR":             filepath.Join(dir, "config"),
		"XDG_STATE_HOME":                  filepath.Join(dir, "state"),
		"OPENCODE_DISABLE_PROJECT_CONFIG": "1",
		"OPENCODE_DISABLE_CLAUDE_CODE":    "1",
		"OPENCODE_DISABLE_AUTOUPDATE":     "1",
		"OPENCODE_DISABLE_FREE_MODELS":    "1",
		// openCodeCommand's isolation still holds under the serve's overlay.
		"OPENCODE_DB":    filepath.Join(root, "opencode-v2.db"),
		"XDG_DATA_HOME":  filepath.Join(root, "opencode-data"),
		"WITNESS_WORKER": "1",
	} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD", "OPENCODE_DISABLE_MODELS_FETCH"} {
		if _, ok := env[k]; ok {
			t.Errorf("env carries %s", k)
		}
	}
	if cmd.WaitDelay != serveCloseGrace {
		t.Errorf("WaitDelay = %s, want %s", cmd.WaitDelay, serveCloseGrace)
	}
	free := envMap(t, buildServeCmd(context.Background(), root, dir, "pw", true).Env)
	for _, k := range []string{"OPENCODE_DISABLE_FREE_MODELS", "OPENCODE_DISABLE_MODELS_FETCH"} {
		if _, ok := free[k]; ok {
			t.Errorf("a free-model serve carries %s", k)
		}
	}
}

func TestStartPrivateServeReadsTheURLAndAuthenticates(t *testing.T) {
	useFakeServe(t, "lease")
	s := startFake(t, t.TempDir())
	if !strings.HasPrefix(s.URL, "http://127.0.0.1:") {
		t.Fatalf("URL = %q", s.URL)
	}
	for auth, want := range map[string]int{
		s.AuthHeader(): http.StatusOK,
		"Basic " + basicAuthToken("opencode", "wrong"): http.StatusUnauthorized,
	} {
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/info", nil)
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET /api/info with %q = %d, want %d", auth, resp.StatusCode, want)
		}
	}
}

func TestPrivateServeCloseEndsTheLease(t *testing.T) {
	useFakeServe(t, "lease")
	s := startFake(t, t.TempDir())
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %s; stdin EOF should end the serve at once", d)
	}
	if code := s.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("serve exit code = %d, want 0 (a clean lease end)", code)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Errorf("serve dir survived Close: %v", err)
	}
}

func TestPrivateServeCloseKillsAServeThatIgnoresEOF(t *testing.T) {
	prev := serveCloseGrace
	serveCloseGrace = 200 * time.Millisecond
	t.Cleanup(func() { serveCloseGrace = prev })
	useFakeServe(t, "ignore-eof")
	s := startFake(t, t.TempDir())
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Close = nil for a serve it had to kill")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close never returned: a serve that ignores EOF must be killed after the grace")
	}
	if s.cmd.ProcessState.Success() {
		t.Error("the serve exited cleanly; it should have been killed")
	}
}

// A cancelled ctx (a drain stopped mid-Open, `lens try` interrupted) ends the lease the way Close
// does, through EOF and the serve's own exit path, not with exec.CommandContext's default SIGKILL.
func TestACancelledContextEndsTheLeaseInsteadOfKilling(t *testing.T) {
	useFakeServe(t, "lease")
	ctx, cancel := context.WithCancel(context.Background())
	s, err := startPrivateServe(ctx, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cancel()
	select {
	case <-s.waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the serve outlived its cancelled ctx")
	}
	if code := s.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("serve exit code = %d after the ctx was cancelled, want 0 (a lease end, not a kill)", code)
	}
}

func TestStartPrivateServeReportsAnEarlyExit(t *testing.T) {
	useFakeServe(t, "exit")
	_, err := startPrivateServe(context.Background(), t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "exited before") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the early exit and its stderr", err)
	}
}

func TestStartPrivateServeRefusesANonV2ReadyLine(t *testing.T) {
	useFakeServe(t, "v1")
	_, err := startPrivateServe(context.Background(), t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "v2.2.7") {
		t.Fatalf("err = %v, want the v2.2.7 requirement named", err)
	}
}

// readyServe is a privateServe that has already printed its address line, for waitReady tests.
func readyServe(url string) (*privateServe, chan string) {
	ch := make(chan string, 1)
	ch <- `{"url":"` + url + `"}`
	return &privateServe{Password: "pw", waitDone: make(chan struct{}), logs: &safeBuffer{}}, ch
}

// Readiness is an AUTHENTICATED /api/info that names a version: /global/health is the web UI and
// answers 200 to anyone, so a 200 alone proves nothing about which server answered.
func TestWaitReadyRequiresAnAuthenticatedInfoWithAVersion(t *testing.T) {
	prev := healthCheckBudget
	healthCheckBudget = 300 * time.Millisecond
	t.Cleanup(func() { healthCheckBudget = prev })
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"401", "", http.StatusUnauthorized, false},
		{"no version", `{}`, http.StatusOK, false},
		{"version", `{"version":"2.3.0"}`, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s *privateServe
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/info" || r.Header.Get("Authorization") != s.AuthHeader() {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			s, url := readyServe(ts.URL)
			err := s.waitReady(context.Background(), url)
			if tc.ok != (err == nil) {
				t.Fatalf("waitReady = %v, want ok=%v", err, tc.ok)
			}
			if tc.status == http.StatusUnauthorized && !strings.Contains(err.Error(), "401") {
				t.Errorf("err = %v, want the 401 named", err)
			}
			if tc.ok && s.URL != ts.URL {
				t.Errorf("URL = %q, want %q", s.URL, ts.URL)
			}
		})
	}
}

// The not-ready error names the last real answer, not the ctx error of the probe the budget cut
// off mid-flight: the serve answers 401 once, then never again until the budget ends.
func TestWaitReadyKeepsTheLastRealStatus(t *testing.T) {
	prev := healthCheckBudget
	healthCheckBudget = 500 * time.Millisecond
	t.Cleanup(func() { healthCheckBudget = prev })
	release := make(chan struct{})
	var probes atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		<-release
	}))
	defer ts.Close()
	defer close(release)
	s, url := readyServe(ts.URL)
	if err := s.waitReady(context.Background(), url); err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("err = %v, want the 401 named", err)
	}
}

// waitReady must honor its OWN budget even when a probe never answers. It runs during Open, under
// the machine-wide WorkerLock, so one Do() that never returns would hang `distill start` and
// `doctor` and stop capture from draining, with no error anywhere (measured before the
// per-probe deadline: still blocked past 40 s against a 30 s gate).
func TestWaitReadyBoundsAWedgedProbe(t *testing.T) {
	withShortTimeouts(t, 400*time.Millisecond)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // accept the connection, then never respond
	}))
	defer ts.Close()
	defer close(release)
	s, url := readyServe(ts.URL)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.waitReady(context.Background(), url) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a server that never answered reported ready")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("waitReady took %s against a 400ms budget", d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("waitReady never returned against a wedged serve")
	}
}

// `lens try` opens a runner without WorkerLock, so its serve can run beside a worker's on the same
// runtime root. Each serve owns its own dir, and closing one must not touch the other's config.
func TestTwoServesKeepSeparateConfigDirs(t *testing.T) {
	useFakeServe(t, "lease")
	root := t.TempDir()
	a, b := startFake(t, root), startFake(t, root)
	if a.Dir == b.Dir {
		t.Fatalf("two serves share %s", a.Dir)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.ConfigPath()); err != nil {
		t.Errorf("closing one serve removed the other's config: %v", err)
	}
}

// A serve dir outlives its serve only when witness was hard-killed before Close; the next start
// sweeps the old ones and leaves recent ones (possibly a live sibling's) alone.
func TestStaleServeDirsAreSwept(t *testing.T) {
	useFakeServe(t, "lease")
	root := t.TempDir()
	old, fresh := filepath.Join(root, "opencode-serve", "old"), filepath.Join(root, "opencode-serve", "fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	startFake(t, root)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an 8-day-old serve dir survived the start: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh serve dir was swept: %v", err)
	}
}

// An empty root used to mean the AMBIENT OpenCode database (#152's trap): the `opencode models`
// probe and serve itself ran against the USER's opencode.db. Both entry points refuse it before
// spawning; the stub fails loudly if anything is spawned at all. The cached provider means the
// pre-fix ValidateOpenCodeModelsIn returned nil here instead of spawning.
func TestEmptyRuntimeRootIsRefused(t *testing.T) {
	prevVersion, prevExec := openCodeVersionCommand, execCommandContext
	t.Cleanup(func() { openCodeVersionCommand, execCommandContext = prevVersion, prevExec })
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("opencode v2.2.7\n"), nil }
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("spawned %s %v with no runtime root", name, args)
		return exec.CommandContext(ctx, os.Args[0], "-test.run=XXX_NONE")
	}
	const provider = "witness-test-emptyroot"
	openCodeModelsCache.Store(provider, openCodeModelList{set: map[string]bool{provider + "/m": true}})
	t.Cleanup(func() { openCodeModelsCache.Delete(provider) })

	if s, err := startPrivateServe(context.Background(), "", false); err == nil {
		_ = s.Close()
		t.Error("startPrivateServe accepted an empty runtime root")
	}
	if err := ValidateOpenCodeModelsIn(context.Background(), "", provider+"/m"); err == nil {
		t.Error("ValidateOpenCodeModelsIn accepted an empty runtime root")
	}
}

func TestAllowsFreeOnlyForANamedFreeModel(t *testing.T) {
	for _, tc := range []struct {
		models []string
		want   bool
	}{
		{nil, false},
		{[]string{"openai/x"}, false},
		{[]string{"openai/x", "opencode/glm-5.3-flash-free"}, true},
		{[]string{"opencode-go/x"}, false},
	} {
		if got := allowsFree(tc.models); got != tc.want {
			t.Errorf("allowsFree(%q) = %v, want %v", tc.models, got, tc.want)
		}
	}
}

// StartOpenCodeServerIn wires the lease serve into the HTTP client: address, credentials and the
// private config path all come from the serve, and Close ends the lease.
func TestStartOpenCodeServerInWrapsTheLeaseServe(t *testing.T) {
	useFakeServe(t, "lease")
	srv, err := StartOpenCodeServerIn(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.baseURL != srv.serve.URL || srv.authHeader != srv.serve.AuthHeader() || srv.configPath != srv.serve.ConfigPath() {
		t.Fatalf("server not built from its serve: %+v", srv)
	}
	if _, err := srv.doJSON(context.Background(), http.MethodGet, "/api/info", nil, http.StatusOK); err != nil {
		t.Fatalf("authenticated request through the server: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if !srv.serve.cmd.ProcessState.Exited() {
		t.Error("Close left the serve running")
	}
	// A bare server (tests build ~20 of them) has no serve; Close must still be a no-op.
	if err := newOpenCodeServer("http://127.0.0.1:1", "Basic x", "", http.DefaultClient).Close(); err != nil {
		t.Errorf("bare Close = %v", err)
	}
}
