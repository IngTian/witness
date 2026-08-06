package commands

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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

	// And the real implementation must actually honor a deadline. Drive the same shape
	// against a command that never exits, and require it to return.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "60")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		_ = cmd.Run()
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("CommandContext did not kill a hung child — the bound is not effective")
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
	// Both callers must route through the bounded helpers.
	for _, want := range []string{"claudeCLIRun(", "claudeCLIOutput("} {
		if !strings.Contains(src, want) {
			t.Errorf("expected the bounded helper %s to be in use", want)
		}
	}
}
