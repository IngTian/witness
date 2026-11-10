package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sum(b string) string { s := sha256.Sum256([]byte(b)); return hex.EncodeToString(s[:]) }

// mirror serves tiny stand-ins for the two files and points the mirror env vars at them, so
// no test touches the network or the real 470MB model. It returns the request counter.
func mirror(t *testing.T, model, tok string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/onnx/model.onnx":
			_, _ = w.Write([]byte(model))
		case "/tokenizer.json":
			_, _ = w.Write([]byte(tok))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WITNESS_MODEL_BASE_URL", srv.URL+"/")
	t.Setenv("WITNESS_MODEL_SHA256", sum(model))
	t.Setenv("WITNESS_TOKENIZER_SHA256", strings.ToUpper(sum(tok))) // case-insensitive, as model.js
	return &hits
}

func noLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") || e.Name() == lockName {
			t.Errorf("left behind %s", e.Name())
		}
	}
}

func TestFetchDownloadsVerifiesAndReleasesTheLock(t *testing.T) {
	mirror(t, "fake-model", "fake-tokenizer")
	dir := filepath.Join(t.TempDir(), "assets", "e5-small") // does not exist yet
	var last int64
	if err := Fetch(context.Background(), dir, func(name string, done, _ int64) {
		if name == "model.onnx" {
			last = done
		}
	}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"model.onnx": "fake-model", "tokenizer.json": "fake-tokenizer"} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if last != int64(len("fake-model")) {
		t.Errorf("progress ended at %d bytes, want %d", last, len("fake-model"))
	}
	noLeftovers(t, dir)
}

