package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/model"
)

// modelMirror points the mirror env vars at h, with hashes for the two stand-in bodies "m" and
// "t", so no test downloads the real model. It returns the request counter.
func modelMirror(t *testing.T, h http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	sum := func(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
	t.Setenv("WITNESS_MODEL_BASE_URL", srv.URL)
	t.Setenv("WITNESS_MODEL_SHA256", sum("m"))
	t.Setenv("WITNESS_TOKENIZER_SHA256", sum("t"))
	return &hits
}

func serveStandIns(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/onnx/model.onnx":
		_, _ = w.Write([]byte("m"))
	case "/tokenizer.json":
		_, _ = w.Write([]byte("t"))
	default:
		http.NotFound(w, r)
	}
}

func TestModelFetchDownloadsIntoTheAssetsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WITNESS_ASSETS", dir)
	modelMirror(t, serveStandIns)
	out, err := runCLI(t, "model", "fetch")
	if err != nil {
		t.Fatalf("model fetch: %v\n%s", err, out)
	}
	for name, want := range map[string]string{"model.onnx": "m", "tokenizer.json": "t"} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(out, "embedding model ready") {
		t.Errorf("no result line:\n%s", out)
	}
}

func TestModelFetchNamesABusyLockAndHowToRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WITNESS_ASSETS", dir)
	modelMirror(t, serveStandIns)
	host, _ := os.Hostname()
	if err := os.WriteFile(filepath.Join(dir, ".download.lock"), fmt.Appendf(nil, "tok %d %s\n", os.Getpid(), host), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "model", "fetch")
	if err == nil || !strings.Contains(err.Error(), "another process") || !strings.Contains(err.Error(), "witness model fetch") {
		t.Fatalf("err = %v, want the busy lock named with the command to retry", err)
	}
}

// The lazy start is for `npx … mcp` alone, and never when the model is there: everywhere else
// `witness mcp` must behave exactly as it did.
func TestMCPStartsWithoutTheModelOnlyUnderNpx(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WITNESS_ASSETS", dir)
	t.Setenv("WITNESS_NPM_PACKAGE", "")
	if lazyModel() {
		t.Error("outside the npm launcher a missing model must still stop witness mcp, as before")
	}
	t.Setenv("WITNESS_NPM_PACKAGE", "1")
	if !lazyModel() {
		t.Error("under npx a missing model must not stop witness mcp")
	}
	// Sparse files past embed.ModelReady's size floors: a present model.
	for name, size := range map[string]int64{"model.onnx": 400_000_000, "tokenizer.json": 1_000_000} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if lazyModel() {
		t.Error("with the model present, witness mcp must load it exactly as before")
	}
}

func TestLazyEmbedderSaysDownloadingThenWhyItFailed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WITNESS_ASSETS", dir)
	t.Setenv(model.SkipEnv, "")
	release := make(chan struct{})
	modelMirror(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		http.Error(w, "gone", http.StatusServiceUnavailable)
	})
	l := startLazyEmbedder()
	if _, err := l.Embed("q"); err == nil || !strings.Contains(err.Error(), "still downloading") {
		t.Fatalf("mid-download err = %v, want \"still downloading\"", err)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.mu.Lock()
		failed := l.err != nil
		l.mu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background fetch never reported its failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := l.Embed("q")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "model fetch") {
		t.Fatalf("after a failed fetch err = %v, want the cause and the command to fix it", err)
	}
}

func TestLazyEmbedderHonoursSkipDownload(t *testing.T) {
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	t.Setenv(model.SkipEnv, "1")
	hits := modelMirror(t, serveStandIns)
	l := startLazyEmbedder()
	if _, err := l.Embed("q"); err == nil || !strings.Contains(err.Error(), model.SkipEnv+"=1") {
		t.Fatalf("err = %v, want the skip switch named", err)
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != 0 {
		t.Errorf("%s=1 still made %d download requests", model.SkipEnv, hits.Load())
	}
}

// The lazy branch must be the one cmdMCP takes: under npx with no model, the server starts (and
// here ends at stdin EOF) instead of failing on the missing model, as it did before.
func TestMCPServesUnderNpxWithoutTheModel(t *testing.T) {
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	t.Setenv("WITNESS_NPM_PACKAGE", "1")
	t.Setenv(model.SkipEnv, "1") // no download from this test
	prevLog := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close() // EOF at once: the client hung up
	prev := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = prev; _ = r.Close() })
	if err := cmdMCP(); err != nil && strings.Contains(err.Error(), "embed model") {
		t.Fatalf("witness mcp under npx still refused to start without the model: %v", err)
	}
}
