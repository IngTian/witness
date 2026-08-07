package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spawnDetached must be inert under `go test`.
//
// It re-execs os.Executable(), which under `go test` is the TEST binary — so it re-ran the
// whole package's TestMain with `worker-run` as argv, setsid'd and Released, i.e. fully
// orphaned. Nothing reaps those, and `go test` cannot kill them either. Measured on a real
// machine: one `go test ./...` left 746 orphaned `commands.test worker-run` processes, drove
// load average past 20, and blew its own 600s timeout — in a package that passes in ~1.3s
// when quiet.
//
// The hazard was known and worked around per-test rather than prevented, so every new test
// that reached a spawn point re-created it. These tests pin the prevention.
func TestUnderTestDetectsTheTestBinary(t *testing.T) {
	if !underTest() {
		t.Fatal("underTest() is false inside a test binary — spawnDetached would fork orphans")
	}
}

// The real witness binary must NOT be considered under test, or distillation would never
// start for actual users. This is the half that a naive "always no-op" would break.
//
// It calls looksLikeTestBinary — the PRODUCTION predicate — deliberately. The earlier version of
// this test re-implemented the suffix check inline (`strings.HasSuffix(base, ".test")`) and
// asserted that copy against its own string literals, never invoking any witness code. That was a
// tautology: underTest could have been rewritten to `return true`, silently disabling distillation
// for every real user, and it still passed. The name half is also the ONLY half that runs in a
// real binary, since -test.* flags are absent there and underTest's flag short-circuit misses.
func TestUnderTestIsFalseForARealBinaryName(t *testing.T) {
	for _, name := range []string{
		"witness", "witness-darwin-arm64", "witness.exe", "witness-v0.7.2-linux-amd64",
		"/usr/local/bin/witness", `C:\Users\tzy20\AppData\Local\witness\witness.exe`,
		"witness-testing",   // "test" in the name but not the .test SUFFIX
		"latest",            // ends in "test" — must not match, the suffix is ".test"
		"my.test.d/witness", // ".test" in a PARENT dir, not the basename
	} {
		if looksLikeTestBinary(name) {
			t.Errorf("%q was misread as a test binary — real users would get no distillation", name)
		}
	}
	for _, name := range []string{
		"commands.test", "store.test", "commands.test.exe",
		"/tmp/go-build123/b396/commands.test",
		"COMMANDS.TEST", // the check lowercases first
	} {
		if !looksLikeTestBinary(name) {
			t.Errorf("%q must be recognized as a test binary, or `go test` forks unreapable orphans", name)
		}
	}
}

// The behavioral proof: calling spawnDetached many times spawns NOTHING.
//
// Counted by asking the OS, because that is the only check that would have caught the
// original leak — reading the code did not.
func TestSpawnDetachedSpawnsNothingUnderTest(t *testing.T) {
	self := filepath.Base(os.Args[0])
	count := func() int {
		out, err := exec.Command("ps", "-eo", "args").Output()
		if err != nil {
			t.Skipf("ps unavailable: %v", err)
		}
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, self) && strings.Contains(line, "worker-run") {
				n++
			}
		}
		return n
	}

	before := count()
	for i := 0; i < 25; i++ {
		spawnDetached("worker-run")
		spawnDetached("worker-run", "--auto")
	}
	// A spawn is asynchronous; give any child time to appear before declaring victory.
	time.Sleep(300 * time.Millisecond)
	if after := count(); after > before {
		t.Fatalf("50 spawnDetached calls created %d orphaned test-binary processes "+
			"(before=%d after=%d) — this is the leak that pinned a real machine at load 20",
			after-before, before, after)
	}
}

// ingest is the specific path that leaked: cmdIngest kicks a worker whenever it writes
// anything, and the ingest tests call it directly. It must still succeed while spawning
// nothing.
func TestIngestKicksNoWorkerUnderTest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	self := filepath.Base(os.Args[0])
	countSelf := func() int {
		out, err := exec.Command("ps", "-eo", "args").Output()
		if err != nil {
			t.Skipf("ps unavailable: %v", err)
		}
		return strings.Count(string(out), self)
	}
	before := countSelf()

	n, _, err := cmdIngest(strings.NewReader(`{"text":"body","id":"a","session":"s"}`+"\n"), true)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 1 {
		t.Fatalf("ingested %d, want 1", n)
	}
	time.Sleep(300 * time.Millisecond)
	if after := countSelf(); after > before {
		t.Errorf("ingest spawned %d test-binary processes (before=%d after=%d)", after-before, before, after)
	}
}

// Every spawn point must route through spawnDetached, so the single guard covers all of
// them. A direct exec of os.Executable() elsewhere would reintroduce the leak.
func TestNoCommandExecsTheOwnExecutableDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "cli.go" {
			continue // cli.go IS spawnDetached's home
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "os.Executable()") && strings.Contains(line, "exec.Command") {
				t.Errorf("%s:%d re-execs itself outside spawnDetached, bypassing the test guard: %s",
					f, n+1, trimmed)
			}
		}
	}
}
