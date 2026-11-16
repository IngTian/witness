//go:build opencodecontract

package opencode

// Two ways to hold a real serve:
//   - startRawServe spawns `serve --stdio` itself, outside witness's spawn policy: the raw lease,
//     and the npm `.cmd` shim that production resolves away (T3);
//   - startHolder runs the test binary as a `hold` stub that starts witness's OWN private serve
//     (startPrivateServe → buildServeCmd: HideWindow, WaitDelay, Cancel = stdin.Close, the isolated
//     env), so killing the holder is exactly a worker dying with its serve (T1, T4, T8, T9). Its
//     extraEnv selects two variants: WITNESS_CONTRACT_HOLD_RAW=1 holds a raw `bin serve --stdio`
//     instead (T3's `.cmd` chain, which witness's own spawn resolves away), and
//     WITNESS_CONTRACT_HOLD_OVERLAY=<json> becomes privateConfigOverlay (T9's MCP server, since
//     the private serve drops OPENCODE_CONFIG_CONTENT).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/proc"
)

type rawServe struct {
	Cmd      *exec.Cmd // nil for a holderInfo.Client
	Stdin    io.WriteCloser
	URL      string
	Password string
	Work     string

	w *serveWait // a pointer, so a copy (T10's wrong-password client) stays vet-clean and shares it
}

// serveWait is the serve's one cmd.Wait, run from Start, so readiness can see an early exit and
// Close can be called any number of times.
type serveWait struct {
	done   chan struct{} // closed once Wait returned, err set first
	err    error
	stderr safeBuffer
}

