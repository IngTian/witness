//go:build opencodecontract

package opencode

// The contract harness: what every contract_*_test.go uses to drive a REAL OpenCode v2 binary.
//
// Opt-in by build tag, so `make test` never compiles it (contractpins_test.go, untagged, checks
// that every contract file carries the tag). Everything here is hermetic and bounded:
//   - each test gets a scratch HOME/XDG/WITNESS_HOME and a subprocess env built from nothing, not
//     os.Environ(), so neither the runner's OPENCODE_* nor its real home can reach a spawned
//     OpenCode or witness, and a dead HTTP(S)_PROXY fails any non-loopback request (no model call);
//   - every spawn is exec.CommandContext under a deadline, and noLeaks checks the process table
//     returned to its baseline.
//
// On Windows the suite is CI-only: `wire` edits HKCU\Environment (the user PATH), and outside CI
// the worker-wakeup T7 leaves is never killed, so noLeaks reports it.
//
// Cleanup order matters, and noLeaks fixes it for any call order: Cleanup is LIFO, and noLeaks must
// run BEFORE the test's TempDir removal, or Windows' RemoveAll (which retries a dir a live process
// still holds for only 2 s) fails the test before noLeaks' 5 s grace even starts. So the order is
// always: the test's serves close, noLeaks waits out and kills any leftover, then the TempDir goes.
//
// The test binary doubles as a stub (init below): stubPath copies it under another name, and
// WITNESS_CONTRACT_STUB picks what it does, so tests need no second helper binary.

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/proc"
)

// contractProcessBound is the deadline on every long-lived process a contract test starts (a serve,
// a holder): far past any test, but a process the test forgot can never run unbounded.
const contractProcessBound = 10 * time.Minute

// init turns the test binary into a stub. It runs before testing parses flags, so a stub invoked
// with foreign arguments (`claude -p …`, an MCP command line) never reaches the test runner.
func init() {
	switch v := os.Getenv("WITNESS_CONTRACT_STUB"); v {
	case "":
		return
	case "sleep": // a `claude -p` or MCP server that never answers
		// T9: an MCP server's own child, the grandchild a contained serve must not lose to breakaway.
		if child := os.Getenv("WITNESS_CONTRACT_SLEEP_CHILD"); child != "" {
			c := exec.Command(child, "-test.run=^$") // bounded: it sleeps 600 s too
			c.Env = append(os.Environ(), "WITNESS_CONTRACT_SLEEP_CHILD=")
			if err := c.Start(); err != nil {
				fmt.Fprintln(os.Stderr, "witness contract stub: start sleep child:", err)
				os.Exit(1)
			}
		}
		time.Sleep(600 * time.Second)
		os.Exit(0)
	case "fail": // a `claude -p` that fails at once, so the mine backs off
		fmt.Fprintln(os.Stderr, "witness contract stub: failing as asked (WITNESS_CONTRACT_STUB=fail)")
		os.Exit(1)
	case "hold":
		holdStub() // contract_serve_test.go; never returns
	default:
		fmt.Fprintf(os.Stderr, "witness contract stub: unknown WITNESS_CONTRACT_STUB=%q\n", v)
		os.Exit(2)
	}
}

type contractOpenCode struct {
	Bin     string
	Version Version
}

// requireContract resolves the OpenCode under test, or fails. It never skips: a contract run that
// cannot reach OpenCode must go red, not green.
func requireContract(t *testing.T) contractOpenCode {
	t.Helper()
	for _, k := range []string{platform.DisableExternalRunnersEnv, "WITNESS_WORKER"} {
		if v := os.Getenv(k); v != "" {
			t.Fatalf("%s=%q: under it the runner refuses and witness short-circuits, so a contract test could only fail or pass vacuously (run it outside make test)", k, v)
		}
	}
	bin, err := ResolveExe()
	if err != nil {
		t.Fatalf("resolve opencode: %v (put the OpenCode under test on PATH)", err)
	}
	v, err := ProbeVersion(context.Background()) // bounded by openCodeProbeTimeout (60 s)
	if err != nil {
		t.Fatalf("%s: %v", bin, err)
	}
	if err := CheckVersion(v); err != nil {
		t.Fatalf("%s: %v", bin, err)
	}
	// The binary under test must be the one the CI leg pinned, or a stale PATH entry passes for it.
	if want := os.Getenv("WITNESS_CONTRACT_OPENCODE_VERSION"); want != "" {
		if pinned, err := ParseOpenCodeVersion(want); err != nil || pinned != v {
			t.Fatalf("%s is OpenCode %v, but WITNESS_CONTRACT_OPENCODE_VERSION=%q", bin, v, want)
		}
	}
	return contractOpenCode{Bin: bin, Version: v}
}

