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

const winExe = `C:\Users\zetian\AppData\Local\witness\witness.exe`

// The plugin must bake in a Windows exe path that JavaScript parses back to the ORIGINAL
// string. This is the trap: a Windows path is full of backslashes, and `\U` / `\w` are
// escape sequences in a JS string literal, so hand-concatenating the path would corrupt it
// (or fail to parse). Source() uses json.Marshal, which escapes them — this pins that.
func TestOpenCodePluginSourceBakesWindowsExePathSafely(t *testing.T) {
	src := opencodeplugin.Source(winExe)

	line := ""
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, "globalThis.WITNESS_SHIM") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("no globalThis.WITNESS_SHIM assignment found in the generated plugin")
	}
	// Backslashes MUST be escaped in the emitted literal, or JS mis-parses the path.
	if strings.Contains(line, `\U`) && !strings.Contains(line, `\\U`) {
		t.Errorf("the Windows path was emitted with an unescaped backslash: %s", line)
	}

	// Decode the literal the way a JS engine would (JSON string syntax is a subset) and
	// require it to round-trip to the exact path.
	quoted := strings.TrimSuffix(strings.TrimPrefix(line, "globalThis.WITNESS_SHIM = "), ";")
	var got string
	if err := json.Unmarshal([]byte(strings.TrimSpace(quoted)), &got); err != nil {
		t.Fatalf("the baked value is not a parseable JS/JSON string: %v (%s)", err, line)
	}
	if got != winExe {
		t.Errorf("the baked path does not round-trip:\n got  %q\n want %q", got, winExe)
	}
}

// The plugin spawns an argv ARRAY, which is what makes the exe usable with no shell. If this
// ever became a single command STRING, a Windows path with spaces ("C:\Program Files\…")
// would word-split and capture would break — the exact class of bug this issue is about.
func TestOpenCodePluginSpawnsAnArgvArrayNotAShellString(t *testing.T) {
	body := opencodeplugin.Body()
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

// A Windows exe path with SPACES must survive both generators. Spaces are ordinary in
// %LOCALAPPDATA% paths on Windows (a username with a space), and nothing here may quote or
// split them — the plugin passes argv, and the MCP config passes a JSON array.
func TestWindowsPathWithSpacesSurvivesBothGenerators(t *testing.T) {
	spaced := `C:\Users\Zeying Tian\AppData\Local\witness\witness.exe`

	src := opencodeplugin.Source(spaced)
	var baked string
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, "globalThis.WITNESS_SHIM") {
			q := strings.TrimSpace(strings.TrimPrefix(l, "globalThis.WITNESS_SHIM = "))
			if err := json.Unmarshal([]byte(q), &baked); err != nil {
				t.Fatalf("plugin literal unparseable: %v", err)
			}
		}
	}
	if baked != spaced {
		t.Errorf("plugin mangled a spaced path:\n got  %q\n want %q", baked, spaced)
	}

	out, err := mergeOpenCodeMCP(nil, spaced)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	cmd := root["mcp"].(map[string]any)["witness"].(map[string]any)["command"].([]any)
	if cmd[0].(string) != spaced {
		t.Errorf("MCP config mangled a spaced path:\n got  %q\n want %q", cmd[0], spaced)
	}
	// It must stay an ARRAY (argv), not be flattened into a quoted string.
	if len(cmd) != 2 || cmd[1].(string) != "mcp" {
		t.Errorf("MCP command must be the argv array [exe, \"mcp\"], got %v", cmd)
	}
}

// The MCP entry must invoke the same Windows exe, JSON-encoded.
func TestMergeOpenCodeMCPWithWindowsExe(t *testing.T) {
	out, err := mergeOpenCodeMCP(nil, winExe)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v", err)
	}
	entry := root["mcp"].(map[string]any)["witness"].(map[string]any)
	cmd := entry["command"].([]any)
	if cmd[0].(string) != winExe {
		t.Errorf("command[0] = %q, want the installed exe %q", cmd[0], winExe)
	}
	if entry["type"] != "local" || entry["enabled"] != true {
		t.Errorf("MCP entry shape regressed: %v", entry)
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
// semantic mistake (returning the bash shim where no shell exists), not a type error. And the
// behavioral assertion above is runtime.GOOS-gated, so on the maintainer's macOS and in Linux
// CI it can never fail. Without this, reverting the fix would go completely undetected by the
// whole suite until someone ran it on real Windows.
//
// So: read install_windows.go and require that its resolveOpenCodeInstall does NOT hand back
// the shim. Asserting on source is the weakest kind of test, and it is used here precisely
// because the strong kind is unavailable on this platform.
func TestWindowsResolverDoesNotReturnTheBashShim(t *testing.T) {
	src := readSource(t, "install_windows.go")
	i := strings.Index(src, "func resolveOpenCodeInstall(")
	if i < 0 {
		t.Fatal("resolveOpenCodeInstall not found in install_windows.go — issue #10 would regress " +
			"to the shim path with no shell to run it")
	}
	fn := src[i:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	if strings.Contains(fn, "repoShim()") {
		t.Error("the Windows OpenCode resolver returns the bash shim: Windows has no guaranteed " +
			"shell to run witness.sh, so the plugin's Bun.spawn fails and NOTHING is captured " +
			"— while `wire opencode` still reports success (issue #10)")
	}
	if !strings.Contains(fn, "installSelf()") {
		t.Error("the Windows resolver must self-install and return the installed witness.exe; " +
			"the plugin bakes in an absolute path, so it cannot point at a build checkout")
	}
	// And the shared install path must still be what Claude uses, so the two cannot drift.
	if !strings.Contains(src, "func installSelf()") {
		t.Error("installSelf is missing; both integrations must share one install sequence")
	}
}

// (TestUnixOpenCodeStillBakesTheShim used to live here. Its single assertion — that Source()
// bakes "/repo/hooks/witness.sh" verbatim — is asserted IDENTICALLY, with additional checks, by
// TestOpenCodePluginSourceBakesShim in install_test.go, which is NOT GOOS-gated. This copy was
// skipped on Windows and redundant everywhere else, so it protected nothing that the ungated
// test does not already protect.)