// startRawServe starts `bin serve --stdio` in the scratch env and waits until it answers an
// authenticated /api/info (30 s). No WaitDelay, so T2 measures the lease alone.
func startRawServe(t *testing.T, bin string, s scratch, extraEnv ...string) *rawServe {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), contractProcessBound)
	srv := &rawServe{Password: randomHex(16), Work: s.Work, w: &serveWait{done: make(chan struct{})}}
	srv.Cmd = exec.CommandContext(ctx, bin, "serve", "--stdio", "--port", "0", "--hostname", "127.0.0.1", "--log-level", "error")
	srv.Cmd.Env = s.procEnv(append([]string{"OPENCODE_PASSWORD=" + srv.Password}, extraEnv...))
	srv.Cmd.Dir = s.Work
	url := make(chan string, 1)
	srv.Cmd.Stdout = &readyLine{ch: url} // the address line, then drained
	srv.Cmd.Stderr = &srv.w.stderr
	var err error
	if srv.Stdin, err = srv.Cmd.StdinPipe(); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := srv.Cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s serve: %v", bin, err)
	}
	go func() {
		srv.w.err = srv.Cmd.Wait()
		cancel()
		close(srv.w.done)
	}()
	t.Cleanup(func() { _ = srv.Close() }) // its error is the test's to check; a test may have killed it

	// One 30 s budget for the address AND every /api/info probe, each probe bounded by what is left.
	ready, readyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer readyCancel()
	select {
	case line := <-url:
		var ready struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &ready) != nil || ready.URL == "" {
			t.Fatalf("serve printed %.200q, want {\"url\":…}", line)
		}
		srv.URL = ready.URL
	case <-srv.w.done:
		t.Fatalf("serve exited before printing its address: %v (stderr: %s)", srv.w.err, srv.w.stderr.String())
	case <-ready.Done():
		t.Fatalf("serve printed no address in 30 s (stderr: %s)", srv.w.stderr.String())
	}
	for {
		code, body := srv.do(ready, http.MethodGet, "/api/info", nil, true)
		if code == http.StatusOK {
			return srv
		}
		select {
		case <-srv.w.done:
			t.Fatalf("serve exited before it was ready: %v (stderr: %s)", srv.w.err, srv.w.stderr.String())
		case <-ready.Done():
			t.Fatalf("serve not ready in 30 s: GET /api/info = %d %.300s (stderr: %s)", code, body, srv.w.stderr.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Do sends one request (basic auth user `opencode` when auth), bounded at 30 s. A transport error is
// status 0 with the error as the body, so the caller's status check reports it.
func (s *rawServe) Do(method, path string, body any, auth bool) (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.do(ctx, method, path, body, auth)
}

func (s *rawServe) do(ctx context.Context, method, path string, body any, auth bool) (int, []byte) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, []byte(err.Error())
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, r)
	if err != nil {
		return 0, []byte(err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.SetBasicAuth("opencode", s.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b
}

// Prompt creates a session on mock/<model> located in Work and prompts it; it returns the
// session id at once (the prompt route does not wait for the turn).
func (s *rawServe) Prompt(t *testing.T, model, text string) string {
	t.Helper()
	code, b := s.Do(http.MethodPost, "/api/session", map[string]any{"title": "contract",
		"location": map[string]string{"directory": s.Work}, "model": map[string]string{"providerID": "mock", "id": model}}, true)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal(b, &created) != nil || created.Data.ID == "" {
		t.Fatalf("POST /api/session: %d %.500s", code, b)
	}
	if code, b := s.Do(http.MethodPost, "/api/session/"+created.Data.ID+"/prompt", map[string]string{"text": text}, true); code != http.StatusOK {
		t.Fatalf("POST /api/session/%s/prompt: %d %.500s", created.Data.ID, code, b)
	}
	return created.Data.ID
}

// Close ends the lease: close stdin, wait up to 5 s, else kill. It returns the serve's exit error
// (nil for rc=0), or the kill. A nil Cmd (a holder's client) has nothing to close.
func (s *rawServe) Close() error {
	if s.Cmd == nil {
		return nil
	}
	_ = s.Stdin.Close()
	select {
	case <-s.w.done:
		return s.w.err
	case <-time.After(5 * time.Second):
		_ = s.Cmd.Process.Kill()
		return fmt.Errorf("serve ignored stdin EOF for 5 s; killed (stderr: %s)", s.w.stderr.String())
	}
}

type holderInfo struct {
	ServePID   int    `json:"serve_pid"`
	SiblingPID int    `json:"sibling_pid"`
	URL        string `json:"url"`
	Password   string `json:"password"`
}

// Client is a rawServe for Do against the holder's serve.
func (h holderInfo) Client(s scratch) *rawServe {
	return &rawServe{URL: h.URL, Password: h.Password, Work: s.Work}
}

// startHolder starts a `hold` stub that holds witness's private serve of bin, contained in a
// kill-on-close job first when contain (Windows; a no-op elsewhere), plus a sleeping sibling child
// when sibling. extraEnv goes to the stub (the variants above). The test kills holder; Cleanup
// kills whatever is left.
func startHolder(t *testing.T, bin string, s scratch, contain, sibling bool, extraEnv ...string) (holder *exec.Cmd, h holderInfo) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), contractProcessBound)
	holder = exec.CommandContext(ctx, exe, "-test.run=^$") // if the stub ever misfires, it runs no test
	env := append([]string{"WITNESS_CONTRACT_STUB=hold", "WITNESS_CONTRACT_HOLD_BIN=" + bin}, extraEnv...)
	if contain {
		env = append(env, "WITNESS_CONTRACT_HOLD_CONTAIN=1")
	}
	if sibling {
		env = append(env, "WITNESS_CONTRACT_HOLD_SIBLING=1")
	}
	holder.Env, holder.Dir = s.procEnv(env), s.Work
	line := make(chan string, 1)
	var stderr safeBuffer
	holder.Stdout, holder.Stderr = &readyLine{ch: line}, &stderr
	if err := holder.Start(); err != nil {
		cancel()
		t.Fatalf("start holder: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = holder.Wait()
		cancel()
		close(done)
	}()
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("holder %d still running 5 s after a kill", holder.Process.Pid)
		}
		kill := func(pid int) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
		if h.SiblingPID > 0 && proc.Alive(h.SiblingPID) {
			kill(h.SiblingPID)
		}
		// The serve follows its holder on the lease (T1's 2 s). One still alive after that is a lease
		// failure, reported here because killing it hides it from noLeaks.
		if h.ServePID > 0 && !pidGone(h.ServePID, 2*time.Second) {
			kill(h.ServePID)
			t.Errorf("serve %d outlived its killed holder by 2 s; killed", h.ServePID)
		}
	})
	select {
	case l := <-line:
		if json.Unmarshal([]byte(l), &h) != nil || h.ServePID == 0 || h.URL == "" {
			t.Fatalf("holder printed %.300q (stderr: %s)", l, stderr.String())
		}
	case <-done:
		t.Fatalf("holder exited before reporting its serve (stderr: %s)", stderr.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("holder reported no serve in 60 s (stderr: %s)", stderr.String())
	}
	return holder, h
}

