//go:build opencodecontract && windows

package opencode

// The Windows-only contracts (evidence windows.md §E): what only a real Windows run can settle.
//   - T11: the first `--version` and the first serve of a fresh install, against witness's probe
//     and ready budgets. A regression bound only: a hosted image may run without Defender's
//     real-time scan, so the owner's box measures the real first run (Task 3.12).
//   - T3: the npm `.cmd` → cmd.exe → opencode.exe chain, spawned raw. Production resolves the shim
//     away (T5), so this is evidence for the npm channel, not a production path.
//   - T5: resolution per install channel (npm, curl, desktop).
//   - T6: a decoy named OpenCode.exe first on PATH never hangs a probe.
//   - T8: 2.3.0's pty daemon (and so its console's processes) inside witness's kill-on-close job.
//   - T9: an MCP server's process tree under that job.
//
// Process trees are compared by pid AND name against a snapshot (fresh, survivors): a pid Windows
// reuses for another image counts as gone, and a developer's own opencode is never counted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

const npmPrefixMissing = "WITNESS_CONTRACT_NPM_PREFIX is unset: the npm step of .github/workflows/opencode-contract.yml " +
	"(Task 3.10) must export the Windows path of `npm prefix -g` (cygpath -w)"

func TestContractWindowsFirstRunLatency(t *testing.T) { // T11: the workflow runs it first, alone, on a fresh runner
	bin, err := ResolveExe()
	if err != nil {
		t.Fatalf("resolve opencode: %v (put the OpenCode under test on PATH)", err)
	}
	// Timed before requireContract, whose own probe would make this the binary's second run.
	start := time.Now()
	v, err := ProbeVersion(context.Background()) // fails on its own at openCodeProbeTimeout
	probe := time.Since(start)
	t.Logf("T11: first `%s --version`: %s (witness's budget %s)", bin, probe.Round(time.Millisecond), openCodeProbeTimeout)
	if err != nil || probe > openCodeProbeTimeout {
		t.Fatalf("first --version: %v after %s, want a version within %s", err, probe, openCodeProbeTimeout)
	}
	if err := CheckVersion(v); err != nil {
		t.Fatal(err)
	}
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	start = time.Now()
	startRawServe(t, oc.Bin, s) // fails on its own past 30 s of waiting
	ready := time.Since(start)
	t.Logf("T11: first serve to an authenticated /api/info: %s (witness's budget %s)", ready.Round(time.Millisecond), healthCheckBudget)
	if ready > healthCheckBudget {
		t.Fatalf("first serve ready after %s, past witness's %s health budget", ready, healthCheckBudget)
	}
}

func TestContractWindowsLeaseCmdChain(t *testing.T) { // T3: the npm leg only
	prefix := os.Getenv("WITNESS_CONTRACT_NPM_PREFIX") // before any scratch env (G3)
	if os.Getenv("WITNESS_CONTRACT_CHANNEL") != "npm" {
		t.Skip("T3 needs npm's opencode.cmd: the npm channel leg runs it")
	}
	if prefix == "" {
		t.Fatal(npmPrefixMissing)
	}
	shim := filepath.Join(prefix, "opencode.cmd")
	requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	// chain checks the spawned pid is cmd.exe and the serve answering is a different opencode.exe.
	chain := func(t *testing.T, spawned int, c *rawServe) map[int]string {
		t.Helper()
		pid := servePID(t, c)
		live := procList(t)
		if live[spawned] != "cmd" || live[pid] != "opencode" || pid == spawned {
			t.Fatalf("want %s to run as cmd.exe (pid %d: %q) → opencode.exe (/api/info pid %d: %q)", shim, spawned, live[spawned], pid, live[pid])
		}
		return map[int]string{spawned: "cmd", pid: "opencode"}
	}

	t.Run("close", func(t *testing.T) { // T2 through the chain: stdin's EOF reaches opencode.exe past cmd.exe
		before := procList(t)
		srv := startRawServe(t, shim, s)
		procs := chain(t, srv.Cmd.Process.Pid, srv)
		for pid, n := range fresh(t, before, "opencode") { // back to the baseline, not just the one we asked
			procs[pid] = n
		}
		start := time.Now()
		if err := srv.Close(); err != nil {
			t.Fatalf("close: %v after %s", err, time.Since(start))
		}
		if left := survivors(t, procs, 2*time.Second); len(left) > 0 {
			t.Fatalf("outlived the closed lease by 2 s: %v", left)
		}
	})

	t.Run("kill", func(t *testing.T) { // T1 through the chain: the holder's death reaches opencode.exe past cmd.exe
		holder, h := startHolder(t, shim, s, false, false, "WITNESS_CONTRACT_HOLD_RAW=1")
		procs := chain(t, h.ServePID, h.Client(s))
		_ = holder.Process.Kill()
		if left := survivors(t, procs, 2*time.Second); len(left) > 0 {
			t.Fatalf("outlived the killed holder by 2 s: %v (the lease did not pass through cmd.exe)", left)
		}
	})
}

