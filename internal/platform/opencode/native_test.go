package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
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

func TestNativeRunUsesADistinctFreshScratchSessionPerJob(t *testing.T) {
	root, source := t.TempDir(), filepath.Join(t.TempDir(), "opencode.db")
	writeFakeSourceDB(t, source)
	t.Setenv("WITNESS_OPENCODE_DB", source)
	var commands [][]string
	oldCommand := nativeCommand
	nativeCommand = func(_ context.Context, root string, env []string, args ...string) ([]byte, error) {
		commands = append(commands, append(append([]string{"root=" + root}, env...), "--", strings.Join(args, " ")))
		return []byte(`{"info":{},"messages":[]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()
	// A FRESH scratch session per job, not a fork of the user's conversation: POST /api/session
	// carries no source id at all, and the fake fails on any route it does not know (a fork).
	fake, srv := newFakeV2(t)
	fake.turn = okTurn("[]")
	sessions := func() int { return fake.count(func(c string) bool { return c == "POST /api/session" }) }
	prompts := func() int { return fake.count(func(c string) bool { return strings.HasSuffix(c, "/prompt") }) }
	deletes := func() int { return fake.count(func(c string) bool { return strings.HasPrefix(c, "DELETE ") }) }
	n := newNativeRuntime(root, srv)
	jobs := []struct{ lens, input string }{{"a", "0:same"}, {"a", "1:same"}, {"b", "0:same"}}
	for _, job := range jobs {
		w := &platform.NativeSession{Session: "opencode:source", RawHigh: 7, Total: 2, Lens: job.lens, Input: job.input}
		if _, err := n.run(context.Background(), w, "openai/gpt-5.5", "P", "I"); err != nil {
			t.Fatal(err)
		}
		if deletes() != 0 {
			t.Fatal("the scratch session was deleted before finalization")
		}
	}
	// Three jobs, three DISTINCT scratch sessions, three prompts: no sharing of context
	// between lenses or chunks.
	if sessions() != 3 || prompts() != 3 {
		t.Fatalf("scratch sessions=%d prompts=%d, want 3 and 3", sessions(), prompts())
	}
	beforeCommands, beforePrompts := len(commands), prompts()
	n2 := newNativeRuntime(root, n.server)
	if _, err := n2.run(context.Background(), &platform.NativeSession{Session: "opencode:source", RawHigh: 7, Total: 2, Lens: "a", Input: "0:same"}, "openai/gpt-5.5", "P", "I"); err != nil {
		t.Fatal(err)
	}
	if len(commands) != beforeCommands || prompts() != beforePrompts {
		t.Fatalf("completed reply was not reused: commands=%d prompts=%d", len(commands), prompts())
	}
	// ONE subprocess per job: the export. There is no import any more — it existed only to
	// materialize a fork parent, and the scratch session is now created empty. Asserting the
	// exact count is what keeps a re-introduced import (or a second export per job) visible.
	if len(commands) != 3 {
		t.Fatalf("commands=%d, want exactly one `export` per lens+chunk job and no import", len(commands))
	}
	for _, c := range commands {
		if strings.Contains(strings.Join(c, "\n"), "import --pure") {
			t.Error("an `opencode import` ran: the snapshot is only exported for its digest now, " +
				"never imported (that step existed solely to give fork() a parent)")
		}
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
			// openCodeCommand isolates XDG_DATA_HOME to the root it is given
			// (TestOpenCodeCommandAppliesTheSpawnPolicy); the export must give it witness's.
			if !strings.HasPrefix(joined, "root="+root+"\n") {
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
	nativeCommand = func(context.Context, string, []string, ...string) ([]byte, error) {
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

func TestNativeRunsGenerateConcurrentlyAfterIsolatedScratchSetup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WITNESS_OPENCODE_DB", writeFakeSourceDB(t, filepath.Join(t.TempDir(), "source.db")))
	oldCommand := nativeCommand
	nativeCommand = func(context.Context, string, []string, ...string) ([]byte, error) {
		return []byte(`{"info":{},"messages":[]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()
	// A RENDEZVOUS, not a sleep: no turn starts until all three are prompted, so the concurrency
	// assertion is deterministic. (A version that slept 20ms and hoped the windows overlapped
	// flaked to peak=1 whenever CPU contention serialized them.) If the native path ever
	// re-serializes generation, the first turn never starts and its queue budget fails the test.
	prev := queueWaitBudget
	queueWaitBudget = 10 * time.Second
	t.Cleanup(func() { queueWaitBudget = prev })
	const wantConcurrent = 3
	fake, srv := newFakeV2(t)
	fake.turn = func(s *fakeSession) []map[string]any { // runs under fake.mu
		prompted := 0
		for _, o := range fake.sessions {
			if o.MsgID != "" {
				prompted++
			}
		}
		if prompted < wantConcurrent {
			return []map[string]any{s.user()} // queued until every generation is in flight
		}
		return okTurn("[]")(s)
	}
	n := newNativeRuntime(root, srv)

	var wg sync.WaitGroup
	errs := make(chan error, wantConcurrent)
	for i := range wantConcurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := n.run(context.Background(), &platform.NativeSession{
				Session: "opencode:source", RawHigh: 7, Total: 2, Lens: fmt.Sprintf("lens-%d", i), Input: "0:chunk",
			}, "openai/gpt-5.5", "P", "I")
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
	if got := fake.count(func(c string) bool { return c == "POST /api/session" }); got != wantConcurrent {
		t.Fatalf("scratch sessions=%d, want %d", got, wantConcurrent)
	}
	if fake.peak() != wantConcurrent {
		t.Fatalf("peak concurrent native generations=%d, want %d", fake.peak(), wantConcurrent)
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

func TestNativeReconcileCleansCommittedAndStaleGenerations(t *testing.T) {
	n, deletesp := nativeReconcileFixture(t, 9, "ok")
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
	deletes := *deletesp
	if len(deletes) != 2 {
		t.Fatalf("deletes=%v", deletes)
	}
	if strings.Join(deletes, ",") != "/api/session/f2,/api/session/f3" {
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
	n.server = newOpenCodeServer(ts.URL, "", "", ts.Client())
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

// nativeReconcileFixture builds a runtime whose sibling witness.db holds session opencode:s with
// rawRows L0 rows (raw ids 1..rawRows), distilled through 2 records for distilledLens (none when
// empty), plus a fake OpenCode server that records fork deletions. It is a REAL store, written
// through the store's own API: reconcile reads witness.db with raw SQL, so a hand-rolled schema
// here would keep passing after a store schema change silently broke that read (the #119 leak).
func nativeReconcileFixture(t *testing.T, rawRows int, distilledLens string) (*nativeRuntime, *[]string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "runtime")
	t.Setenv("WITNESS_HOME", home)
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rawRows; i++ {
		if err := st.AppendRaw(store.RawRecord{Session: "opencode:s", Seq: i, Role: "user", Text: fmt.Sprint("turn ", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if distilledLens != "" {
		if err := st.MarkDistilled("opencode:s", distilledLens, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
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
	n := newNativeRuntime(root, newOpenCodeServer(ts.URL, "", "", ts.Client()))
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
	n, deletes := nativeReconcileFixture(t, 9, "ok")
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
	if len(*deletes) != 1 || (*deletes)[0] != "/api/session/f_good" {
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
	n, _ := nativeReconcileFixture(t, 9, "")
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
	// raw now holds id 9 AND newer ids up to 12 for the same session.
	n, deletes := nativeReconcileFixture(t, 12, "")
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
	if len(*deletes) != 1 || (*deletes)[0] != "/api/session/f_old" {
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
	nativeCommand = func(context.Context, string, []string, ...string) ([]byte, error) {
		return []byte(`{"info":{},"messages":[]}`), nil
	}
	defer func() { nativeCommand = oldCommand }()

	fake, srv := newFakeV2(t)
	fake.turn = okTurn("[]")
	prompts := func() int { return fake.count(func(c string) bool { return strings.HasSuffix(c, "/prompt") }) }
	n := newNativeRuntime(root, srv)
	session := func() *platform.NativeSession {
		return &platform.NativeSession{Session: "opencode:s", RawHigh: 7, Total: 1, Lens: "l", Input: "0:chunk"}
	}

	if _, err := n.run(context.Background(), session(), "prov/modelA", "promptA", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts() != 1 {
		t.Fatalf("first run should generate once, got %d", prompts())
	}
	// Same request → resume from the manifest, no new generation.
	if _, err := n.run(context.Background(), session(), "prov/modelA", "promptA", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts() != 1 {
		t.Fatalf("identical request must reuse the retained reply, got %d generations", prompts())
	}
	// Changed PROMPT → must re-generate, not replay.
	if _, err := n.run(context.Background(), session(), "prov/modelA", "promptB", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts() != 2 {
		t.Fatalf("a changed prompt must re-generate (stale reply replayed?), got %d generations", prompts())
	}
	// Changed MODEL → must re-generate too.
	if _, err := n.run(context.Background(), session(), "prov/modelB", "promptB", "I"); err != nil {
		t.Fatal(err)
	}
	if prompts() != 3 {
		t.Fatalf("a changed model must re-generate (stale reply replayed?), got %d generations", prompts())
	}
}