// holdStub is WITNESS_CONTRACT_STUB=hold: start witness's private serve of $WITNESS_CONTRACT_HOLD_BIN
// under the scratch WITNESS_HOME's runtime root (as the worker does), or a raw serve under
// WITNESS_CONTRACT_HOLD_RAW=1, print holderInfo as one JSON line, then hold the lease until killed
// or contractProcessBound.
func holdStub() {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "witness contract hold stub:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), contractProcessBound)
	defer cancel()
	if os.Getenv("WITNESS_CONTRACT_HOLD_CONTAIN") == "1" {
		if err := proc.System().ContainChildren(); err != nil {
			fail(err)
		}
	}
	if js := os.Getenv("WITNESS_CONTRACT_HOLD_OVERLAY"); js != "" {
		if err := json.Unmarshal([]byte(js), &privateConfigOverlay); err != nil {
			fail(fmt.Errorf("WITNESS_CONTRACT_HOLD_OVERLAY: %w", err))
		}
	}
	bin := os.Getenv("WITNESS_CONTRACT_HOLD_BIN")
	var serve *privateServe
	if os.Getenv("WITNESS_CONTRACT_HOLD_RAW") == "1" {
		// The pre-G16 raw spawn: bin as given (no ResolveExe, so a `.cmd` stays a `.cmd` and
		// serve_pid is cmd.exe's), in this stub's env, held on its StdinPipe. waitReady (the same
		// /api/info gate) reads only Password, logs and the wait fields.
		serve = &privateServe{Password: randomHex(24), logs: &safeBuffer{}, waitDone: make(chan struct{})}
		serve.cmd = exec.CommandContext(ctx, bin, "serve", "--stdio", "--port", "0", "--hostname", "127.0.0.1", "--log-level", "error")
		serve.cmd.Env = append(os.Environ(), "OPENCODE_PASSWORD="+serve.Password)
		url := make(chan string, 1)
		serve.cmd.Stdout, serve.cmd.Stderr = &readyLine{ch: url}, serve.logs
		var err error
		if serve.stdin, err = serve.cmd.StdinPipe(); err != nil {
			fail(err)
		}
		if err := serve.cmd.Start(); err != nil {
			fail(err)
		}
		go func() { // also keeps serve, and so the lease's write end, reachable while we hold
			serve.waitErr = serve.cmd.Wait()
			close(serve.waitDone)
		}()
		if err := serve.waitReady(ctx, url); err != nil {
			fail(err)
		}
	} else {
		if err := os.Setenv(openCodeBinEnv, bin); err != nil { // pluginExe picks it
			fail(err)
		}
		var err error
		if serve, err = startPrivateServe(ctx, filepath.Join(os.Getenv("WITNESS_HOME"), "runtime"), false); err != nil {
			fail(err)
		}
	}
	h := holderInfo{ServePID: serve.cmd.Process.Pid, URL: serve.URL, Password: serve.Password}
	if os.Getenv("WITNESS_CONTRACT_HOLD_SIBLING") == "1" {
		// Started AFTER the serve's stdin pipe exists: if the write end leaked into children, this
		// sleeper would keep the serve alive past the holder (T4).
		exe, err := os.Executable()
		if err != nil {
			fail(err)
		}
		sib := exec.CommandContext(ctx, exe, "-test.run=^$")
		sib.Env = append(os.Environ(), "WITNESS_CONTRACT_STUB=sleep")
		if err := sib.Start(); err != nil {
			fail(err)
		}
		h.SiblingPID = sib.Process.Pid
	}
	b, _ := json.Marshal(h)
	fmt.Println(string(b))
	<-ctx.Done()
	os.Exit(0)
}
