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
func TestUnderTestIsFalseForARealBinaryName(t *testing.T) {
	// underTest short-circuits on the -test.* flags, which are always present here, so the
	// NAME half is verified directly against the same predicate the function applies.
	realNames := []string{"witness", "witness-darwin-arm64", "witness.exe", "witness-v0.7.2-linux-amd64"}
	for _, name := range realNames {
		base := strings.ToLower(filepath.Base(name))
		if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
			t.Errorf("%q would be misread as a test binary — real users would get no distillation", name)
		}
	}
	testNames := []string{"commands.test", "store.test", "commands.test.exe", "/tmp/go-build123/b396/commands.test"}
	for _, name := range testNames {
		base := strings.ToLower(filepath.Base(name))
		if !strings.HasSuffix(base, ".test") && !strings.HasSuffix(base, ".test.exe") {
			t.Errorf("%q must be recognized as a test binary", name)
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