func TestFetchHashesFilesAlreadyRightInsteadOfDownloading(t *testing.T) {
	hits := mirror(t, "fake-model", "fake-tokenizer")
	dir := t.TempDir()
	if err := Fetch(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if err := Fetch(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before {
		t.Errorf("a second fetch made %d requests; files already right must not be downloaded", hits.Load()-before)
	}
}

func TestFetchRejectsABadHashAndRemovesTheWrongFile(t *testing.T) {
	mirror(t, "fake-model", "fake-tokenizer")
	t.Setenv("WITNESS_TOKENIZER_SHA256", sum("something else"))
	dir := t.TempDir()
	// A wrong tokenizer already in place (a truncated or swapped file) must not survive.
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Fetch(context.Background(), dir, nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want a sha256 mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tokenizer.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a tokenizer.json that fails its hash is still in place (%v); embed.ModelReady checks size only", err)
	}
	noLeftovers(t, dir)
}

func TestAMirrorNeedsBothHashes(t *testing.T) {
	t.Setenv("WITNESS_MODEL_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("WITNESS_MODEL_SHA256", sum("x"))
	t.Setenv("WITNESS_TOKENIZER_SHA256", "")
	if err := Fetch(context.Background(), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "WITNESS_TOKENIZER_SHA256") {
		t.Fatalf("err = %v, want the missing hash named", err)
	}
}

func TestFetchFailsOnAStalledDownload(t *testing.T) {
	old := stallTimeout
	stallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stallTimeout = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then nothing, until the client gives up
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WITNESS_MODEL_BASE_URL", srv.URL)
	t.Setenv("WITNESS_MODEL_SHA256", sum("x"))
	t.Setenv("WITNESS_TOKENIZER_SHA256", sum("y"))
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- Fetch(context.Background(), dir, nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, errStalled) {
			t.Fatalf("err = %v, want errStalled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled download hung instead of failing")
	}
	noLeftovers(t, dir)
}

// The stall timer measures silence, not the whole download: a slow but steady transfer of the
// real 470MB file takes far longer than stallTimeout and must still finish.
func TestFetchSurvivesASlowSteadyDownload(t *testing.T) {
	old := stallTimeout
	stallTimeout = 80 * time.Millisecond
	t.Cleanup(func() { stallTimeout = old })
	body := strings.Repeat("x", 12)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/onnx/model.onnx" {
			_, _ = w.Write([]byte("t"))
			return
		}
		for i := range body { // ~360ms in all, never silent for long
			_, _ = w.Write([]byte{body[i]})
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WITNESS_MODEL_BASE_URL", srv.URL)
	t.Setenv("WITNESS_MODEL_SHA256", sum(body))
	t.Setenv("WITNESS_TOKENIZER_SHA256", sum("t"))
	if err := Fetch(context.Background(), t.TempDir(), nil); err != nil {
		t.Fatalf("a download that kept receiving bytes was cut off: %v", err)
	}
}

// The lock is shared with model.js, so its body and its stale rules must be model.js's.
func TestLockFollowsModelJS(t *testing.T) {
	host, _ := os.Hostname()
	write := func(t *testing.T, dir, body string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, lockName)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		body string
		age  time.Duration
		busy bool
	}{
		{"live owner on this host", fmt.Sprintf("tok %d %s\n", os.Getppid(), host), 0, true},
		{"our own pid: a dead predecessor's, after pid reuse", fmt.Sprintf("tok %d %s\n", os.Getpid(), host), 0, false},
		{"pid 1 is left to the 12h rule, as model.js leaves it", fmt.Sprintf("tok 1 %s\n", host), 0, true},
		{"dead owner on this host", fmt.Sprintf("tok %d %s\n", 0x7FFFFFF0, host), 0, false},
		{"fresh lock from another host", fmt.Sprintf("tok %d other-host\n", os.Getpid()), 0, true},
		{"12h-old lock from another host", fmt.Sprintf("tok %d other-host\n", os.Getpid()), 13 * time.Hour, false},
		{"unreadable body, fresh", "garbage", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, c.body, c.age)
			release, err := lock(dir)
			if c.busy {
				if !errors.Is(err, ErrBusy) {
					t.Fatalf("err = %v, want ErrBusy", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a stale lock was not taken over: %v", err)
			}
			body, _ := os.ReadFile(filepath.Join(dir, lockName))
			if f := strings.Fields(string(body)); len(f) != 3 || f[1] != fmt.Sprint(os.Getpid()) || f[2] != host {
				t.Errorf("lock body %q, want \"<token> %d %s\" (model.js's format)", body, os.Getpid(), host)
			}
			release()
			if _, err := os.Stat(filepath.Join(dir, lockName)); !errors.Is(err, os.ErrNotExist) {
				t.Error("release left the lock behind")
			}
		})
	}
	t.Run("release leaves a lock that is no longer ours", func(t *testing.T) {
		dir := t.TempDir()
		release, err := lock(dir)
		if err != nil {
			t.Fatal(err)
		}
		write(t, dir, "someone-else 1 h\n", 0) // reaped and handed on while we ran
		release()
		if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
			t.Error("release deleted another downloader's lock")
		}
	})
}

func TestReapPartsKeepsOnlyOurs(t *testing.T) {
	dir := t.TempDir()
	mine := fmt.Sprintf("model.onnx.%d.part", os.Getpid())
	for _, n := range []string{mine, "model.onnx.1.part", "tokenizer.json.part", "model.onnx"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reapParts(dir)
	for n, want := range map[string]bool{mine: true, "model.onnx.1.part": false, "tokenizer.json.part": false, "model.onnx": true} {
		_, err := os.Stat(filepath.Join(dir, n))
		if (err == nil) != want {
			t.Errorf("%s present=%v, want %v", n, err == nil, want)
		}
	}
}

// model.js still downloads for the OpenCode plugin (#218 folds the two). Both must fetch the
// same bytes, or the plugin and npx would each call the other's model corrupt.
func TestPinsMatchModelJS(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "npm", "opencode", "bin", "model.js"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"MODEL_REVISION":   Revision,
		"MODEL_SHA256":     ModelSHA256,
		"TOKENIZER_SHA256": TokenizerSHA256,
	} {
		m := regexp.MustCompile(`const ` + name + ` = "([0-9a-f]+)"`).FindSubmatch(src)
		if m == nil || string(m[1]) != want {
			t.Errorf("model.js %s = %q, Go has %q", name, m, want)
		}
	}
	if !strings.Contains(string(src), `"https://huggingface.co/intfloat/multilingual-e5-small/resolve/" + MODEL_REVISION`) {
		t.Error("model.js's DEFAULT_BASE_URL no longer matches Go's defaultBaseURL")
	}
}
