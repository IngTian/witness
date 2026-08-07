package opencode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every `opencode` spawn must go through openCodeCommand.
//
// Three platform concerns are decided there — which binary (the Windows desktop install puts a
// GUI launcher on PATH ahead of the CLI), whether a console window pops, and the
// execCommandContext test seam. A site that calls exec.CommandContext("opencode", …) directly
// silently opts out of all three: on Windows it flashes a console and may run the GUI
// launcher, and in tests it spawns the user's REAL opencode. This is a source scan because the
// property is "no other call site exists", which no single behavioral test can establish.
func TestEveryOpenCodeSpawnGoesThroughTheOneHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "opencode_cmd.go" {
			continue // opencode_cmd.go IS the helper
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for n, line := range strings.Split(normalizeOpenCodeNewlines(string(b)), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// Any exec of the opencode binary by name, in any exec form.
			if strings.Contains(line, `"opencode"`) &&
				(strings.Contains(line, "exec.Command") || strings.Contains(line, "execCommandContext")) {
				t.Errorf("%s:%d spawns opencode outside openCodeCommand, bypassing the exe pinning, "+
					"the hidden-window flag, and the test seam: %s", f, n+1, trimmed)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files — the glob is wrong and this test is vacuous")
	}
}

// openCodeCommand must route through execCommandContext, the package's ONE indirection point.
// If it calls exec.CommandContext directly, unit tests spawn the user's real opencode — the
// class of mistake that once left 366 unkillable children on a developer machine.
func TestOpenCodeCommandUsesTheTestSeam(t *testing.T) {
	prev := execCommandContext
	t.Cleanup(func() { execCommandContext = prev })

	var gotName string
	var gotArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName, gotArgs = name, args
		return prev(ctx, "echo", "stubbed")
	}

	cmd := openCodeCommand(context.Background(), "serve", "--pure")
	if cmd == nil {
		t.Fatal("openCodeCommand returned nil")
	}
	if gotName == "" {
		t.Fatal("openCodeCommand did not go through execCommandContext — a test would spawn the " +
			"user's real opencode")
	}
	// The resolved binary must still BE opencode (an absolute path on Windows, the bare name
	// when PATH lookup fails), never some other program.
	if base := strings.ToLower(filepath.Base(gotName)); !strings.HasPrefix(base, "opencode") {
		t.Errorf("resolved binary %q is not opencode", gotName)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "serve" || gotArgs[1] != "--pure" {
		t.Errorf("args were not passed through verbatim: %q", gotArgs)
	}
}

// hideOpenCodeWindow must be safe to call on every platform and must not clobber an attr
// another layer already set. Off Windows it is a no-op; on Windows it sets one field.
// Asserted unconditionally so the call is exercised on the platform where it is developed.
func TestHideOpenCodeWindowIsSafeEverywhere(t *testing.T) {
	cmd := exec.Command("echo", "hi")
	hideOpenCodeWindow(cmd)
	// The command must remain runnable — i.e. we did not corrupt it.
	if cmd.Path == "" || len(cmd.Args) != 2 {
		t.Fatalf("hideOpenCodeWindow damaged the command: path=%q args=%q", cmd.Path, cmd.Args)
	}
	// A nil receiver must not panic: reapPriorServe's Windows probe calls this on a command it
	// built, and a future caller may pass a nil from a failed construction.
	hideOpenCodeWindow(nil)
}

// reapPriorServe must NOT kill on a pid-file match alone.
//
// Pids are recycled, and this code runs precisely in the crash case where the file is stale —
// so between the hard kill and the next drain the OS may have handed that number to something
// else, plausibly the user's editor or another witness process. Killing it would destroy work.
// The guard is isOurServePID: only a process that still matches the private-serve fingerprint
// may be signalled.
//
// The record here names a LIVE process that is not an opencode serve — the PARENT of this test
// process (`go test`, or the shell) — with a DEAD owner, so gate 1 does not short-circuit and
// the identity gates are the only thing between the reap and an innocent process. A correct
// implementation must decline: our parent is not a witness serve and does not answer our
// password. If it killed anyway, the test binary would lose its parent mid-run.
func TestReapPriorServeDoesNotKillAnUnrelatedProcess(t *testing.T) {
	root := t.TempDir()
	pidFile := servePIDPath(root)
	if pidFile == "" {
		t.Fatal("servePIDPath returned empty for a non-empty runtimeRoot")
	}
	victim := os.Getppid()
	if victim <= 1 {
		t.Skip("no usable parent pid to stand in for a recycled-pid victim")
	}
	// The OWNER must be a pid that is genuinely dead, or gate 1 short-circuits and the identity
	// gates are never reached. Searching for one beats hardcoding: a low literal like 2 is a live
	// kernel thread in a Linux container (that assumption failed in CI on the first attempt),
	// while a high pid is free on a fresh container but taken on a long-running desktop.
	deadOwner := 0
	for _, cand := range []int{0x7FFFFFF0, 0x7FFFFF00, 0x3FFFFFF0, 999983, 999979} {
		if !processAlive(cand) {
			deadOwner = cand
			break
		}
	}
	if deadOwner == 0 {
		t.Skip("could not find a dead pid to use as the record's owner on this machine")
	}
	rec, err := json.Marshal(serveRecord{Serve: victim, Owner: deadOwner, Port: 1, Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, rec, 0o600); err != nil {
		t.Fatal(err)
	}

	reapPriorServe(root) // must not signal our parent

	// THE assertion: the victim is still alive. Everything else here is bookkeeping.
	if !processAlive(victim) {
		t.Fatalf("reapPriorServe killed pid %d, which was NOT a witness serve — a recycled pid in "+
			"the record is then enough to destroy an unrelated process", victim)
	}
	// The record must also be cleared, so a later start does not re-evaluate the same dead
	// record forever. This only holds because we established the owner is dead above; with a live
	// owner, keeping the record is the CORRECT behavior (see the live-sibling test).
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("the stale record survived (err=%v); each later start would re-evaluate it", err)
	}
}

// The identity guard itself: this process must never be mistaken for a witness serve.
func TestIsOurServePIDRejectsThisProcess(t *testing.T) {
	if isOurServePID(os.Getpid()) {
		t.Error("the test binary was identified as a witness `opencode serve` — reapPriorServe " +
			"would kill an arbitrary process whose pid landed in the file")
	}
	// A pid that cannot exist must be rejected rather than error into a kill.
	if isOurServePID(-1) {
		t.Error("a negative pid was accepted")
	}
}

// A LIVE sibling's serve must never be reaped, and its record must survive.
//
// This is the hazard that makes the pid file dangerous rather than merely useless. The record
// lives at a MACHINE-WIDE path (runtimeRoot is <store root>/runtime), so every witness process
// shares one file — and `witness lens try` opens an OpenCode runner WITHOUT taking WorkerLock,
// because the runner reports SweepsOnClose()==false. So a `lens try` can run reapPriorServe
// while a worker is mid-drain. If the reap keys on "the pid looks like our serve", it kills the
// WORKER's live serve: every remaining Run in that drain then fails against a closed port.
//
// The gate that prevents it is Owner — the pid of the witness process that started the serve.
// A live owner means a live sibling, not an orphan. Here the owner is this test process, which
// is by definition alive.
func TestReapPriorServeSparesALiveSiblingsServe(t *testing.T) {
	root := t.TempDir()
	pidFile := servePIDPath(root)
	// A record whose OWNER is alive (us) and whose serve pid is a plausible other process.
	rec, err := json.Marshal(serveRecord{Serve: 999999, Owner: os.Getpid(), Port: 1, Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, rec, 0o600); err != nil {
		t.Fatal(err)
	}

	reapPriorServe(root)

	// The record must SURVIVE: the live owner is responsible for removing it on its own Close,
	// and deleting it here would leave that serve untracked — unreapable on Windows, where this
	// record is the only cleanup path.
	if _, err := os.Stat(pidFile); err != nil {
		t.Errorf("a live sibling's serve record was deleted (%v) — that serve is now untracked and "+
			"can never be reaped", err)
	}
}

// processAlive must be right about BOTH answers, since each error direction is a distinct bug:
// a false "dead" lets reapPriorServe kill a live sibling's serve, and a false "alive" makes it
// decline to reap a genuine orphan forever.
func TestProcessAliveDistinguishesLiveFromDead(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this process was reported DEAD — reapPriorServe would treat a live owner's serve " +
			"as an orphan and kill it")
	}
	// pid 1 and below are never valid owners (init/launchd, or nonsense).
	for _, pid := range []int{0, 1, -1} {
		if processAlive(pid) {
			t.Errorf("processAlive(%d) = true; pids <= 1 must never be treated as a live owner", pid)
		}
	}
	// A pid that almost certainly does not exist must read as dead, or no orphan is ever reaped.
	// (Not absolutely guaranteed free, so this is a soft check with a clear message.)
	if processAlive(0x7FFFFFF0) {
		t.Log("note: pid 0x7FFFFFF0 appears live on this machine; the dead-pid direction was not " +
			"exercised. Re-run or pick another pid if this persists.")
	}
}

