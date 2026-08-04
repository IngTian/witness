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

// writeFakeSourceDB creates a minimal real SQLite file to stand in for the user's
// opencode.db. export() snapshots the source read-only (VACUUM INTO) before handing a
// COPY to the subprocess, so the file has to exist even when `opencode` itself is faked.
func writeFakeSourceDB(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS session (id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeRunUsesPrivateImportAndDistinctForks(t *testing.T) {
	root, source := t.TempDir(), filepath.Join(t.TempDir(), "opencode.db")
	writeFakeSourceDB(t, source)
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
		if strings.Contains(joined, "export --pure") {
			// The export subprocess must NEVER be handed the user's DB path: `opencode
			// export` opens its OPENCODE_DB read-write and durably mutates it. It gets a
			// disposable read-only snapshot COPY instead, plus the isolated XDG_DATA_HOME
			// so it cannot reach the user's data dir either. Match the exact line — the
			// env legitimately still carries witness's own WITNESS_OPENCODE_DB=<user db>
			// resolver var, which OpenCode never reads.
			if strings.Contains(joined, "\nOPENCODE_DB="+source) {
				t.Fatalf("export was handed the USER db read-write: %s", joined)
			}
			if !strings.Contains(joined, "OPENCODE_DB="+filepath.Join(root, "opencode-native", sourceCopyPrefix)) {
				t.Fatalf("export did not target a disposable source copy: %s", joined)
			}
			if !strings.Contains(joined, "XDG_DATA_HOME="+filepath.Join(root, "xdg")) {
				t.Fatalf("export not isolated from the user data dir: %s", joined)
			}
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
	t.Setenv("WITNESS_OPENCODE_DB", writeFakeSourceDB(t, filepath.Join(t.TempDir(), "source.db")))
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
	t.Setenv("WITNESS_OPENCODE_DB", writeFakeSourceDB(t, filepath.Join(t.TempDir(), "source.db")))
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
	// A RENDEZVOUS, not a sleep: every generation blocks until all three have arrived, so
	// the concurrency assertion is deterministic. The previous version slept 20ms and hoped
	// the windows overlapped, which flaked to peak=1 whenever CPU contention serialized
	// them (observed in a full-suite run). If the native path ever re-serializes
	// generation, the barrier never fills and the test fails by timeout instead.
	const wantConcurrent = 3
	barrier := make(chan struct{})
	var arrived atomic.Int32
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
			// Hold every generation open until all of them are in flight together.
			if arrived.Add(1) == wantConcurrent {
				close(barrier)
			}
			select {
			case <-barrier:
			case <-time.After(10 * time.Second):
				t.Error("native generations did not run concurrently: barrier never filled")
			}
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

func TestReadOnlyURIConnectsEscapedAbsolutePathReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "witness#archive.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker(value TEXT); INSERT INTO marker VALUES ('correct database')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ro, err := sql.Open("sqlite", readOnlyURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var value string
	if err := ro.QueryRow(`SELECT value FROM marker`).Scan(&value); err != nil || value != "correct database" {
		t.Fatalf("read correct database: value=%q err=%v", value, err)
	}
	if _, err := ro.Exec(`INSERT INTO marker VALUES ('must fail')`); err == nil {
		t.Fatal("read-only URI allowed write")
	}
}

func TestNativeReconcileCleansCommittedAndStaleGenerations(t *testing.T) {
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
	var deletes []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.URL.Path)
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
	current := nativeManifest{Session: "opencode:s", Lens: "pending", RawHigh: 9, Total: 2, Fork: "f1"}
	committed := nativeManifest{Session: "opencode:s", Lens: "ok", RawHigh: 9, Total: 2, Fork: "f2"}
	stale := nativeManifest{Session: "opencode:s", Lens: "stale", RawHigh: 8, Total: 2, Fork: "f3"}
	if err := n.save(filepath.Join(n.dir(), "current.json"), current); err != nil {
		t.Fatal(err)
	}
	if err := n.save(filepath.Join(n.dir(), "committed.json"), committed); err != nil {
		t.Fatal(err)
	}
	if err := n.save(filepath.Join(n.dir(), "stale.json"), stale); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"current.json", "committed.json", "stale.json"} {
		if err := os.WriteFile(n.snapshot(filepath.Join(n.dir(), name)), []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 2 {
		t.Fatalf("deletes=%v", deletes)
	}
	if strings.Join(deletes, ",") != "/session/f2,/session/f3" {
		t.Fatalf("deleted forks=%v", deletes)
	}
	if _, err := os.Stat(filepath.Join(n.dir(), "current.json")); err != nil {
		t.Fatal("uncommitted current manifest removed")
	}
	if _, err := os.Stat(n.snapshot(filepath.Join(n.dir(), "current.json"))); err != nil {
		t.Fatal("uncommitted current snapshot removed")
	}
	for _, name := range []string{"committed.json", "stale.json"} {
		if _, err := os.Stat(filepath.Join(n.dir(), name)); !os.IsNotExist(err) {
			t.Fatalf("%s manifest retained", name)
		}
		if _, err := os.Stat(n.snapshot(filepath.Join(n.dir(), name))); !os.IsNotExist(err) {
			t.Fatalf("%s snapshot retained", name)
		}
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
	if err := os.WriteFile(n.snapshot(p), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.finalize(p); err == nil {
		t.Fatal("expected cleanup failure")
	}
	m, err := n.load(p)
	if err != nil || !m.Committed {
		t.Fatalf("manifest not committed for retry: %+v %v", m, err)
	}
	if _, err := os.Stat(n.snapshot(p)); err != nil {
		t.Fatalf("snapshot not retained for retry: %v", err)
	}
	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("retry manifest retained")
	}
	if _, err := os.Stat(n.snapshot(p)); !os.IsNotExist(err) {
		t.Fatal("retry snapshot retained")
	}
}

// nativeReconcileFixture builds a runtime whose sibling witness.db has one session at
// raw high id 9, plus a fake OpenCode server that records fork deletions.
func nativeReconcileFixture(t *testing.T, extraSQL string) (*nativeRuntime, *[]string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(root), "witness.db"))
	if err != nil {
		t.Fatal(err)
	}
	stmt := `CREATE TABLE raw(id INTEGER PRIMARY KEY, session TEXT);
		CREATE TABLE progress(session TEXT,lens TEXT,distilled INTEGER);
		INSERT INTO raw VALUES(9,'opencode:s');` + extraSQL
	if _, err = db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
	db.Close()
	deletes := &[]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			*deletes = append(*deletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(ts.Close)
	n := newNativeRuntime(root, &OpenCodeServer{baseURL: ts.URL, client: ts.Client()})
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return n, deletes
}

// A single unreadable manifest must not end the sweep. os.ReadDir returns names sorted,
// so a malformed entry used to abort reconcile and hide every later manifest — and
// because runner Open treated that error as fatal, one bad file could block all
// OpenCode distillation. The bad entry is now reaped and the rest still processed.
func TestNativeReconcileIsolatesUnreadableManifest(t *testing.T) {
	n, deletes := nativeReconcileFixture(t, `INSERT INTO progress VALUES('opencode:s','ok',2)`)
	// "aaa" sorts BEFORE "zzz", so pre-fix the corrupt file aborted before zzz was seen.
	bad := filepath.Join(n.dir(), "aaa.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.snapshot(bad), []byte("snap"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(n.dir(), "zzz.json")
	if err := n.save(good, nativeManifest{Session: "opencode:s", Lens: "ok", RawHigh: 9, Total: 2, Fork: "f_good"}); err != nil {
		t.Fatal(err)
	}

	if err := n.reconcile(); err != nil {
		t.Fatalf("reconcile must not fail on one bad manifest: %v", err)
	}

	// The committed manifest AFTER the bad one was still finalized.
	if len(*deletes) != 1 || (*deletes)[0] != "/session/f_good" {
		t.Fatalf("later manifest was not reconciled: deletes=%v", *deletes)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatal("committed manifest after the bad entry should be finalized")
	}
	// The unparseable manifest is reaped (its fork id is unrecoverable), not retained.
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("unreadable manifest should be discarded, not leaked forever")
	}
	if _, err := os.Stat(n.snapshot(bad)); !os.IsNotExist(err) {
		t.Fatal("unreadable manifest's snapshot should be discarded too")
	}
}

// export() writes the snapshot BEFORE any manifest exists, so a crash in that window
// leaves an orphan .snapshot.json that the manifest-only loop can never see. Same for an
// interrupted .tmp and a leftover disposable source-db copy. reconcile must sweep them,
// while leaving a snapshot that a live manifest still owns.
func TestNativeReconcileSweepsOrphanResidue(t *testing.T) {
	n, _ := nativeReconcileFixture(t, "")
	owned := filepath.Join(n.dir(), "owned.json")
	if err := n.save(owned, nativeManifest{Session: "opencode:s", Lens: "pending", RawHigh: 9, Total: 2, Fork: "f1"}); err != nil {
		t.Fatal(err)
	}
	ownedSnap := n.snapshot(owned)
	orphanSnap := filepath.Join(n.dir(), "deadbeef.snapshot.json")
	tmp := filepath.Join(n.dir(), "deadbeef.json.tmp")
	srcCopy := filepath.Join(n.dir(), sourceCopyPrefix+"abc123.db")
	for _, p := range []string{ownedSnap, orphanSnap, tmp, srcCopy} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{orphanSnap, tmp, srcCopy} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("residue not swept: %s", filepath.Base(p))
		}
	}
	// A snapshot a live (uncommitted, still-current) manifest owns must survive.
	if _, err := os.Stat(ownedSnap); err != nil {
		t.Fatal("snapshot owned by a live manifest must not be swept")
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatal("live manifest must not be removed")
	}
}

// A generation a LATER mine has already grown past can never be the one that commits
// (a mine always reads the session's present max raw id). Without reaping it, a lens
// that keeps failing strands one manifest + full-session snapshot + live isolated fork
// per drain, forever.
func TestNativeReconcileReapsGenerationSupersededByNewerRaw(t *testing.T) {
	// raw now holds id 9 AND a newer id 12 for the same session.
	n, deletes := nativeReconcileFixture(t, `INSERT INTO raw VALUES(12,'opencode:s')`)
	superseded := filepath.Join(n.dir(), "old.json")
	if err := n.save(superseded, nativeManifest{Session: "opencode:s", Lens: "failing", RawHigh: 9, Total: 2, Fork: "f_old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.snapshot(superseded), []byte("snap"), 0o600); err != nil {
		t.Fatal(err)
	}
	newest := filepath.Join(n.dir(), "new.json")
	if err := n.save(newest, nativeManifest{Session: "opencode:s", Lens: "failing", RawHigh: 12, Total: 2, Fork: "f_new"}); err != nil {
		t.Fatal(err)
	}

	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}

	// The superseded generation is reaped, fork included.
	if _, err := os.Stat(superseded); !os.IsNotExist(err) {
		t.Fatal("generation superseded by a newer raw id must be reaped")
	}
	if _, err := os.Stat(n.snapshot(superseded)); !os.IsNotExist(err) {
		t.Fatal("superseded snapshot must be reaped")
	}
	if len(*deletes) != 1 || (*deletes)[0] != "/session/f_old" {
		t.Fatalf("only the superseded fork should be deleted, got %v", *deletes)
	}
	// The CURRENT generation (still the high-water mark, uncommitted) must survive.
	if _, err := os.Stat(newest); err != nil {
		t.Fatal("current uncommitted generation must be retained")
	}
}

// A retained manifest must not replay a stale reply after the REQUEST changes. The
// manifest is a crash-resume cache keyed by identity, and `lens backfill --fresh` clears
// only DB state — it cannot see the manifest directory. So if the key ignored the prompt
// and model, editing a lens prompt (or switching triage_model) and re-backfilling would
// short-circuit on the OLD model's answer and write it into L1 as if it were fresh.
func TestNativeManifestKeyCoversModelAndPrompt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WITNESS_OPENCODE_DB", writeFakeSourceDB(t, filepath.Join(t.TempDir(), "source.db")))
	oldCommand := nativeCommand
	nativeCommand = func(context.Context, []string, ...string) ([]byte, error) {
		return []byte(`{"info":{},"messages":[]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()
	oldPoll := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = oldPoll }()

	var prompts int
	var replies sync.Map
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork"):
			_, _ = fmt.Fprintf(w, `{"id":"fork_%d"}`, prompts+1)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt_async"):
			var body struct{ MessageID string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			prompts++
			replies.Store(strings.Split(r.URL.Path, "/")[2], body.MessageID)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/message"):
			id, ok := replies.Load(strings.Split(r.URL.Path, "/")[2])
			if !ok {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = fmt.Fprintf(w, `[
				{"info":{"id":%q,"role":"user"},"parts":[{"type":"text","text":"request"}]},
				{"info":{"id":"reply","role":"assistant"},"parts":[{"type":"text","text":"[]"}]}
			]`, id)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()
	n := newNativeRuntime(root, &OpenCodeServer{baseURL: ts.URL, client: ts.Client()})
	session := func() *platform.NativeSession {
		return &platform.NativeSession{Session: "opencode:s", RawHigh: 7, Total: 1, Lens: "l", Input: "0:chunk"}
	}

	if _, err := n.run(context.Background(), session(), "prov/modelA", "promptA", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Fatalf("first run should generate once, got %d", prompts)
	}
	// Same request → resume from the manifest, no new generation.
	if _, err := n.run(context.Background(), session(), "prov/modelA", "promptA", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Fatalf("identical request must reuse the retained reply, got %d generations", prompts)
	}
	// Changed PROMPT → must re-generate, not replay.
	if _, err := n.run(context.Background(), session(), "prov/modelA", "promptB", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts != 2 {
		t.Fatalf("a changed prompt must re-generate (stale reply replayed?), got %d generations", prompts)
	}
	// Changed MODEL → must re-generate too.
	if _, err := n.run(context.Background(), session(), "prov/modelB", "promptB", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts != 3 {
		t.Fatalf("a changed model must re-generate (stale reply replayed?), got %d generations", prompts)
	}
}