func TestContractWindowsResolve(t *testing.T) { // T5
	prefix := os.Getenv("WITNESS_CONTRACT_NPM_PREFIX")
	channel := os.Getenv("WITNESS_CONTRACT_CHANNEL")
	if channel != "npm" && channel != "curl" && channel != "desktop" {
		t.Skip("pinned/latest legs resolve the zip on PATH; covered by requireContract")
	}
	t.Setenv(openCodeBinEnv, "") // as if no plugin handed a binary over
	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	switch channel {
	case "npm":
		if prefix == "" {
			t.Fatal(npmPrefixMissing)
		}
		want := filepath.Join(prefix, "node_modules", "@opencode", "cli", "bin", "opencode.exe")
		// SameFile, not a string compare: an 8.3 short name or another spelling of the prefix is
		// the same file.
		gi, gerr := os.Stat(got)
		wi, werr := os.Stat(want)
		if strings.EqualFold(filepath.Ext(got), ".cmd") || gerr != nil || werr != nil || !os.SameFile(gi, wi) {
			t.Fatalf("ResolveExe = %s (%v), want the real exe %s (%v), not the npm shim %s", got, gerr, want, werr, filepath.Join(prefix, "opencode.cmd"))
		}
	case "curl":
		if p, err := exec.LookPath("opencode"); err == nil {
			t.Fatalf("exec.LookPath found %s: the curl installer appends ~/.opencode/bin to $GITHUB_PATH whenever GITHUB_ACTIONS=true, even with --no-modify-path (install:470-473); run it with GITHUB_ACTIONS unset, or this case proves nothing", p)
		}
		want := filepath.Join(os.Getenv("USERPROFILE"), ".opencode", "bin", "opencode.exe")
		if !strings.EqualFold(got, want) {
			t.Fatalf("ResolveExe = %s, want the curl installer's %s", got, want)
		}
		requireContract(t) // the same rung, then the binary runs and is the pinned --version

	case "desktop":
		cli := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "ai.opencode.desktop", "resources", "opencode-cli.exe")
		if fi, err := os.Stat(cli); err != nil || fi.IsDir() {
			t.Fatalf("no bundled CLI at %s after the desktop /S install: %v", cli, err)
		}
		if !strings.EqualFold(got, cli) {
			t.Fatalf("ResolveExe = %s, want the desktop's bundled %s", got, cli)
		}
		beforeFile := os.Getenv("WITNESS_CONTRACT_PATH_BEFORE")
		if beforeFile == "" {
			t.Fatal("WITNESS_CONTRACT_PATH_BEFORE is unset: the desktop step of .github/workflows/opencode-contract.yml (Task 3.10) must write the user PATH to a file before the install and export its path")
		}
		b, err := os.ReadFile(beforeFile)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(b, []byte{0xff, 0xfe}) {
			t.Fatalf("%s is UTF-16; write it as UTF-8 (pwsh 7, or -Encoding utf8)", beforeFile)
		}
		was := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
		raw, expanded := userPath(t)
		if was != strings.TrimSpace(raw) && was != strings.TrimSpace(expanded) {
			t.Fatalf("the desktop install changed the user PATH:\nbefore: %q\nafter:  %q (expanded %q)", was, raw, expanded)
		}
		t.Setenv(openCodeBinEnv, cli) // what the plugin hands over from inside the desktop's serve
		if got, err := ResolveExe(); err != nil || !strings.EqualFold(got, cli) {
			t.Fatalf("with %s=%s: ResolveExe = %s, %v", openCodeBinEnv, cli, got, err)
		}
		v, err := ProbeVersion(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", cli, err)
		}
		if err := CheckVersion(v); err != nil {
			t.Fatalf("%s: %v", cli, err)
		}
	}
}