// The ownership probe must not authenticate against a port nobody owns, and must never be the
// thing that decides a kill when the record predates the port/password fields.
func TestServeAnswersOurPasswordRequiresBothPortAndPassword(t *testing.T) {
	for _, rec := range []serveRecord{
		{Serve: 2, Owner: 3},                          // neither
		{Serve: 2, Owner: 3, Port: 8080},              // no password
		{Serve: 2, Owner: 3, Password: "s3cret"},      // no port
		{Serve: 2, Owner: 3, Port: -1, Password: "s"}, // nonsense port
	} {
		if serveAnswersOurPassword(rec) {
			t.Errorf("an unprovable record was accepted as ours: %+v — a missing port/password must "+
				"never authorize a kill", rec)
		}
	}
	// A closed port must not authenticate. Port 1 is privileged and not a witness serve.
	if serveAnswersOurPassword(serveRecord{Serve: 2, Owner: 3, Port: 1, Password: "nope"}) {
		t.Error("a closed/foreign port authenticated as our serve")
	}
}

// Degenerate records must be discarded, never acted on.
//
// Note the pid <= 1 cases: pid 1 is init/launchd and pid 0 is the kernel/process-group
// wildcard — on Unix, Kill(0) signals the WHOLE PROCESS GROUP, so accepting a 0 here would
// signal every process in this group, not one stray serve. The `serve <= 1` gate is what stops
// a corrupt or truncated file from turning into that.
func TestReapPriorServeIgnoresGarbageRecords(t *testing.T) {
	for _, content := range []string{
		"",
		"   ",
		"not-json",
		"12345",         // the OLD bare-pid format: unreadable now, must be dropped not guessed
		`{"serve":0}`,   // process-group wildcard
		`{"serve":1}`,   // init/launchd
		`{"serve":-5}`,  // nonsense
		`{"owner":123}`, // no serve pid at all
		`{"serve":`,     // truncated mid-write (a crash during the write)
		`[]`,            // right JSON, wrong shape
	} {
		t.Run(strconv.Quote(content), func(t *testing.T) {
			root := t.TempDir()
			pidFile := servePIDPath(root)
			if err := os.WriteFile(pidFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			reapPriorServe(root) // must not panic, must not signal anything
			if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
				t.Errorf("unusable record %q was left in place; every later start would re-evaluate it", content)
			}
		})
	}
}

// No runtimeRoot (the legacy shared-DB path) means no pid tracking at all, and reaping must
// be a silent no-op rather than touching the process's cwd.
func TestServePIDPathIsEmptyWithoutARuntimeRoot(t *testing.T) {
	if got := servePIDPath(""); got != "" {
		t.Errorf("servePIDPath(\"\") = %q, want empty — an empty root must not resolve to a "+
			"relative path in whatever cwd the worker inherited", got)
	}
	reapPriorServe("") // must not panic
}
