package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// unsetDisableSwitch clears WITNESS_DISABLE_EXTERNAL_RUNNERS for one test.
//
// `make test` (and CI) run the whole suite with WITNESS_DISABLE_EXTERNAL_RUNNERS=1, so Run returns
// its "runner disabled" error on the FIRST line and never reaches the error classification these
// tests exist to pin. Without this the tests pass when run alone and fail — or worse, silently
// assert nothing — under the real command, which is the exact failure mode this branch has been
// removing. t.Setenv also restores the previous value at cleanup, so the switch is back on for
// every other test in the package.
func unsetDisableSwitch(t *testing.T) {
	t.Helper()
	t.Setenv("WITNESS_DISABLE_EXTERNAL_RUNNERS", "")
	if platform.ExternalRunnersDisabled() {
		t.Fatal("the external-runner kill switch is still set; this test cannot reach Run's " +
			"error classification")
	}
}

// testRunner is the runner NewRunner mints, over a scratch runtime root (Run refuses an empty one).
func testRunner(t *testing.T) platform.Runner {
	return Platform{}.NewRunner(store.Config{RuntimeRoot: t.TempDir()})
}

// A `claude -p` that runs out of time must be reported as context.DeadlineExceeded.
//
// This is the engine's BACKPRESSURE CONTRACT, not cosmetics. internal/distill classifies a mine
// error with errors.Is(err, context.DeadlineExceeded) and only then sets MineTimedOut, which is the
// single signal that narrows the drain's concurrency window. os/exec does not produce that error:
// when CommandContext kills the child, Run returns *exec.ExitError "signal: killed", for which
// errors.Is(..., context.DeadlineExceeded) is FALSE. So the original bare wrap made the signal
// permanently dead on the Claude path — a timed-out mine was filed as a generic transport failure
// that backs the lens off, and concurrency never narrowed no matter how many Runs starved.
//
// The test drives the REAL Run() so the assertion covers the classification under test; the child
// is a stand-in that hangs, and `claude` itself is never spawned.
func TestClaudeRunReportsTimeoutAsDeadlineExceeded(t *testing.T) {
	unsetDisableSwitch(t)
	if testing.Short() {
		t.Skip("spawns one short-lived `sleep` child")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no `sleep` on PATH to stand in for a wedged CLI: %v", err)
	}

	// Redirect the child at something that never finishes, keeping whatever ctx wiring Run
	// installed — that wiring is precisely what decides the error.
	prev := buildRunCmdFn
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		return exec.CommandContext(ctx, sleepPath, "60")
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	prevTimeout := claudeRunTimeout
	claudeRunTimeout = 250 * time.Millisecond
	t.Cleanup(func() { claudeRunTimeout = prevTimeout })

	start := time.Now()
	out, err := testRunner(t).Run(context.Background(), "", "sys", "corpus")
	if err == nil {
		t.Fatalf("a child that never exits must fail; got output %q", out)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("Run took %s — the timeout is not effective", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run() error = %v; must satisfy errors.Is(err, context.DeadlineExceeded) or the "+
			"engine cannot tell a starved request from a 4xx, and the drain never narrows its "+
			"concurrency window", err)
	}
	// The operator-facing half: the message must say it was a timeout, since "signal: killed"
	// alone reads like a crash or an OOM.
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error %q does not say the run exceeded its budget", err)
	}
}

// A CALLER-cancelled Run must NOT be reported as our timeout.
//
// The distinction is load-bearing in the other direction: the worker cancels its context on
// shutdown, and that is not a provider signal. If it surfaced as DeadlineExceeded the drain would
// treat an ordinary Ctrl-C as evidence the provider is starving and narrow the window, then persist
// a backoff for lenses that never actually failed.
func TestClaudeRunDistinguishesCallerCancellationFromTimeout(t *testing.T) {
	unsetDisableSwitch(t)
	if testing.Short() {
		t.Skip("spawns one short-lived `sleep` child")
	}
	sleepPath, lookErr := exec.LookPath("sleep")
	if lookErr != nil {
		t.Skipf("no `sleep` on PATH: %v", lookErr)
	}
	prev := buildRunCmdFn
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		return exec.CommandContext(ctx, sleepPath, "60")
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	// A generous internal timeout, so the only thing that can end this Run is the caller.
	prevTimeout := claudeRunTimeout
	claudeRunTimeout = 5 * time.Minute
	t.Cleanup(func() { claudeRunTimeout = prevTimeout })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := testRunner(t).Run(ctx, "", "sys", "corpus")
	if err == nil {
		t.Fatal("a cancelled Run must return an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v; a caller cancellation must satisfy errors.Is(err, "+
			"context.Canceled)", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run() error = %v; a caller cancellation must NOT read as our timeout, or "+
			"shutting the worker down would narrow the drain window and back lenses off", err)
	}
}