func TestContractWindowsDecoyOpenCodeExe(t *testing.T) { // T6, re-aimed: spec 7.9 deleted the dead case check
	noLeaks(t)
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "OpenCode.exe"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(openCodeBinEnv, "")
	got, _ := ResolveExe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	v, err := ProbeVersion(ctx)
	took := time.Since(start)
	t.Logf("T6: ResolveExe = %s; ProbeVersion = %v, %v after %s", got, v, err, took)
	// A GUI-named decoy first on PATH fails loudly or is skipped; it never hangs a probe, and never
	// passes for an OpenCode.
	if took >= 10*time.Second {
		t.Fatalf("ProbeVersion took %s with a decoy OpenCode.exe first on PATH: it hung until killed", took)
	}
	if err == nil {
		if cerr := CheckVersion(v); cerr != nil {
			t.Fatalf("the decoy %s read as OpenCode %v: %v", got, v, cerr)
		}
	}
}

func TestContractWindowsDaemonInJob(t *testing.T) { // T8
	oc := requireContract(t)
	if oc.Version.Major == 2 && oc.Version.Minor < 3 {
		t.Skipf("OpenCode %v spawns shell commands itself; the pty daemon runs them from 2.3.0", oc.Version)
	}
	noLeaks(t)
	s := scratchEnv(t)
	// shell starts a contained holder of witness's private serve and runs a 60 s ping through its
	// pty daemon. /api/shell is a server route, not a model tool, so witness's deny-all does not
	// gate it (core/src/shell.ts:138-141, server/src/handlers/shell.ts:19-27: no permission check).
	shell := func(t *testing.T) (*exec.Cmd, holderInfo, map[int]string) {
		t.Helper()
		before := procList(t)
		holder, h := startHolder(t, oc.Bin, s, true, false)
		code, body := h.Client(s).Do(http.MethodPost, atWork("/api/shell", s), map[string]string{"command": "ping -n 60 127.0.0.1"}, true)
		if code != http.StatusOK {
			t.Fatalf("POST /api/shell: %d %.500s", code, body)
		}
		t.Logf("POST /api/shell: %.300s", body) // its shell and pid, for a run that finds no daemon
		var tree map[int]string
		// ping without opencode-pty reads as OpenCode's spawn-it-itself fallback, not a job failure.
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("tree at failure: %v", tree)
			}
		})
		waitFor(t, 20*time.Second, "opencode-pty and ping running (no opencode-pty: the daemon never started, e.g. its spawn was refused inside the job)", func() bool {
			tree = fresh(t, before, "opencode", "opencode-pty", "ping")
			return hasName(tree, "opencode-pty") && hasName(tree, "ping")
		})
		if tree[h.ServePID] == "" {
			t.Fatalf("serve %d is not running beside its daemon: %v", h.ServePID, tree)
		}
		return holder, h, tree
	}

	t.Run("serve-killed", func(t *testing.T) { // the daemon stops with its owner, the job still open
		_, h, tree := shell(t)
		killPID(h.ServePID)
		if left := survivors(t, tree, 2*time.Second); len(left) > 0 {
			t.Fatalf("outlived their killed serve by 2 s: %v. The pty daemon must stop when its owner connection breaks, and take its shell's ping", left)
		}
	})

	t.Run("holder-killed", func(t *testing.T) { // a worker dying: its job closes and the lease ends
		holder, _, tree := shell(t)
		_ = holder.Process.Kill()
		if left := survivors(t, tree, 2*time.Second); len(left) > 0 {
			t.Fatalf("outlived the contained holder by 2 s: %v. Neither the holder's kill-on-close job nor the serve's exit took them, so witness's job does not contain OpenCode's pty daemon or its shell on Windows. This is a real finding, not a flake", left)
		}
	})

	// holder-killed passes if EITHER the job or the daemon's owner loss takes the tree. A prepared
	// handoff keeps an unowned daemon alive up to 120 s (process-daemon/README.md:27-37), so here
	// only the job can.
	t.Run("holder-killed-after-handoff", func(t *testing.T) {
		holder, h, tree := shell(t)
		code, body := h.Client(s).Do(http.MethodPost, "/api/experimental/persistent-pty/handoff", nil, true)
		var r struct{ Handoff *struct{ Ticket string } }
		if code != http.StatusOK || json.Unmarshal(body, &r) != nil || r.Handoff == nil || r.Handoff.Ticket == "" {
			t.Fatalf("POST /api/experimental/persistent-pty/handoff: %d %.500s, want a ticket", code, body)
		}
		// Control: with the serve alone gone and the job still open, the handed-off daemon and its
		// ping must live on. Otherwise owner loss still kills them, and this subtest cannot tell the
		// job's kill from the daemon's own stop. 5 s, well past the ~1.2 s an unhanded daemon took to
		// follow its serve; the handoff keeps it up to 120 s, so this costs only time.
		killPID(h.ServePID)
		time.Sleep(5 * time.Second)
		rest := map[int]string{}
		for pid, n := range tree {
			if n != "opencode" {
				rest[pid] = n
			}
		}
		alive := survivors(t, rest, 0)
		for pid, n := range rest {
			if alive[pid] == "" {
				t.Fatalf("control: %s %d died with its serve despite the handoff, so this subtest cannot isolate the job (not a containment finding)", n, pid)
			}
		}
		_ = holder.Process.Kill()
		if left := survivors(t, tree, 2*time.Second); len(left) > 0 {
			t.Fatalf("outlived the contained holder by 2 s: %v. The daemon was handed off, so only the holder's kill-on-close job could stop it, and it did not: witness's job does not contain the pty daemon Bun spawns detached, or its shell, on Windows. This is a real finding, not a flake", left)
		}
	})
}

