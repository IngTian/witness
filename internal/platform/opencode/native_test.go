package opencode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

const nativeModel = "openai/gpt-5.5"

// nativeSourceDB stands in for the user's opencode.db: PR 1's v2 fixture, session ses_src with one
// user and one completed assistant turn. It points DefaultDBPath at it and returns that
// transcript's digest built the way distill/worker.go builds it from the imported L0, so a native
// check that stopped reading what the importer reads fails here as well as in
// TestTranscriptDigestMatchesTheImportedL0.
func nativeSourceDB(t *testing.T) string {
	t.Helper()
	t.Setenv("WITNESS_OPENCODE_DB", seedV2DB(t, session("ses_src", 5000), user("ses_src", 1, 1100, "hello"),
		assistant("ses_src", 2, 2000, 3000, item("text", "answer"))))
	return platform.TranscriptDigest([]platform.TranscriptEntry{{Role: "user", Text: "hello"}, {Role: "assistant", Text: "answer"}})
}

// forbidCLI fails the test if anything spawns an opencode CLI: the native path talks to the serve
// over HTTP and reads opencode.db itself. The `opencode export` it once ran opened its DB read-write
// (witness pointed it at a VACUUM INTO copy of the user's).
func forbidCLI(t *testing.T) {
	t.Helper()
	prev := execCommandContext
	t.Cleanup(func() { execCommandContext = prev })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("spawned %s %v: the native path runs no opencode CLI", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}
}

func nativeJob(lens, input, digest string) *platform.NativeSession {
	return &platform.NativeSession{Session: SessionPrefix + "ses_src", RawHigh: 7, Total: 2, Lens: lens, Input: input, Digest: digest}
}

// retain writes the manifest a previous process left for w: scratch session fork, request msg.
func retain(t *testing.T, n *nativeRuntime, w *platform.NativeSession, prompt, fork, msg string) string {
	t.Helper()
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	p := n.path(w, nativeModel, prompt)
	m := nativeManifest{Session: w.Session, Lens: w.Lens, Input: w.Input, Digest: w.Digest, RawHigh: w.RawHigh, Total: w.Total, Fork: fork, MessageID: msg}
	if err := n.save(p, m); err != nil {
		t.Fatal(err)
	}
	return p
}

func userRec(id string) map[string]any {
	return map[string]any{"id": id, "type": "user", "content": []map[string]any{{"type": "text", "text": "I"}}}
}

func TestNativeRunUsesADistinctFreshScratchSessionPerJob(t *testing.T) {
	forbidCLI(t)
	digest := nativeSourceDB(t)
	// A FRESH scratch session per job, not a fork of the user's conversation: POST /api/session
	// carries no source id at all, and the fake fails on any route it does not know (a fork).
	fake, srv := newFakeV2(t)
	fake.turn = okTurn("[]")
	sessions := func() int { return fake.count(func(c string) bool { return c == "POST /api/session" }) }
	prompts := func() int { return fake.count(func(c string) bool { return strings.HasSuffix(c, "/prompt") }) }
	deletes := func() int { return fake.count(func(c string) bool { return strings.HasPrefix(c, "DELETE ") }) }
	n := newNativeRuntime(t.TempDir(), srv)
	jobs := []struct{ lens, input, prompt string }{{"a", "0:same", "PA"}, {"a", "1:same", "PA"}, {"b", "0:same", "PB"}}
	for _, job := range jobs {
		if _, err := n.run(context.Background(), nativeJob(job.lens, job.input, digest), nativeModel, job.prompt, "I"); err != nil {
			t.Fatal(err)
		}
		if deletes() != 0 {
			t.Fatal("the scratch session was deleted before finalization")
		}
	}
	// Three jobs, three DISTINCT scratch sessions, three prompts: no sharing of context between
	// lenses or chunks. Each session runs as its own lens's agent: two lenses, two agents.
	if sessions() != 3 || prompts() != 3 {
		t.Fatalf("scratch sessions=%d prompts=%d, want 3 and 3", sessions(), prompts())
	}
	for i, job := range jobs {
		if got := fake.session(fmt.Sprintf("ses_%d", i+1)).Agent; got != agentID(job.prompt) {
			t.Errorf("job %d ran as agent %q, want %q", i, got, agentID(job.prompt))
		}
	}
	// A completed reply is reused by the next runtime on the same root: no new session or prompt.
	if _, err := newNativeRuntime(n.root, srv).run(context.Background(), nativeJob("a", "0:same", digest), nativeModel, "PA", "I"); err != nil {
		t.Fatal(err)
	}
	if sessions() != 3 || prompts() != 3 {
		t.Fatalf("completed reply was not reused: sessions=%d prompts=%d", sessions(), prompts())
	}
}

