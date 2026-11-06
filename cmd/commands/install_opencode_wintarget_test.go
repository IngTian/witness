package commands

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	opencodeplugin "github.com/IngTian/witness/internal/platform/opencode/plugin"
)

// Issue #10. `witness wire opencode` baked the bash shim into the plugin unconditionally
// (cmdInstallOpenCode called repoShim() directly, with no GOOS split). Windows has no
// guaranteed shell to run a .sh, so the plugin's Bun.spawn silently failed and NOTHING was
// ever captured on Windows — while the command reported success.
//
// These tests are platform-INDEPENDENT on purpose: they exercise the generators with a
// Windows-shaped path so the properties are verified on the maintainer's macOS machine and in
// Linux CI, neither of which can run resolveOpenCodeInstall's Windows branch.

// Both generators must carry every target shape through unchanged: the Unix shim, a Windows exe
// (`\U` and `\w` are escape sequences in a JS string literal, so hand-concatenating the path would
// corrupt it — Source uses json.Marshal), and a Windows path with SPACES (ordinary in
// %LOCALAPPDATA% for a username with a space; the plugin passes argv and the MCP config a JSON
// array, so nothing may quote or split it).
func TestOpenCodeGeneratorsCarryEveryTargetPath(t *testing.T) {
	for _, target := range []string{
		"/repo/hooks/witness.sh",
		`C:\Users\zetian\AppData\Local\witness\witness.exe`,
		`C:\Users\Zeying Tian\AppData\Local\witness\witness.exe`,
	} {
		// Decode the plugin literal the way a JS engine would (JSON string syntax is a subset).
		baked := ""
		for _, l := range strings.Split(opencodeplugin.Source(target), "\n") {
			if q, ok := strings.CutPrefix(l, "globalThis.WITNESS_SHIM = "); ok {
				if err := json.Unmarshal([]byte(q), &baked); err != nil {
					t.Fatalf("the baked value is not a parseable JS string: %v (%s)", err, l)
				}
			}
		}
		if baked != target {
			t.Errorf("the plugin does not round-trip the path:\n got  %q\n want %q", baked, target)
		}

		out, err := mergeOpenCodeMCP(nil, target)
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]any
		if err := json.Unmarshal(out, &root); err != nil {
			t.Fatalf("generated config is not valid JSON: %v", err)
		}
		entry := root["mcp"].(map[string]any)["witness"].(map[string]any)
		// An argv ARRAY [exe, "mcp"], never one flattened (quoted) string.
		if cmd, _ := entry["command"].([]any); len(cmd) != 2 || cmd[0] != target || cmd[1] != "mcp" {
			t.Errorf("MCP command must be [%q, \"mcp\"], got %v", target, entry["command"])
		}
		if entry["type"] != "local" || entry["enabled"] != true {
			t.Errorf("MCP entry shape regressed: %v", entry)
		}
	}
}

// The plugin spawns an argv ARRAY, which is what makes the exe usable with no shell. If this
// ever became a single command STRING, a Windows path with spaces ("C:\Program Files\…")
// would word-split and capture would break — the exact class of bug this issue is about.
func TestOpenCodePluginSpawnsAnArgvArrayNotAShellString(t *testing.T) {
	body := opencodeplugin.Source("")
	if !strings.Contains(body, "Bun.spawn([WITNESS_BIN, ...args]") {
		t.Error("the plugin must spawn an argv array [target, ...args]: a command string would " +
			"require a shell (absent on Windows) and would word-split a path with spaces")
	}
	// And it must not route through a shell explicitly.
	for _, bad := range []string{"cmd.exe", "/bin/sh", "shell: true", "powershell"} {
		if strings.Contains(body, bad) {
			t.Errorf("the plugin references %q; it must spawn the target directly", bad)
		}
	}
}

