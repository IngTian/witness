package commands

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPlumbingCommandsHidden(t *testing.T) {
	if !newImportCmd().Hidden {
		t.Error("import must be hidden (plugin/hook plumbing, not a user verb)")
	}
	obs := newObservationsCmd()
	for _, s := range obs.Commands() {
		switch s.Name() {
		case "record", "delete":
			if !s.Hidden {
				t.Errorf("observations %s must be hidden (MCP-parity, not a human verb)", s.Name())
			}
		case "search":
			if s.Hidden {
				t.Error("observations search must stay visible (human recall)")
			}
		}
	}
}

func TestRootSurface(t *testing.T) {
	root := newRootCmd()
	visible := map[string]bool{}
	for _, c := range root.Commands() {
		if !c.Hidden && c.Name() != "help" && c.Name() != "completion" {
			visible[c.Name()] = true
		}
	}
	// present:
	for _, want := range []string{"profile", "status", "lens", "config", "ingest", "install", "wire", "unwire", "doctor", "export", "cleanup"} {
		if !visible[want] {
			t.Errorf("expected visible command %q", want)
		}
	}
	// gone from the visible front door:
	for _, gone := range []string{"distill", "review", "version", "import"} {
		if visible[gone] {
			t.Errorf("%q must NOT be a visible top-level command anymore", gone)
		}
	}
	// groups registered
	if len(root.Groups()) == 0 {
		t.Error("root should register cobra command groups")
	}
}

func TestJSONOutputHasNoANSI(t *testing.T) {
	// status --json and profile --json must contain no ESC byte, TTY or not.
	// (Run the command with color forced on to prove decoration is gated off for --json.)
	old := useColor
	useColor = true
	defer func() { useColor = old }()
	// build an isolated archive
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	// capture stdout of cmdDistillStatus(true) and assert no "\x1b"
	out := captureStdout(t, func() { _ = cmdDistillStatus(true) })
	if strings.Contains(out, "\x1b") {
		t.Errorf("status --json must not contain ANSI escapes: %q", out)
	}
}

func TestInternalWorkerCommandResolution(t *testing.T) {
	// Regression guard for C1: the internal worker token `worker-run` must resolve to
	// the flag-bearing command (--auto/--since/--until), NOT to the hidden operator group
	// `worker` (which has run/stop/review subcommands but no flags). A name collision
	// kills automatic distillation: hooks spawn `worker-run --auto`, and a misroute to
	// the group errors ("unknown flag: --auto").
	root := newRootCmd()

	// Assert the internal worker token resolves to the flag-bearing command.
	cmd, _, err := root.Find([]string{"worker-run", "--auto"})
	if err != nil {
		t.Fatalf("worker-run token must resolve: %v", err)
	}
	if cmd.Name() != "worker-run" {
		t.Errorf("expected worker-run to resolve to 'worker-run', got %q", cmd.Name())
	}
	if cmd.Flags().Lookup("auto") == nil {
		t.Error("worker-run command must carry the --auto flag (proves it's the internal worker, not a group)")
	}

	// Assert the hidden operator group still resolves correctly.
	groupCmd, _, err := root.Find([]string{"worker"})
	if err != nil {
		t.Fatalf("worker group must resolve: %v", err)
	}
	if groupCmd.Name() != "worker" {
		t.Errorf("expected worker group to resolve to 'worker', got %q", groupCmd.Name())
	}
	runSubcmd, _, err := root.Find([]string{"worker", "run"})
	if err != nil || runSubcmd.Name() != "run" {
		t.Error("worker group must have a 'run' subcommand")
	}
}
