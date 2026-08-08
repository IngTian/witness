package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
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
	out, err := runner{}.Run(context.Background(), "", "sys", "corpus")
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

	_, err := runner{}.Run(ctx, "", "sys", "corpus")
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

// The disabled-runner guard must still fire before anything is spawned.
//
// Cheap, but it pins the ordering the new error handling could plausibly disturb: the kill switch
// is checked before the child is built, so WITNESS_DISABLE_EXTERNAL_RUNNERS=1 spawns nothing.
func TestDisabledRunnerSpawnsNothing(t *testing.T) {
	t.Setenv("WITNESS_DISABLE_EXTERNAL_RUNNERS", "1")
	prev := buildRunCmdFn
	spawned := false
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		spawned = true
		return exec.CommandContext(ctx, os.Args[0], "-test.run=XXX_NONE")
	}
	t.Cleanup(func() { buildRunCmdFn = prev })

	if _, err := (runner{}).Run(context.Background(), "", "sys", "corpus"); err == nil {
		t.Error("the disabled runner must return an error")
	}
	if spawned {
		t.Error("the disable switch was checked AFTER building the child; it must short-circuit first")
	}
}
