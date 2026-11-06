//go:build !windows

package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The hook shim must not re-enter install.sh's ~/.local/bin launcher forever when the bundled
// binary is missing (`make clean`, `git clean -fdx`, or an unmapped arch like armv7l).
//
// The shim's PATH fallback execs `witness`, which is the launcher, and the launcher execs the
// shim: reproduced at 25 re-entries before a counter stopped it. Claude Code hooks spin until
// their timeout (SessionStart is synchronous), and the OpenCode plugin spawns with no timeout and
// unref()s, so every event leaks another looping process — "capture must never break a session".
//
// Bounded twice: the fake launcher gives up after 20 entries, and the shim runs under a 10s
// context. It spawns only bash and coreutils, never an editor CLI.
func TestHookShimDoesNotLoopThroughThePathLauncher(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "hooks", "witness.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// A plugin root with the shim and no bin/ and no go.mod: neither the bundled binary nor the
	// `go run` dev fallback is available, which is exactly the state after `make clean`.
	root := t.TempDir()
	shim := filepath.Join(root, "hooks", "witness.sh")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, src, 0o755); err != nil {
		t.Fatal(err)
	}
	// install.sh's launcher, plus an entry counter that stops a loop.
	pathDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "entries")
	launcher := "#!" + bash + "\n" +
		"n=$(( $(cat " + counter + " 2>/dev/null || echo 0) + 1 )); echo $n > " + counter + "\n" +
		"[ $n -gt 20 ] && exit 3\n" +
		"exec " + shim + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(pathDir, "witness"), []byte(launcher), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, shim, "capture") // hooks invoke the shim directly
	cmd.Env = []string{"PATH=" + pathDir + ":/usr/bin:/bin", "CLAUDE_PLUGIN_ROOT=" + root, "HOME=" + t.TempDir()}
	cmd.Stdin = strings.NewReader(`{"session_id":"s"}`)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the shim did not return within 10s: %s", out)
	}
	b, _ := os.ReadFile(counter)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n != 1 {
		t.Fatalf("launcher entered %d time(s), err %v (want once, exit 0 — a hook with no binary is a no-op): %s", n, err, out)
	}
}