// A grandchild that keeps the child's stdout open must not hold Run past the child. os/exec's Wait
// waits for every writer of the pipe, so a `claude -p` that leaves a helper behind (a hook, an npm
// wrapper's node) held the drain slot until the grandchild ended, and a killed one hung the same way
// past its deadline. WaitDelay bounds both; ErrWaitDelay after exit 0 is the whole reply, not a
// failure. The `sleep 3` grandchildren end on their own.
func TestClaudeRunReturnsPromptlyWhenAGrandchildHoldsStdout(t *testing.T) {
	unsetDisableSwitch(t)
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("spawns a shell that leaves a `sleep 3` grandchild holding its stdout")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no `sh` to stand in for the CLI: %v", err)
	}
	prevDelay, prevTimeout, prevBuild := claudeWaitDelay, claudeRunTimeout, buildRunCmdFn
	t.Cleanup(func() { claudeWaitDelay, claudeRunTimeout, buildRunCmdFn = prevDelay, prevTimeout, prevBuild })
	claudeWaitDelay = 200 * time.Millisecond

	for _, c := range []struct {
		name, script string
		timeout      time.Duration
	}{
		{"exit 0", `echo '{"type":"result","is_error":false,"result":"ok"}'; sleep 3 &`, time.Minute},
		{"deadline", "sleep 3 & sleep 3", 100 * time.Millisecond},
	} {
		claudeRunTimeout = c.timeout
		buildRunCmdFn = func(ctx context.Context, _, _, _ string) *exec.Cmd {
			return exec.CommandContext(ctx, sh, "-c", c.script)
		}
		start := time.Now()
		out, err := testRunner(t).Run(context.Background(), "", "sys", "corpus")
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("%s: Run took %s; the grandchild held it (no WaitDelay?)", c.name, el)
		}
		if c.name == "exit 0" && (err != nil || out != "ok") {
			t.Errorf("exit 0: Run = %q, %v; want the reply (ErrWaitDelay after exit 0 is success)", out, err)
		}
		if c.name == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("deadline: Run error = %v; want context.DeadlineExceeded", err)
		}
	}
}

// The disabled-runner guard must still fire before anything is spawned.
//
// Cheap, but it pins the ordering the new error handling could plausibly disturb: the kill switch
// is checked before the child is built, so WITNESS_DISABLE_EXTERNAL_RUNNERS=1 spawns nothing, and
// the error names the switch so a disabled drain is diagnosable.
func TestDisabledRunnerSpawnsNothing(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "1")
	prev := buildRunCmdFn
	spawned := false
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		spawned = true
		return exec.CommandContext(ctx, os.Args[0], "-test.run=XXX_NONE")
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	if _, err := (runner{}).Run(context.Background(), "", "sys", "corpus"); err == nil || !strings.Contains(err.Error(), platform.DisableExternalRunnersEnv) {
		t.Errorf("the disabled runner must return an error naming %s, got %v", platform.DisableExternalRunnersEnv, err)
	}
	if spawned {
		t.Error("the disable switch was checked AFTER building the child; it must short-circuit first")
	}
}

