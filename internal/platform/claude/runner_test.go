package claude

import (
	"context"
	"io"
	"slices"
	"testing"
)

// The corpus must travel as the user turn, never as instructions: witness's prompt
// goes to --append-system-prompt; the corpus goes on stdin. Both VERBATIM — the fence is
// applied once, by distill.RunnerMine, before any runner sees them. Moved from distill
// with the runner impl (issue #21 PR4b).
func TestBuildRunCmdRoleSeparation(t *testing.T) {
	cmd := buildRunCmd(context.Background(), "", "EXTRACT INSTRUCTIONS", "payload")
	i := slices.Index(cmd.Args, "--append-system-prompt")
	if i < 0 || i+1 >= len(cmd.Args) {
		t.Fatalf("system prompt not passed: %v", cmd.Args)
	}
	if sys := cmd.Args[i+1]; sys != "EXTRACT INSTRUCTIONS" {
		t.Fatalf("system prompt not passed verbatim: %q", sys)
	}
	for _, want := range []string{"-p", "--no-session-persistence", "--strict-mcp-config"} {
		if !slices.Contains(cmd.Args, want) {
			t.Errorf("isolation flag %q lost: %v", want, cmd.Args)
		}
	}
	// The corpus goes on stdin, not concatenated into the instructions.
	if cmd.Stdin == nil {
		t.Fatal("corpus must be passed on stdin")
	}
	if in, _ := io.ReadAll(cmd.Stdin); string(in) != "payload" {
		t.Fatalf("stdin = %q, want the corpus verbatim", in)
	}
}

// The model runs on an untrusted corpus. A tool turns an injected instruction into an
// action; the 2026-11-16 audit (D1) caught the model using tools. buildRunCmd must
// pass --tools "". model == "" on purpose, so --tools slipping into the --model branch
// would fail this.
func TestBuildRunCmdOffersNoTools(t *testing.T) {
	cmd := buildRunCmd(context.Background(), "", "SYS", "corpus")
	i := slices.Index(cmd.Args, "--tools")
	if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != "" {
		t.Fatalf(`argv must carry --tools "" (no built-in tool on an untrusted corpus): %q`, cmd.Args)
	}
}

func TestClaudeCmdIsolation(t *testing.T) {
	c := newClaudeCmd(context.Background(), "")
	// The isolation flags themselves are pinned on buildRunCmd (TestBuildRunCmdRoleSeparation, TestBuildRunCmdOffersNoTools).
	if slices.Contains(c.Args, "--model") {
		t.Errorf("empty model must omit --model, got %v", c.Args)
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