func TestNativeRunRefusesASessionChangedAfterCapture(t *testing.T) {
	forbidCLI(t)
	nativeSourceDB(t)
	// L0 holds the user turn alone: the assistant turn completed in opencode.db after capture.
	stale := platform.TranscriptDigest([]platform.TranscriptEntry{{Role: "user", Text: "hello"}})
	fake, srv := newFakeV2(t)
	_, err := newNativeRuntime(t.TempDir(), srv).run(context.Background(), nativeJob("l", "0:chunk", stale), nativeModel, "P", "I")
	if !errors.Is(err, errNativeSessionChanged) {
		t.Fatalf("run = %v, want errNativeSessionChanged", err)
	}
	if got := fake.count(func(c string) bool { return c == "POST /api/session" }); got != 0 {
		t.Fatalf("a changed session reached the serve: %d sessions created", got)
	}
}

func TestNativeResumeHarvestsACompletedReply(t *testing.T) {
	forbidCLI(t)
	var logs safeBuffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	// The previous worker died after the turn finished but before it saved the reply.
	fake, srv := newFakeV2(t)
	fake.leftover("ses_old", userRec("msg_old"), assistantRec("[1]", nil), idleRec("succeeded", nil))
	n := newNativeRuntime(t.TempDir(), srv)
	w := nativeJob("l", "0:chunk", "")
	p := retain(t, n, w, "P", "ses_old", "msg_old")
	if reply, err := n.run(context.Background(), w, nativeModel, "P", "I"); err != nil || reply != "[1]" {
		t.Fatalf("run = %q, %v; want the retained reply", reply, err)
	}
	if got := fake.count(func(c string) bool {
		return c == "POST /api/session" || strings.HasSuffix(c, "/prompt") || strings.HasPrefix(c, "DELETE ")
	}); got != 0 {
		t.Fatalf("a finished turn was regenerated or reaped: %v", fake.seen())
	}
	// Saved, so a crash before the L1 commit replays it instead of paying for it again.
	if m, err := n.load(p); err != nil || m.Reply != "[1]" {
		t.Fatalf("manifest = %+v, %v; want the reply saved", m, err)
	}
	// No createSession line names this session's model and agent, so the resume line must.
	if l := logs.String(); !strings.Contains(l, "model="+nativeModel) || !strings.Contains(l, "agent="+agentID("P")) {
		t.Errorf("the resume log names neither model nor agent:\n%s", l)
	}
}