// A failing `claude -p` prints its error on STDOUT, so a failure must carry stdout too.
//
// Run captured stdout but read it only on success, and every error branch reported stderr alone.
// The default-lens stall logged the same window 322 times as `claude -p failed: exit status 1
// (stderr: )` — the cause was in the discarded stdout, and finding it took a GUI prototype. The
// child here is a stand-in shell; `claude` itself is never spawned.
func TestClaudeRunFailureKeepsStdout(t *testing.T) {
	unsetDisableSwitch(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no `sh` to stand in for a failing CLI: %v", err)
	}
	prev := buildRunCmdFn
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		return exec.CommandContext(ctx, sh, "-c", "echo 'API Error: prompt is too long'; exit 1")
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	_, err = testRunner(t).Run(context.Background(), "", "sys", "corpus")
	if err == nil {
		t.Fatal("a child that exits 1 must fail")
	}
	if !strings.Contains(err.Error(), "prompt is too long") {
		t.Errorf("the error drops the child's stdout, where `claude -p` prints its error: %v", err)
	}
}

// A CLI too old for --tools must fail with its own stderr (nothing pins stderr today; Run
// reported only the exit code before the owner's 2026-11-01 default-lens-stall diagnosis).
func TestClaudeRunSurfacesARejectedFlag(t *testing.T) {
	unsetDisableSwitch(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no `sh` to stand in for a failing CLI: %v", err)
	}
	prev := buildRunCmdFn
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		return exec.CommandContext(ctx, sh, "-c", "echo 'error: unknown option --tools' >&2; exit 1")
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	_, err = testRunner(t).Run(context.Background(), "", "sys", "corpus")
	if err == nil || !strings.Contains(err.Error(), "unknown option --tools") {
		t.Errorf("a CLI that rejects a flag must fail with its own stderr in the error: %v", err)
	}
}

// Every Run must use ONE stable, private cwd under witness's runtime root. Claude Code loads
// CLAUDE.md from every ancestor of its cwd, so a dir under the world-writable temp dir still
// loads a planted /tmp/CLAUDE.md; and a fresh cwd per call leaves a ~/.claude/projects entry
// behind per call. The stub's child is a missing binary, so Run fails at Start and nothing spawns;
// the cwd is read off the *exec.Cmd Run was handed.
func TestClaudeRunUsesOneStablePrivateCwd(t *testing.T) {
	unsetDisableSwitch(t)
	root, tmp := t.TempDir(), t.TempDir()
	// Point os.TempDir() at a sibling of root, so "not under the temp dir" can fail here.
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	var cmds []*exec.Cmd
	prev := buildRunCmdFn
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		c := exec.CommandContext(ctx, filepath.Join(tmp, "no-such-cli"))
		cmds = append(cmds, c)
		return c
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	r := Platform{}.NewRunner(store.Config{RuntimeRoot: root})
	want := filepath.Join(root, "claude-cwd")
	for i := range 2 {
		_, _ = r.Run(context.Background(), "", "sys", "corpus")
		if len(cmds) != i+1 {
			t.Fatalf("Run %d did not build its child", i+1)
		}
		dir := cmds[i].Dir
		if dir != want {
			t.Fatalf("Run %d cwd = %q, want the stable runtime-root dir %q", i+1, dir, want)
		}
		if rel, err := filepath.Rel(os.TempDir(), dir); err == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("Run %d cwd %q is under os.TempDir() %q", i+1, dir, os.TempDir())
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("the cwd must survive Run %d: %v", i+1, err)
		}
		if runtime.GOOS != "windows" {
			if got := fi.Mode().Perm(); got != 0o700 {
				t.Errorf("Run %d cwd mode = %o, want 0700", i+1, got)
			}
			// A dir left looser (an older witness, a manual mkdir) must be tightened next Run.
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The owner's call: one `claude -p` may run 20 minutes. The worker is async, so a stuck call
// holding it that long is the accepted price, and compaction's union writes are sized against this
// wall (distill's compactUnionMaxChars folds the 24k-char cluster the 10-minute wall would kill).
// Lowering it silently re-breaks those writes.
func TestAClaudeCallMayRunTwentyMinutes(t *testing.T) {
	if claudeRunTimeout != 20*time.Minute {
		t.Errorf("claudeRunTimeout = %s; compaction's union cap is sized for a 20-minute wall", claudeRunTimeout)
	}
}
