package opencode

// The private serve: one `opencode serve --stdio` per runner Open, held on a stdin LEASE.
//
// In stdio mode the serve prints one `{"url":…}` line and then waits for EOF on stdin, which is
// its only shutdown signal (no parent-pid watch; windows.md §C2). Witness holds the write end, so
// the serve ends when witness closes it OR when witness dies, however it dies: a SIGKILL, an OOM
// kill or a Windows TerminateProcess closes the handle, and the serve exits ~0.05 s later
// (verified on macOS, runner.md (a)). That is what replaced the pid-file and `ps` reapers: an
// orphaned serve cannot outlive its owner, so there is nothing to find later. Go marks the pipe
// CLOEXEC (and on Windows passes only the child's std handles), so no sibling child can inherit
// the write end and keep a dead worker's serve alive.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// serveCloseGrace is how long Close waits for the serve to exit on EOF before killing it, and the
// cmd.WaitDelay backstop: a grandchild still holding the stdout pipe (the #218 npm `.cmd` hang,
// where cmd.exe dies but opencode.exe keeps the pipe) cannot make Wait block past it. A var so a
// test can shrink it.
var serveCloseGrace = 5 * time.Second

type privateServe struct {
	URL, Password, Dir string

	cmd   *exec.Cmd
	stdin io.WriteCloser // the lease; never nil once started (a nil Stdin is NUL, i.e. instant EOF)
	logs  *safeBuffer

	// waitDone is CLOSED once cmd.Wait returns, with its error stored in waitErr first, so both
	// waitReady's early-exit check and Close can observe it any number of times.
	waitDone chan struct{}
	waitErr  error

	once     sync.Once
	closeErr error
}

func (s *privateServe) AuthHeader() string {
	return "Basic " + basicAuthToken("opencode", s.Password)
}

func (s *privateServe) ConfigPath() string {
	return filepath.Join(s.Dir, "config", "opencode.json")
}

// startPrivateServe starts a lease serve under runtimeRoot and waits until it is ready. Each serve
// gets its own dir under <root>/opencode-serve: `lens try` opens a runner without WorkerLock, so
// two serves can share a root, and neither may overwrite or delete the other's config.
func startPrivateServe(ctx context.Context, runtimeRoot string, allowFree bool) (*privateServe, error) {
	if runtimeRoot == "" {
		return nil, errors.New("opencode isolated runtime root is unavailable")
	}
	// Absolute, because the serve runs in os.TempDir() (openCodeCommand): a relative WITNESS_HOME
	// made its OPENCODE_CONFIG_DIR name a dir witness never wrote, so no agent ever loaded. After
	// the empty check: Abs("") is the cwd.
	runtimeRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return nil, fmt.Errorf("opencode runtime root: %w", err)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir opencode runtime: %w", err)
	}
	// ponytail: age-based sweep; a serve alive >7 days loses its dir. Record owner pids if that ever happens.
	sweepStaleServeDirs(runtimeRoot, 7*24*time.Hour)
	parent := filepath.Join(runtimeRoot, "opencode-serve")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir opencode serve dir: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "")
	if err != nil {
		return nil, fmt.Errorf("mkdir opencode serve dir: %w", err)
	}
	s := &privateServe{Dir: dir, Password: randomHex(24), logs: &safeBuffer{}, waitDone: make(chan struct{})}
	fail := func(err error) (*privateServe, error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	for _, sub := range []string{"config", "state"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return fail(fmt.Errorf("mkdir opencode serve dir: %w", err))
		}
	}
	if err := writePrivateConfig(s.ConfigPath(), nil); err != nil {
		return fail(err)
	}
	s.cmd = buildServeCmd(ctx, runtimeRoot, dir, s.Password, allowFree)
	url := make(chan string, 1)
	s.cmd.Stdout = &readyLine{ch: url}
	s.cmd.Stderr = s.logs
	if s.stdin, err = s.cmd.StdinPipe(); err != nil {
		return fail(fmt.Errorf("opencode serve stdin: %w", err))
	}
	// A cancelled ctx ends the lease (EOF) instead of exec.CommandContext's default SIGKILL, so
	// every shutdown takes the serve's own exit path; WaitDelay still kills one that ignores EOF.
	s.cmd.Cancel = s.stdin.Close
	if err := s.cmd.Start(); err != nil {
		return fail(fmt.Errorf("opencode serve start: %w", err))
	}
	go func() {
		s.waitErr = s.cmd.Wait()
		close(s.waitDone)
	}()
	if err := s.waitReady(ctx, url); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// buildServeCmd layers the serve's own variables over openCodeCommand's isolated env (no
