package claude

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The corpus must travel as the user turn, never as instructions: witness's prompt
// goes to --append-system-prompt; the corpus is fenced on stdin. Moved from distill
// with the runner impl (issue #21 PR4b).
func TestBuildRunCmdRoleSeparation(t *testing.T) {
	cmd := buildRunCmd(context.Background(), "", "EXTRACT INSTRUCTIONS", "payload")
	i := slices.Index(cmd.Args, "--append-system-prompt")
	if i < 0 || i+1 >= len(cmd.Args) {
		t.Fatalf("system prompt not passed: %v", cmd.Args)
	}
	sys := cmd.Args[i+1]
	if !strings.Contains(sys, "EXTRACT INSTRUCTIONS") || !strings.Contains(sys, "UNTRUSTED") {
		t.Fatalf("system prompt missing instructions or corpus notice: %q", sys)
	}
	for _, want := range []string{"-p", "--no-session-persistence", "--strict-mcp-config"} {
		if !slices.Contains(cmd.Args, want) {
			t.Errorf("isolation flag %q lost: %v", want, cmd.Args)
		}
	}
	// The corpus must be fenced on stdin, not concatenated into the instructions.
	if cmd.Stdin == nil {
		t.Fatal("corpus must be passed on stdin")
	}
}

func TestClaudeCmdIsolation(t *testing.T) {
	c := newClaudeCmd(context.Background(), "")

	for _, want := range []string{"-p", "--no-session-persistence", "--strict-mcp-config"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("missing %q in args: %v", want, c.Args)
		}
	}
	if slices.Contains(c.Args, "--model") {
		t.Errorf("empty model must omit --model, got %v", c.Args)
	}
	if c.Dir == "" {
		t.Errorf("cmd.Dir must be a neutral dir (not inherit the user's project cwd)")
	}

	c2 := newClaudeCmd(context.Background(), "claude-haiku-4-5")
	i := slices.Index(c2.Args, "--model")
	if i < 0 || i+1 >= len(c2.Args) || c2.Args[i+1] != "claude-haiku-4-5" {
		t.Errorf("model not passed correctly: %v", c2.Args)
	}
}

// The recursion guard MUST be set on the nested claude -p env, or the worker's own
// call re-fires the witness hooks (infinite recursion).
func TestBuildRunCmdSetsRecursionGuard(t *testing.T) {
	cmd := buildRunCmd(context.Background(), "", "SYS", "corpus")
	if !slices.Contains(cmd.Env, "WITNESS_WORKER=1") {
		t.Fatalf("WITNESS_WORKER=1 recursion guard missing from env")
	}
}
