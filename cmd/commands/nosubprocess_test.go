package commands

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// readSource reads a source file for the scan-based tests, with line endings NORMALIZED to LF.
//
// The normalization is load-bearing, not tidiness. These tests locate a function body with
// strings.Index(src, "\n}\n"), which does not match "\r\n}\r\n" — and git on Windows checks out
// CRLF by default. On a real Windows machine that made three such tests PANIC (Index returned -1
// and the slice bound went negative) and two others silently stop guarding anything. A test that
// cannot run on a platform protects nothing there.
//
// .gitattributes now pins *.go to LF so the checkout itself is consistent; normalizing here as
// well means these tests hold even in a tree that predates it, or one a tool rewrote.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return normalizeNewlines(string(b))
}

// normalizeNewlines converts CRLF and lone CR to LF, so source scans see one line-ending form.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// No test may spawn the real `claude` CLI, and the production call must be killable.
//
// This exists because of a concrete incident, not a style preference. cmdUninstallClaude
// invoked `claude mcp remove witness` via a bare exec.Command(...).Run() — no deadline, no
// cancellation. A test that called cmdUninstallClaude therefore spawned the user's real CLI,
// and those children never exited: 366 accumulated on a developer machine and pinned the
// CPU, with several reparented to PID 1 so nothing would ever reap them. In production the
// same shape means `witness unwire claude` hangs forever with no output and leaks an
// immortal child on every retry.
//
// Two properties are pinned here: the call goes through the overridable seam (so tests stub
// it), and it is built with CommandContext + a bounded timeout (so the child is killable).
func TestClaudeMCPRemoveIsBoundedAndStubbable(t *testing.T) {
	// The seam exists and is overridable.
	prev := removeClaudeMCP
	called := false
	removeClaudeMCP = func() { called = true }
	t.Cleanup(func() { removeClaudeMCP = prev })

	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := cmdUninstallClaude(); err != nil {
		t.Fatalf("uninstall on an empty dir: %v", err)
	}
	if !called {
		t.Error("cmdUninstallClaude bypassed removeClaudeMCP — a test would spawn the real CLI")
	}

	// The timeout is real and sane: long enough not to abort a healthy CLI, short enough
	// that a wedged one cannot hold the command open indefinitely.
	if claudeMCPRemoveTimeout <= 0 {
		t.Error("claudeMCPRemoveTimeout must be positive; an unbounded child is what leaked 366 orphans")
	}
	if claudeMCPRemoveTimeout > 2*time.Minute {
		t.Errorf("claudeMCPRemoveTimeout = %s is too long to be a liveness bound", claudeMCPRemoveTimeout)
	}

	// And the PRODUCTION builder must actually honor a deadline.
	//
	// This drives claudeCLICommand itself. The earlier version of this block hand-built its own
	// exec.CommandContext(ctx, "sleep", "60") and asserted that returned — which tests the Go
	// standard library, not witness: reverting claudeCLICommand to a bare exec.Command (the exact
	// 366-orphan defect) left it green. Here the child is the wedged one, so the assertion binds
	// to the function under test.
	//
	// `claude` itself is never spawned: the builder takes the program name from the caller's
	// args only, so we exercise it via a stand-in that hangs. cmd.Path is overwritten to a
	// no-op-shell sleep, keeping the ctx wiring the builder installed.
	if testing.Short() {
		t.Skip("spawns one short-lived `sleep` child")
	}
	sleepPath, lookErr := exec.LookPath("sleep")
	if lookErr != nil {
		t.Skipf("no `sleep` on PATH to stand in for a wedged CLI: %v", lookErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := claudeCLICommand(ctx, "mcp", "remove", "witness")
	if cmd.Cancel == nil {
		t.Fatal("claudeCLICommand returned a Cmd with no Cancel — it was not built with " +
			"exec.CommandContext, so a wedged `claude` can neither be killed nor reaped " +
			"(this is what left 366 orphans on a real machine)")
	}
	// Redirect the SAME Cmd at a program that never exits, preserving the ctx plumbing.
	cmd.Path, cmd.Args = sleepPath, []string{"sleep", "60"}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("the child ran %s before dying; the deadline is not effective", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the Cmd built by claudeCLICommand did NOT die on its context deadline — a wedged " +
			"`claude` would hang witness forever and leak an immortal child")
	}
}

// The production source must not reintroduce a deadline-less spawn of the CLI.
//
// EVERY `claude` invocation goes through claudeCLICommand, which is the single place that
// builds the child — so this pins two things: that builder uses CommandContext, and no other
// site bypasses it with a bare exec.Command("claude", ...).
func TestUninstallDoesNotSpawnClaudeWithoutAContext(t *testing.T) {
	src := readSource(t, "install.go")
	i := strings.Index(src, "func claudeCLICommand(")
	if i < 0 {
		t.Fatal("claudeCLICommand not found — every claude spawn must go through one bounded builder")
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit claudeCLICommand")
	}
	fn := src[i : i+end]
	if !strings.Contains(fn, "exec.CommandContext") {
		t.Error("claudeCLICommand must use exec.CommandContext so the child is killable")
	}

	// No site may spawn `claude` outside that builder.
	for n, line := range strings.Split(src, "\n") {
		if strings.Contains(line, `exec.Command("claude"`) {
			t.Errorf("install.go:%d spawns claude unbounded: %s", n+1, strings.TrimSpace(line))
		}
	}
	// Both callers must route through the bounded helpers — and be CALLED, not merely defined.
	//
	// Counting matters: `strings.Contains(src, "claudeCLIRun(")` is satisfied by the declaration
	// `func claudeCLIRun(args ...string) error {` alone. Go permits unused package-level
	// functions, so deleting every call site still compiled and still passed, leaving the helpers
	// orphaned while the real spawns went unbounded again.
	for _, want := range []string{"claudeCLIRun(", "claudeCLIOutput(", "claudeCLICommand("} {
		total := strings.Count(src, want)
		decls := strings.Count(src, "func "+want)
		if calls := total - decls; calls < 1 {
			t.Errorf("%s is defined but never called (%d occurrences, %d of them the declaration) — "+
				"the bounded helper is orphaned and something else is spawning the CLI", want, total, decls)
		}
	}
	// And no `claude` spawn may bypass the builder, in any exec form.
	for n, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, `"claude"`) && strings.Contains(line, "exec.Command") &&
			!strings.Contains(line, "exec.CommandContext") {
			t.Errorf("install.go:%d spawns claude without a context: %s", n+1, trimmed)
		}
	}
}
