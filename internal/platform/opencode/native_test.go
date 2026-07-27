package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
)

func TestNativeRunUsesPrivateImportAndDistinctForks(t *testing.T) {
	root, source := t.TempDir(), filepath.Join(t.TempDir(), "opencode.db")
	t.Setenv("WITNESS_OPENCODE_DB", source)
	var commands [][]string
	oldCommand := nativeCommand
	nativeCommand = func(_ context.Context, env []string, args ...string) ([]byte, error) {
		commands = append(commands, append(append([]string{}, env...), "--", strings.Join(args, " ")))
		return []byte(`{"info":{},"messages":[]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()
	oldPoll := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = oldPoll }()
	var forks, prompts, forkDeletes int
	prompted := map[string]bool{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session/source/fork":
			forks++
			_, _ = w.Write([]byte(`{"id":"fork_` + string(rune('0'+forks)) + `"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/message"):
			if prompted[strings.Split(r.URL.Path, "/")[2]] {
				manifests, _ := os.ReadDir(filepath.Join(root, "opencode-native"))
				messageID := ""
				for _, entry := range manifests {
					if strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".snapshot.json") {
						b, _ := os.ReadFile(filepath.Join(root, "opencode-native", entry.Name()))
						var manifest nativeManifest
						_ = json.Unmarshal(b, &manifest)
						if manifest.Fork == strings.Split(r.URL.Path, "/")[2] {
							messageID = manifest.MessageID
						}
					}
				}
				_, _ = fmt.Fprintf(w, `[
					{"info":{"id":"old","role":"assistant"},"parts":[{"type":"text","text":"historical"}]},
					{"info":{"id":%q,"role":"user"},"parts":[{"type":"text","text":"request"}]},
					{"info":{"id":"reply","role":"assistant"},"parts":[{"type":"text","text":"[]"}]}
				]`, messageID)
			} else {
				_, _ = w.Write([]byte(`[{"info":{"id":"old","role":"assistant"},"parts":[{"type":"text","text":"historical"}]}]`))
			}
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/prompt_async"):
			prompts++
			prompted[strings.Split(r.URL.Path, "/")[2]] = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			if strings.HasPrefix(r.URL.Path, "/session/fork_") {
				forkDeletes++
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	n := newNativeRuntime(root, &OpenCodeServer{baseURL: ts.URL, client: ts.Client()})
	jobs := []struct{ lens, input string }{{"a", "0:same"}, {"a", "1:same"}, {"b", "0:same"}}
	for _, job := range jobs {
		w := &platform.NativeSession{Session: "opencode:source", RawHigh: 7, Total: 2, Lens: job.lens, Input: job.input}
		if _, err := n.run(context.Background(), w, "", "P", "I"); err != nil {
			t.Fatal(err)
		}
		if forkDeletes != 0 {
			t.Fatal("fork deleted before finalization")
		}
	}
	if forks != 3 || prompts != 3 {
		t.Fatalf("forks=%d prompts=%d", forks, prompts)
	}
	beforeCommands, beforePrompts := len(commands), prompts
	n2 := newNativeRuntime(root, n.server)
	if _, err := n2.run(context.Background(), &platform.NativeSession{Session: "opencode:source", RawHigh: 7, Total: 2, Lens: "a", Input: "0:same"}, "", "P", "I"); err != nil {
		t.Fatal(err)
	}
	if len(commands) != beforeCommands || prompts != beforePrompts {
		t.Fatalf("completed reply was not reused: commands=%d prompts=%d", len(commands), prompts)
	}
	if len(commands) != 6 {
		t.Fatalf("commands=%d, want export/import per lens and chunk", len(commands))
	}
	for _, c := range commands {
		joined := strings.Join(c, "\n")
		if strings.Contains(joined, "export --pure") && !strings.Contains(joined, "OPENCODE_DB="+source) {
			t.Fatalf("export did not target source DB: %s", joined)
		}
		if strings.Contains(joined, "import --pure") && (!strings.Contains(joined, "XDG_DATA_HOME="+filepath.Join(root, "xdg")) || !strings.Contains(joined, "OPENCODE_DB="+filepath.Join(root, "opencode.db"))) {
			t.Fatalf("import not isolated: %s", joined)
		}
	}
}

func TestExportedTranscriptDigestMatchesCapturedL0(t *testing.T) {
	data := []byte(`{
		"messages":[
			{"info":{"role":"user"},"parts":[{"type":"text","text":"hello"},{"type":"tool","text":"ignored"}]},
			{"info":{"role":"assistant","time":{"completed":2}},"parts":[{"type":"text","text":"answer"}]},
			{"info":{"role":"assistant","time":{}},"parts":[{"type":"text","text":"incomplete"}]}
		]
	}`)
	got, err := exportedTranscriptDigest(data)
	if err != nil {
		t.Fatal(err)
	}
	want := platform.TranscriptDigest([]platform.TranscriptEntry{{Role: "user", Text: "hello"}, {Role: "assistant", Text: "answer"}})
	if got != want {
		t.Fatalf("digest=%s, want %s", got, want)
	}
}

func TestNativeRunRejectsExportNewerThanCapturedL0(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(t.TempDir(), "source.db"))
	oldCommand := nativeCommand
	nativeCommand = func(context.Context, []string, ...string) ([]byte, error) {
		return []byte(`{"messages":[{"info":{"role":"user"},"parts":[{"type":"text","text":"new text"}]}]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()
	w := &platform.NativeSession{
		Session: "opencode:source", RawHigh: 7, Total: 1, Lens: "lens", Input: "0:chunk",
		Digest: platform.TranscriptDigest([]platform.TranscriptEntry{{Role: "user", Text: "captured text"}}),
	}
	if _, err := newNativeRuntime(root, nil).run(context.Background(), w, "", "P", "I"); err == nil || !strings.Contains(err.Error(), "changed after L0 capture") {
		t.Fatalf("snapshot drift error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "opencode.db")); !os.IsNotExist(err) {
		t.Fatalf("snapshot drift reached isolated DB: %v", err)
	}
}

func TestNativeRunsGenerateConcurrentlyAfterIsolatedForkSetup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(t.TempDir(), "source.db"))
	oldCommand := nativeCommand
	nativeCommand = func(context.Context, []string, ...string) ([]byte, error) {
		return []byte(`{"info":{},"messages":[]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()
	oldPoll := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = oldPoll }()

	var forks, inFlight, peak atomic.Int32
	var replies sync.Map
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork"):
			id := forks.Add(1)
			_, _ = fmt.Fprintf(w, `{"id":"fork_%d"}`, id)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async"):
			var body struct {
				MessageID string `json:"messageID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			current := inFlight.Add(1)
			for {
				old := peak.Load()
				if current <= old || peak.CompareAndSwap(old, current) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
			replies.Store(strings.Split(r.URL.Path, "/")[2], body.MessageID)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/message"):
			fork := strings.Split(r.URL.Path, "/")[2]
			messageID, ok := replies.Load(fork)
			if !ok {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = fmt.Fprintf(w, `[
				{"info":{"id":%q,"role":"user"},"parts":[{"type":"text","text":"request"}]},
				{"info":{"id":"reply","role":"assistant"},"parts":[{"type":"text","text":"[]"}]}
			]`, messageID)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer ts.Close()
	n := newNativeRuntime(root, &OpenCodeServer{baseURL: ts.URL, client: ts.Client()})

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := n.run(context.Background(), &platform.NativeSession{
				Session: "opencode:source", RawHigh: 7, Total: 2, Lens: fmt.Sprintf("lens-%d", i), Input: "0:chunk",
			}, "", "P", "I")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if forks.Load() != 3 {
		t.Fatalf("forks=%d, want 3", forks.Load())
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrent native generations=%d, want at least 2", peak.Load())
	}
}

func TestNativePrepareAuthCopiesPrivately(t *testing.T) {
	root, user := t.TempDir(), t.TempDir()
	db := filepath.Join(user, "opencode.db")
	auth := filepath.Join(user, "auth.json")
	t.Setenv("WITNESS_OPENCODE_DB", db)
	if err := os.WriteFile(auth, []byte(`{"token":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	n := newNativeRuntime(root, nil)
	if err := n.prepareAuth(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "xdg", "opencode", "auth.json")
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != `{"token":"secret"}` {
		t.Fatalf("auth copy: %q %v", b, err)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode=%v err=%v", info.Mode(), err)
	}
	if info, err := os.Stat(auth); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("source auth changed: %v %v", info, err)
	}
}

func TestNativeMalformedManifestIsRetainedError(t *testing.T) {
	n := newNativeRuntime(t.TempDir(), nil)
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(n.dir(), "bad.json")
	if err := os.WriteFile(p, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.load(p); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("load error=%v", err)
	}
}

func TestNativeReconcileOnlyCleansCommittedCurrentGeneration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(root), "witness.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE raw(id INTEGER PRIMARY KEY, session TEXT); CREATE TABLE progress(session TEXT,lens TEXT,distilled INTEGER); INSERT INTO raw VALUES(9,'opencode:s'); INSERT INTO progress VALUES('opencode:s','ok',2)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var deletes int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Fatalf("unexpected %s", r.Method)
	}))
	defer ts.Close()
	n := newNativeRuntime(root, &OpenCodeServer{baseURL: ts.URL, client: ts.Client()})
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	current := nativeManifest{Session: "opencode:s", Lens: "ok", RawHigh: 9, Total: 2, Fork: "f1"}
	stale := nativeManifest{Session: "opencode:s", Lens: "stale", RawHigh: 8, Total: 2, Fork: "f2"}
	if err := n.save(filepath.Join(n.dir(), "current.json"), current); err != nil {
		t.Fatal(err)
	}
	if err := n.save(filepath.Join(n.dir(), "stale.json"), stale); err != nil {
		t.Fatal(err)
	}
	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("deletes=%d", deletes)
	}
	if _, err := os.Stat(filepath.Join(n.dir(), "current.json")); !os.IsNotExist(err) {
		t.Fatal("current manifest retained")
	}
	if _, err := os.Stat(filepath.Join(n.dir(), "stale.json")); err != nil {
		t.Fatal("stale manifest was removed")
	}
}

func TestNativeCleanupFailureKeepsCommittedManifestForRetry(t *testing.T) {
	n := newNativeRuntime(t.TempDir(), nil)
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	tries := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries++
		if tries == 1 {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	n.server = &OpenCodeServer{baseURL: ts.URL, client: ts.Client()}
	p := filepath.Join(n.dir(), "retry.json")
	if err := n.save(p, nativeManifest{Session: "opencode:s", Lens: "l", RawHigh: 1, Total: 1, Fork: "fork"}); err != nil {
		t.Fatal(err)
	}
	if err := n.finalize(p); err == nil {
		t.Fatal("expected cleanup failure")
	}
	m, err := n.load(p)
	if err != nil || !m.Committed {
		t.Fatalf("manifest not committed for retry: %+v %v", m, err)
	}
	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("retry manifest retained")
	}
}