// What a private --stdio serve leaves when the worker dies mid-turn: our request and an assistant
// that never finished, with no idle. No serve resumes that turn, and re-sending msg_old is a no-op
// that returns the original (critic C3), so prompting ses_old again would wait out the budget.
//
// The fake serve starts with no agents, as a new serve's config does (per-serve config dirs are
// removed at Close), and fails a prompt whose agent it does not list: so this is also the resume
// through a fresh serve, end to end, and pins that the new session's agent is loaded into it.
func TestNativeResumeStartsAFreshSessionForADeadTurn(t *testing.T) {
	forbidCLI(t)
	digest := nativeSourceDB(t)
	fake, srv := newFakeV2(t)
	fake.turn = okTurn("[2]")
	fake.leftover("ses_old", userRec("msg_old"), assistantRec("", nil))
	n := newNativeRuntime(t.TempDir(), srv)
	w := nativeJob("l", "0:chunk", digest)
	p := retain(t, n, w, "P", "ses_old", "msg_old")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reply, err := n.run(ctx, w, nativeModel, "P", "I"); err != nil || reply != "[2]" {
		t.Fatalf("run = %q, %v; want the fresh session's reply", reply, err)
	}
	if got := fake.count(func(c string) bool { return c == "DELETE /api/session/ses_old" }); got != 1 {
		t.Errorf("the dead session was deleted %d times, want 1: %v", got, fake.seen())
	}
	if got := fake.count(func(c string) bool { return c == "POST /api/session" }); got != 1 {
		t.Errorf("sessions created = %d, want 1 fresh one: %v", got, fake.seen())
	}
	fresh := fake.session("ses_1")
	if fresh == nil || fresh.MsgID == "" || fresh.MsgID == "msg_old" {
		t.Fatalf("fresh session = %+v; want a prompt under a new message id", fresh)
	}
	if m, err := n.load(p); err != nil || m.Fork != "ses_1" || m.MessageID != fresh.MsgID || m.Reply != "[2]" {
		t.Fatalf("manifest = %+v, %v; want it to name the fresh session, its request and the reply", m, err)
	}
}

// A manifest whose session the private DB no longer has (a v0.13-era manifest named a session of
// the deleted v1 DB): the harvest GET is a 404, and the generation starts over.
func TestNativeResumeSurvivesAMissingSession(t *testing.T) {
	forbidCLI(t)
	fake, srv := newFakeV2(t)
	fake.turn = okTurn("[3]")
	n := newNativeRuntime(t.TempDir(), srv)
	w := nativeJob("l", "0:chunk", nativeSourceDB(t))
	p := retain(t, n, w, "P", "ses_old", "msg_old")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reply, err := n.run(ctx, w, nativeModel, "P", "I"); err != nil || reply != "[3]" {
		t.Fatalf("run = %q, %v; want a fresh session's reply", reply, err)
	}
	if m, err := n.load(p); err != nil || m.Fork != "ses_1" {
		t.Fatalf("manifest = %+v, %v; want the fresh session", m, err)
	}
}