func TestContractWindowsMCPChildNoOrphan(t *testing.T) { // T9
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	// sleeper: a local MCP server that never answers (wstub), with a child of its own (wgrand). A
	// Bun child spawned non-detached sits in libuv's kill-on-close job, which (libuv's uv_spawn,
	// from memory) also sets SILENT_BREAKAWAY_OK, so wgrand leaves that job unasked: does it leave
	// witness's too? This is the question parked from Task 3.1's review. The 10 min timeout keeps
	// OpenCode from giving up on the server mid-test (default 30 s, core/src/mcp/client.ts:41),
	// so only a kill ends it.
	cfg, err := json.Marshal(map[string]any{"mcp": map[string]any{"sleeper": map[string]any{
		"type": "local", "command": []string{stubPath(t, "wstub"), "-test.run=^$"}, "timeout": 600_000,
		"environment": map[string]string{"WITNESS_CONTRACT_STUB": "sleep", "WITNESS_CONTRACT_SLEEP_CHILD": stubPath(t, "wgrand")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	// tree makes a location request (MCP servers start with the location, asynchronously) and
	// waits for the sleeper and its child.
	tree := func(t *testing.T, c *rawServe, before map[int]string) map[int]string {
		t.Helper()
		if code, body := c.Do(http.MethodGet, atWork("/api/mcp", s), nil, true); code != http.StatusOK {
			t.Fatalf("GET /api/mcp: %d %.500s", code, body)
		}
		var procs map[int]string
		waitFor(t, 15*time.Second, "the MCP sleeper (wstub) and its child (wgrand) running", func() bool {
			procs = fresh(t, before, "wstub", "wgrand")
			return hasName(procs, "wstub") && hasName(procs, "wgrand")
		})
		return procs
	}
	kill := func(procs map[int]string) { // our own stubs, by their unique names: safe outside CI too
		for pid := range procs {
			killPID(pid)
		}
	}

	// bareLeft is what outlived the uncontained serve; nil if bare-kill did not finish. It runs
	// first (t.Run is sequential), so the contained case can say what its pass proves.
	var bareLeft map[int]string
	t.Run("bare-kill", func(t *testing.T) { // observed, not asserted: Bun's behaviour, not witness's (G13)
		before := procList(t)
		srv := startRawServe(t, oc.Bin, s, "OPENCODE_CONFIG_CONTENT="+string(cfg))
		procs := tree(t, srv, before)
		_ = srv.Cmd.Process.Kill()
		left := survivors(t, procs, 5*time.Second)
		t.Logf("T9 bare kill of an uncontained serve: of %v, %v outlived it by 5 s (if any, an upstream note, not a witness defect: windows.md §E.9)", procs, left)
		kill(left)
		bareLeft = left
	})

	t.Run("contained-holder-killed", func(t *testing.T) { // what witness relies on: the worker's job takes the tree
		before := procList(t)
		holder, h := startHolder(t, oc.Bin, s, true, false, "WITNESS_CONTRACT_HOLD_OVERLAY="+string(cfg))
		procs := tree(t, h.Client(s), before)
		_ = holder.Process.Kill()
		left := survivors(t, procs, 5*time.Second)
		kill(left)
		if bareLeft != nil && !hasName(left, "wgrand") && !hasName(bareLeft, "wgrand") {
			t.Log("T9: wgrand also died with an uncontained serve, so libuv's job held it; this pass does not show witness's job holds Bun's grandchildren")
		}
		for pid, n := range left {
			switch n {
			case "wstub":
				t.Errorf("the MCP server %d outlived the contained holder by 5 s: the holder's kill-on-close job did not take its serve's MCP child", pid)
			case "wgrand":
				t.Errorf("the MCP server's own child %d outlived the contained holder by 5 s: witness's job does not contain Bun's grandchildren on Windows (most likely it left libuv's SILENT_BREAKAWAY_OK job unasked, and witness's BREAKAWAY_OK job let it climb out too). This is a real finding, not a flake", pid)
			}
		}
	})
}

// servePID is the pid the serve reports, the opencode.exe actually serving.
func servePID(t *testing.T, c *rawServe) int {
	t.Helper()
	code, body := c.Do(http.MethodGet, "/api/info", nil, true)
	var info struct{ Pid int }
	if code != http.StatusOK || json.Unmarshal(body, &info) != nil || info.Pid == 0 {
		t.Fatalf("GET /api/info: %d %.300s", code, body)
	}
	return info.Pid
}

// fresh maps every live process named any of names (procName form) that was not in before to its
// name, from one procList; a pid reused under another name counts as new, as in noLeaks.
// hasName reports whether procs holds a process named name.
func hasName(procs map[int]string, name string) bool {
	return slices.Contains(slices.Collect(maps.Values(procs)), name)
}

// survivors polls up to d for every process in procs to exit, and returns those still alive under
// the same name.
func survivors(t *testing.T, procs map[int]string, d time.Duration) map[int]string {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		live := procList(t)
		left := map[int]string{}
		for pid, n := range procs {
			if live[pid] == n {
				left[pid] = n
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
	}
}

// userPath is HKCU\Environment's Path as stored and as expanded ("" when unset): what
// [Environment]::GetEnvironmentVariable('Path','User') reads, which expands a REG_EXPAND_SZ.
func userPath(t *testing.T) (raw, expanded string) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf(`open HKCU\Environment: %v`, err)
	}
	defer k.Close()
	raw, typ, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", ""
	}
	if err != nil {
		t.Fatalf(`read HKCU\Environment Path: %v`, err)
	}
	if expanded = raw; typ == registry.EXPAND_SZ {
		if expanded, err = registry.ExpandString(raw); err != nil {
			t.Fatalf("expand the user PATH: %v", err)
		}
	}
	return raw, expanded
}
