package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	cmd := openCodeCommand(context.Background(), "", "sub", "--flag")
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
	if !slices.Equal(gotArgs, []string{"sub", "--flag"}) {
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
	// A nil receiver must not panic: a caller may pass a nil from a failed construction.
	hideOpenCodeWindow(nil)
}