// A ctx cancelled during the harvest GET says nothing about the retained turn: the reply may be
// finished and merely unread. Deleting the session then would make the next attempt regenerate it
// (spending free allowance), so the session and its manifest stay for the next attempt.
func TestNativeResumeKeepsTheSessionWhenCancelledDuringTheHarvest(t *testing.T) {
	forbidCLI(t)
	fake, srv := newFakeV2(t)
	fake.leftover("ses_old", userRec("msg_old"), assistantRec("", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onCall = func(c string) {
		if c == "GET /api/session/ses_old/message" {
			cancel()
		}
	}
	n := newNativeRuntime(t.TempDir(), srv)
	w := nativeJob("l", "0:chunk", "")
	p := retain(t, n, w, "P", "ses_old", "msg_old")
	if _, err := n.run(ctx, w, nativeModel, "P", "I"); !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want context.Canceled", err)
	}
	if got := fake.count(func(c string) bool { return strings.HasPrefix(c, "DELETE ") }); got != 0 {
		t.Errorf("a cancelled harvest deleted the retained session: %v", fake.seen())
	}
	if m, err := n.load(p); err != nil || m.Fork != "ses_old" || m.MessageID != "msg_old" {
		t.Errorf("manifest = %+v, %v; want it to still name ses_old/msg_old", m, err)
	}
}

// Spec 7.5: on any error, interrupt (it aborts the upstream request). A prompt POST that fails
// may still have reached the serve, and a native session is retained rather than deleted.
func TestNativeRunInterruptsItsSessionWhenThePromptFails(t *testing.T) {
	forbidCLI(t)
	fake, srv := newFakeV2(t)
	fake.promptStatus = http.StatusInternalServerError
	if _, err := newNativeRuntime(t.TempDir(), srv).run(context.Background(), nativeJob("l", "0:chunk", ""), nativeModel, "P", "I"); err == nil {
		t.Fatal("run succeeded with a failing prompt")
	}
	if got := fake.count(func(c string) bool { return c == "POST /api/session/ses_1/interrupt" }); got != 1 {
		t.Fatalf("interrupts = %d, want 1: %v", got, fake.seen())
	}
}

func TestNativeRunsGenerateConcurrentlyAfterIsolatedScratchSetup(t *testing.T) {
	forbidCLI(t)
	digest := nativeSourceDB(t)
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
	n := newNativeRuntime(t.TempDir(), srv)

	var wg sync.WaitGroup
	errs := make(chan error, wantConcurrent)
	for i := range wantConcurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := n.run(context.Background(), nativeJob(fmt.Sprintf("lens-%d", i), "0:chunk", digest), nativeModel, "P", "I")
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
	for _, name := range []string{"committed.json", "stale.json"} {
		if _, err := os.Stat(filepath.Join(n.dir(), name)); !os.IsNotExist(err) {
			t.Fatalf("%s manifest retained", name)
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
}

// v0.13 and earlier left files the manifest loop never reaps: export()'s per-generation
// .snapshot.json (written even before its manifest), its disposable source-db copies, and an
// interrupted .tmp. Nothing writes the first two any more, so every one is residue — a snapshot
// beside a live manifest included — while the manifests themselves are kept.
func TestNativeReconcileSweepsLegacyResidue(t *testing.T) {
	n, _ := nativeReconcileFixture(t, 9, "")
	live := filepath.Join(n.dir(), "x.json")
	if err := n.save(live, nativeManifest{Session: "opencode:s", Lens: "pending", RawHigh: 9, Total: 2, Fork: "f1"}); err != nil {
		t.Fatal(err)
	}
	residue := []string{"x.snapshot.json", "source-ab.db", "y.tmp"}
	for _, name := range residue {
		if err := os.WriteFile(filepath.Join(n.dir(), name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}

	for _, name := range residue {
		if _, err := os.Stat(filepath.Join(n.dir(), name)); !os.IsNotExist(err) {
			t.Errorf("residue not swept: %s", name)
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live manifest must not be removed")
	}
}

// A generation a LATER mine has already grown past can never be the one that commits
// (a mine always reads the session's present max raw id). Without reaping it, a lens
// that keeps failing strands one manifest + live isolated scratch session per drain, forever.
func TestNativeReconcileReapsGenerationSupersededByNewerRaw(t *testing.T) {
	// raw now holds id 9 AND newer ids up to 12 for the same session.
	n, deletes := nativeReconcileFixture(t, 12, "")
	superseded := filepath.Join(n.dir(), "old.json")
	if err := n.save(superseded, nativeManifest{Session: "opencode:s", Lens: "failing", RawHigh: 9, Total: 2, Fork: "f_old"}); err != nil {
		t.Fatal(err)
	}
	newest := filepath.Join(n.dir(), "new.json")
	if err := n.save(newest, nativeManifest{Session: "opencode:s", Lens: "failing", RawHigh: 12, Total: 2, Fork: "f_new"}); err != nil {
		t.Fatal(err)
	}

	if err := n.reconcile(); err != nil {
		t.Fatal(err)
	}

	// The superseded generation is reaped, scratch session included.
	if _, err := os.Stat(superseded); !os.IsNotExist(err) {
		t.Fatal("generation superseded by a newer raw id must be reaped")
	}
	if len(*deletes) != 1 || (*deletes)[0] != "/api/session/f_old" {
		t.Fatalf("only the superseded session should be deleted, got %v", *deletes)
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
	forbidCLI(t)
	fake, srv := newFakeV2(t)
	fake.turn = okTurn("[]")
	prompts := func() int { return fake.count(func(c string) bool { return strings.HasSuffix(c, "/prompt") }) }
	n := newNativeRuntime(t.TempDir(), srv)
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