// resolveOpenCodeInstall must exist on BOTH platforms and return a path that the plugin can
// spawn. On Unix it is the repo shim; asserting the shape here (rather than the value) keeps
// the test meaningful on either OS.
//
// LOCALAPPDATA IS REDIRECTED FIRST, and that is not hygiene — without it this test DAMAGES a
// Windows machine. On Windows resolveOpenCodeInstall is `return installSelf()`, whose
// installBundle copies os.Executable() to <LOCALAPPDATA>\witness\witness.exe BEFORE it
// validates anything (install_windows.go: MkdirAll then copyFile, then probeSrcTree). Under
// `go test` os.Executable() is the TEST binary, and the os.SameFile short-circuit in copyFile
// compares different directories so it does not fire. So running the suite on Windows would
// overwrite the user's installed witness.exe with a test binary, and also put a temp dir on
// their PATH. Same class as the test that once spawned 366 real `claude` children: a test must
// never mutate the machine it runs on. installRoot() reads LOCALAPPDATA from the environment,
// so a TempDir here is enough to contain it.
func TestResolveOpenCodeInstallReturnsASpawnableTarget(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())

	// On Unix, resolveOpenCodeInstall -> repoShim derives the shim from os.Executable(), which
	// under `go test` is a binary in a build temp dir — so it can NEVER find hooks/witness.sh and
	// this test used to skip on every Unix run. That mattered: combined with the Windows damage
	// above, the test only ever executed on the one platform where it was harmful. Staging a shim
	// beside a fake executable is not possible without an os.Executable seam, so instead assert
	// the CONTRACT that is checkable here — the error must be the documented "not a built working
	// copy" one, not any other failure — and let the shape assertions below run on Windows.
	target, err := resolveOpenCodeInstall()
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Fatalf("resolveOpenCodeInstall: %v", err)
		}
		if !strings.Contains(err.Error(), "shim not found") {
			t.Fatalf("on Unix the ONLY acceptable failure is the missing-shim contract (a test binary "+
				"lives in a build temp dir, so repoShim cannot resolve); got a different error, which "+
				"means the Unix path changed shape: %v", err)
		}
		// The message must stay actionable — it is what a user sees when they run from a
		// non-built checkout.
		if !strings.Contains(err.Error(), "make build") {
			t.Errorf("the missing-shim error must tell the user how to fix it; got %v", err)
		}
		return
	}
	if !filepath.IsAbs(target) {
		t.Errorf("the plugin bakes in an ABSOLUTE path (it spawns from OpenCode's cwd), got %q", target)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(target), ".exe") {
		t.Errorf("on Windows the target must be the installed witness.exe, got %q", target)
	}
	if runtime.GOOS != "windows" && !strings.HasSuffix(target, ".sh") {
		t.Errorf("on Unix the target stays the shim, got %q", target)
	}
}

// A SOURCE guard for the Windows branch, because nothing else can catch this off-Windows.
//
// The original #10 bug compiled cleanly for GOOS=windows and passed `go vet` — it was a
// semantic mistake (returning the bash shim where no shell exists), not a type error. And no
// behavioral test reaches the Windows branch off Windows, so on the maintainer's macOS and in
// Linux CI it can never fail. Without this, reverting the fix would go completely undetected by
// the whole suite until someone ran it on real Windows.
//
// So: read install_windows.go and require that each of its resolvers self-installs and does NOT
// hand back the shim — Claude's included, the oldest Windows path. Asserting on source is the
// weakest kind of test, and it is used here precisely because the strong kind is unavailable on
// this platform.
func TestWindowsResolversSelfInstallAndNeverReturnTheShim(t *testing.T) {
	for _, tc := range []struct{ sig, form, why string }{
		{"func resolveClaudeInstall()", "execInvocation(",
			"a Windows Claude Code hook runs in exec form with no shell, so a shell-form hook never runs"},
		{"func resolveOpenCodeInstall()", "", ""},
		{"func resolveCodexInstall()", "powerShell: true",
			"without it the hook is written in the POSIX `command` field, which Codex will not use on Windows"},
	} {
		fn := funcSource(t, "install_windows.go", tc.sig)
		if strings.Contains(fn, "repoShim()") {
			t.Errorf("%s (Windows) returns the bash shim: Windows has no guaranteed shell to run "+
				"witness.sh, so NOTHING is captured — while `wire` still reports success (issue #10)", tc.sig)
		}
		if !strings.Contains(fn, "installSelf()") {
			t.Errorf("%s (Windows) must self-install and return the installed witness.exe: the "+
				"wiring holds an absolute path, so it cannot point at a build checkout", tc.sig)
		}
		if tc.form != "" && !strings.Contains(fn, tc.form) {
			t.Errorf("%s (Windows) must use %s: %s", tc.sig, tc.form, tc.why)
		}
	}
	// And the integrations must share one install sequence, so they cannot drift.
	if !strings.Contains(readSource(t, "install_windows.go"), "func installSelf()") {
		t.Error("installSelf is missing; every integration must share one install sequence")
	}
}

// (TestUnixOpenCodeStillBakesTheShim used to live here. Its single assertion — that Source()
// bakes "/repo/hooks/witness.sh" verbatim — is asserted IDENTICALLY, with additional checks, by
// TestOpenCodePluginSourceBakesShim in install_test.go, which is NOT GOOS-gated. This copy was
// skipped on Windows and redundant everywhere else, so it protected nothing that the ungated
// test does not already protect.)