// inherited OPENCODE_*, private DB and XDG dirs, WITNESS_WORKER=1). The config dir and state dir
// are per serve (dir); the DB stays per root. OPENCODE_DISABLE_MODELS_FETCH is deliberately
// absent: the serve refreshes the catalog every 5 min, and free model ids deprecate within days.
func buildServeCmd(ctx context.Context, runtimeRoot, dir, password string, allowFree bool) *exec.Cmd {
	cmd := openCodeCommand(ctx, runtimeRoot, "serve", "--stdio", "--port", "0", "--hostname", "127.0.0.1", "--log-level", "error")
	env := []string{
		"OPENCODE_PASSWORD=" + password,
		"OPENCODE_CONFIG_DIR=" + filepath.Join(dir, "config"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	}
	if !allowFree {
		env = append(env, "OPENCODE_DISABLE_FREE_MODELS=1")
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.WaitDelay = serveCloseGrace
	return cmd
}

// Close ends the lease: close stdin (EOF is the serve's only shutdown signal), wait up to
// serveCloseGrace, then kill. It always removes the serve's dir. The error is non-nil only on the
// kill path.
func (s *privateServe) Close() error {
	s.once.Do(func() {
		_ = s.stdin.Close()
		select {
		case <-s.waitDone:
		case <-time.After(serveCloseGrace):
			_ = s.cmd.Process.Kill()
			<-s.waitDone // WaitDelay bounds this even if a grandchild holds stdout
			s.closeErr = fmt.Errorf("opencode serve ignored stdin EOF; killed after %s", serveCloseGrace)
		}
		_ = os.RemoveAll(s.Dir)
	})
	return s.closeErr
}

// waitReady takes the serve's address line, then polls an authenticated GET /api/info until it
// answers 200 with a version. Not /global/health (the web UI, 200 to anyone) and not a pid match
// (behind an npm `.cmd` the pid is cmd.exe's; critic C2).
//
// Everything is bounded by healthCheckBudget, each probe included: the loop condition alone runs
// only BETWEEN probes, so one request that never returns made the old gate unbounded (blocked past
// 40 s against 30 s). This runs during Open, under the machine-wide WorkerLock, so a hang here
// stalls `distill start` and `doctor` and stops capture from draining, with no error anywhere.
func (s *privateServe) waitReady(ctx context.Context, url <-chan string) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckBudget)
	defer cancel()
	exited := func() error {
		return fmt.Errorf("opencode serve exited before it was ready: %v (logs: %s)", s.waitErr, s.logs.String())
	}
	var line string
	select {
	case line = <-url:
	case <-s.waitDone:
		return exited()
	case <-ctx.Done():
		return fmt.Errorf("opencode serve printed no address: %w (logs: %s)", ctx.Err(), s.logs.String())
	}
	var ready struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(line), &ready) != nil || !strings.HasPrefix(ready.URL, "http://127.0.0.1:") {
		return fmt.Errorf("opencode serve printed %.200q instead of its address; witness needs OpenCode v2.2.7 or newer", line)
	}
	s.URL = ready.URL
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/api/info", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", s.AuthHeader())
		if resp, err := http.DefaultClient.Do(req); err != nil {
			// A probe the budget cancelled mid-flight fails with the ctx error, which the timeout
			// message names anyway; keep the last real answer (a 401, say) instead.
			if ctx.Err() == nil || lastErr == nil {
				lastErr = err
			}
		} else {
			var info struct {
				Version string `json:"version"`
			}
			decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode != http.StatusOK:
				lastErr = fmt.Errorf("GET /api/info: %s", resp.Status)
			case decErr != nil || info.Version == "":
				lastErr = fmt.Errorf("GET /api/info named no version")
			default:
				return nil
			}
		}
		select {
		case <-s.waitDone:
			return exited()
		case <-ctx.Done():
			return fmt.Errorf("opencode serve was not ready: %w (last: %v; logs: %s)", ctx.Err(), lastErr, s.logs.String())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// readyLine is the serve's stdout. It hands the first line (the address) to ch once and discards
// the rest, so the pipe keeps draining and a later serve write can never block or hit EPIPE — what
// OpenCode's own client does (windows.md §C2). ch has room for one value, so Write never blocks.
// exec copies stdout from one goroutine, so no lock.
type readyLine struct {
	ch   chan<- string
	buf  []byte
	sent bool
}

func (r *readyLine) Write(p []byte) (int, error) {
	if !r.sent {
		r.buf = append(r.buf, p...)
		if i := bytes.IndexByte(r.buf, '\n'); i >= 0 || len(r.buf) > 64<<10 {
			if i < 0 {
				i = len(r.buf)
			}
			r.ch <- strings.TrimSpace(string(r.buf[:i]))
			r.sent, r.buf = true, nil
		}
	}
	return len(p), nil
}

// sweepStaleServeDirs removes serve dirs left by a witness that was hard-killed before Close (the
// lease ended its serve, but nothing removed the dir). Best-effort.
func sweepStaleServeDirs(runtimeRoot string, olderThan time.Duration) {
	parent := filepath.Join(runtimeRoot, "opencode-serve")
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
}

// privateConfigOverlay is merged into every private config, under witness's own keys. It is nil in
// production; PR 3's contract job sets it to add a mock provider. Nothing from the user's
// opencode.json ever goes here (spec 7.6).
var privateConfigOverlay map[string]any

// denyAll is witness's whole tool policy. It goes on the agent AND on every session: OpenCode's
// bundled browser plugin appends {skill, browser, deny} to every agent after config, and
// Tool.snapshot keys on the last rule per action, so the agent's deny alone left `skill` offered.
// Session rules merge after the agent's (opencode session/context.ts:146).
var denyAll = []map[string]string{{"action": "*", "resource": "*", "effect": "deny"}}

// writePrivateConfig writes the serve's whole config: one deny-all agent per id (prompts never
// travel in env vars, which Windows caps at 32,767 chars). Nothing is copied from the user's
// opencode.json (spec 7.6). Atomic, because a running serve re-reads it when agents are added.
func writePrivateConfig(path string, agents map[string]string) error {
	defs := map[string]any{}
	for id, prompt := range agents {
		defs[id] = map[string]any{
			"description": "witness distillation (no tools)",
			"system":      prompt,
			"permissions": denyAll,
		}
	}
	cfg := map[string]any{}
	maps.Copy(cfg, privateConfigOverlay)
	cfg["$schema"], cfg["agents"] = "https://opencode.ai/config.json", defs
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(path, b); err != nil {
		return fmt.Errorf("write opencode private config: %w", err)
	}
	return nil
}

// allowsFree reports whether a configured model is a free `opencode/*` one. Otherwise the serve
// gets OPENCODE_DISABLE_FREE_MODELS=1, so it never falls back to a free model witness did not name.
func allowsFree(models []string) bool { return slices.ContainsFunc(models, isFreeModel) }