type scratch struct {
	Root, Home, ConfigDir, Work, WitnessHome string
	Env                                      []string
}

// scratchEnv makes a per-test root and the complete env a subprocess gets.
func scratchEnv(t *testing.T) scratch {
	t.Helper()
	root := t.TempDir()
	s := scratch{Root: root, Home: filepath.Join(root, "home"), ConfigDir: filepath.Join(root, "config"),
		Work: filepath.Join(root, "work"), WitnessHome: filepath.Join(root, "witness")}
	for _, e := range append(s.homeEnv(), "WORK="+s.Work) { // every dir the env names exists
		if k, dir, _ := strings.Cut(e, "="); !strings.HasSuffix(k, "_PROXY") {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.Env = append([]string{"PATH=" + os.Getenv("PATH")}, s.homeEnv()...)
	pass := []string{"WITNESS_ASSETS"} // the embedding model the workflow fetched
	if runtime.GOOS == "windows" {
		pass = append(pass, "SystemRoot", "ComSpec", "PATHEXT", "TEMP", "TMP", "ProgramData")
	}
	for _, k := range pass {
		if v, ok := os.LookupEnv(k); ok {
			s.Env = append(s.Env, k+"="+v)
		}
	}
	for _, k := range []string{"AUTOUPDATE", "MODELS_FETCH", "FREE_MODELS", "PROJECT_CONFIG", "CLAUDE_CODE"} {
		s.Env = append(s.Env, "OPENCODE_DISABLE_"+k+"=1")
	}
	return s
}

// homeEnv is the home, XDG and proxy part of the env, shared by scratchEnv and setProcessEnv. The
// NO_PROXY keeps the loopback mock and serve reachable through the dead proxy.
func (s scratch) homeEnv() []string {
	return []string{
		"HOME=" + s.Home,
		"USERPROFILE=" + s.Home,
		"APPDATA=" + filepath.Join(s.Root, "appdata"),
		"LOCALAPPDATA=" + filepath.Join(s.Root, "localappdata"),
		"XDG_CONFIG_HOME=" + s.ConfigDir,
		"XDG_DATA_HOME=" + filepath.Join(s.Root, "data"),
		"XDG_STATE_HOME=" + filepath.Join(s.Root, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(s.Root, "cache"),
		"WITNESS_HOME=" + s.WitnessHome,
		"HTTP_PROXY=http://127.0.0.1:9",
		"HTTPS_PROXY=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost",
	}
}

// setProcessEnv points THIS process at the scratch home, for the in-process runner (its
// openCodeCommand builds the serve's env from os.Environ()).
func (s scratch) setProcessEnv(t *testing.T) {
	for _, e := range s.homeEnv() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
}

// procEnv is s.Env as a subprocess gets it: ProgramData re-read now, because installSystemProvider
// moves it after scratchEnv ran, then extra. os/exec keeps the last of duplicate keys
// (case-insensitively on Windows), so extra overrides.
func (s scratch) procEnv(extra []string) []string {
	env := slices.Clone(s.Env)
	if runtime.GOOS == "windows" {
		env = append(env, "ProgramData="+os.Getenv("ProgramData"))
	}
	return append(env, extra...)
}

// disableAutoDistill stops the plugin's auto-gate from starting a stray worker. `config set` knows
// only runner/mine_model/review_model, and a missing config.toml means defaults, so a one-line
// file is read as written.
func disableAutoDistill(t *testing.T, s scratch) {
	t.Helper()
	if err := os.MkdirAll(s.WitnessHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.WitnessHome, "config.toml"), []byte("auto_distill = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runWitness runs the witness binary under test (WITNESS_CONTRACT_WITNESS_BIN) in the scratch env
// and returns its stdout; a non-zero exit fails the test with both streams.
func runWitness(t *testing.T, s scratch, extraEnv []string, args ...string) string {
	t.Helper()
	bin := os.Getenv("WITNESS_CONTRACT_WITNESS_BIN")
	if bin == "" {
		t.Fatal("WITNESS_CONTRACT_WITNESS_BIN is unset: point it at a built witness binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env, cmd.Dir = s.procEnv(extraEnv), s.Work
	cmd.WaitDelay = 5 * time.Second // a child left holding our pipes fails the run instead of hanging it
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("witness %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// stubPath copies the test binary as name (a `claude` or MCP-server stand-in that init above turns
// into a stub). On macOS a hard link instead: a copied binary's ad-hoc signature breaks and the
// copy dies with SIGKILL (AGENTS.md). Elsewhere a copy, which leaves no open question about
// removing a link to a running image (NTFS) at TempDir cleanup.
func stubPath(t *testing.T, name string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "darwin" {
		err = os.Link(exe, dst)
	} else {
		var b []byte
		if b, err = os.ReadFile(exe); err == nil {
			err = os.WriteFile(dst, b, 0o755)
		}
	}
	if err != nil {
		t.Fatalf("stub %s: %v", name, err)
	}
	return dst
}

// procName normalizes a process name for comparison: Windows names are case-insensitive and
// carry .exe.
func procName(name string) string { return strings.TrimSuffix(strings.ToLower(name), ".exe") }

// fresh returns live processes with these names that are not in the snapshot (a reused pid under
// another name counts as new).
func fresh(t *testing.T, before map[int]string, names ...string) map[int]string {
	t.Helper()
	out := map[int]string{}
	for pid, n := range procList(t) {
		if before[pid] != n && slices.Contains(names, n) {
			out[pid] = n
		}
	}
	return out
}

// killPID kills the process with the given pid (best-effort, no error reported).
func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// noLeaks snapshots every process a contract test could leave behind, and at Cleanup fails listing
// any new one still alive after 5 s. `witness` is the copy `wire opencode` installs on Windows
// (<LOCALAPPDATA>\witness\witness.exe), which the built binary's name does not match.
//
// It kills the extras only under CI=true: on a developer's machine a new `claude` or `opencode`
// may be the developer's own session, started during the test.
func noLeaks(t *testing.T) {
	t.Helper()
	// The test's ONE TempDir RemoveAll is registered at its first TempDir call; making that call
	// here, before our Cleanup, puts our wait ahead of the removal whatever order the test calls in.
	_ = t.TempDir()
	watched := map[string]bool{"opencode": true, "opencode-pty": true, "opencode-cli": true, "wstub": true, "wgrand": true, "claude": true, "witness": true}
	if bin := os.Getenv("WITNESS_CONTRACT_WITNESS_BIN"); bin != "" {
		watched[procName(filepath.Base(bin))] = true
	}
	snapshot := func() map[int]string { // one listing per poll
		out := map[int]string{}
		for pid, n := range procList(t) {
			if watched[n] {
				out[pid] = n
			}
		}
		return out
	}
	baseline := snapshot()
	watchedNames := slices.Collect(maps.Keys(watched))
	t.Cleanup(func() {
		left := fresh(t, baseline, watchedNames...)
		for deadline := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = fresh(t, baseline, watchedNames...) {
			time.Sleep(100 * time.Millisecond)
		}
		if len(left) == 0 {
			return
		}
		killed := os.Getenv("CI") == "true"
		if killed {
			for pid := range left {
				killPID(pid)
			}
		}
		t.Errorf("processes left behind 5 s after the test (pid: name; killed=%v): %v", killed, left)
	})
}

// atWork is path for the scratch Work location. v2 reads the location from `location[directory]`
// or the x-opencode-directory header, else uses the serve's cwd (server/src/location.ts:75-80);
// a bare `directory` query is ignored.
func atWork(path string, s scratch) string {
	return path + "?" + url.Values{"location[directory]": {s.Work}}.Encode()
}

// pidGone polls until pid is no longer alive, up to d.
func pidGone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); proc.Alive(pid); {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
