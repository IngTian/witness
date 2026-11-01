package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/platform/codex"
	"github.com/IngTian/witness/internal/store"
)

// Tests for `witness wire codex` (issue #179).
//
// SCOPE, stated plainly because it bounds what these prove: no Codex is installed on
// the machine that wrote this, so nothing here verifies that Codex READS hooks.json or
// FIRES these events. That is spec-derived from the official hooks guide and needs a
// real Codex run to confirm. What IS verified below is everything on witness's side of
// the boundary — the document we write, its idempotency, its ownership discipline, and
// the Windows form — because those are where the silent failures the issue is about
// actually live.

const unixShim = "/repo/hooks/witness.sh"

// codexCommands returns the command / commandWindows strings for one event.
func codexCommands(t *testing.T, data []byte, event string) []string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	hooks, _ := root["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	var out []string
	for _, e := range entries {
		m, _ := e.(map[string]any)
		hs, _ := m["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			for _, key := range []string{"command", "commandWindows"} {
				if c, ok := hm[key].(string); ok {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func TestCodexMergeWritesEveryLifecycleEvent(t *testing.T) {
	out, err := mergeHooks([]byte(`{}`), codexHookSpecs(codexHook{program: unixShim}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range codexHookEvents {
		cmds := codexCommands(t, out, event)
		if len(cmds) != 1 {
			t.Fatalf("%s: got %d commands, want 1: %v", event, len(cmds), cmds)
		}
		if want := "'" + unixShim + "' " + codexHookArgs; cmds[0] != want {
			t.Errorf("%s command = %q, want %q", event, cmds[0], want)
		}
	}
	// Every handler must be async. Codex's SessionEnd timeout defaults to ONE second
	// (capped at three), so a synchronous hook would be killed mid-import on exactly the
	// large archives that need reconciling most — and it would also violate witness's
	// standing rule that capture never blocks a session.
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, event := range codexHookEvents {
		entries, _ := hooks[event].([]any)
		for _, e := range entries {
			m, _ := e.(map[string]any)
			hs, _ := m["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if async, _ := hm["async"].(bool); !async {
					t.Errorf("%s handler is not async: %v", event, hm)
				}
			}
		}
	}
}

// Re-wiring must produce BYTE-IDENTICAL output, and cmdInstallCodex must then skip the
// write. This is not tidiness: Codex records hook trust against the hook's hash, so a
// rewrite that changed so much as key order would revoke the user's approval and leave
// capture inert with no error reported anywhere.
func TestCodexMergeIsByteIdenticalOnRewire(t *testing.T) {
	specs := codexHookSpecs(codexHook{program: unixShim})
	first, err := mergeHooks([]byte(`{}`), specs, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mergeHooks(first, specs, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("re-wiring changed the bytes, which would revoke Codex hook trust:\n--- first\n%s\n--- second\n%s", first, second)
	}
	for _, event := range codexHookEvents {
		if cmds := codexCommands(t, second, event); len(cmds) != 1 {
			t.Errorf("%s: re-wire left %d commands, want 1 (duplicate hooks fire twice): %v", event, len(cmds), cmds)
		}
	}
}

func TestCodexMergePreservesForeignHooksAndKeys(t *testing.T) {
	in := `{
  "description": "my hooks",
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "/usr/local/bin/notify-me"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "/opt/audit.sh"}]}]
  }
}`
	out, err := mergeHooks([]byte(in), codexHookSpecs(codexHook{program: unixShim}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["description"] != "my hooks" {
		t.Errorf("a sibling top-level key was dropped: %v", root["description"])
	}
	stop := codexCommands(t, out, "Stop")
	if len(stop) != 2 {
		t.Fatalf("Stop should hold the foreign hook AND ours, got %v", stop)
	}
	if !strings.Contains(strings.Join(stop, " "), "/usr/local/bin/notify-me") {
		t.Errorf("foreign Stop hook was clobbered: %v", stop)
	}
	if pre := codexCommands(t, out, "PreToolUse"); len(pre) != 1 || !strings.Contains(pre[0], "/opt/audit.sh") {
		t.Errorf("an event witness does not use was disturbed: %v", pre)
	}
}

func TestCodexRemoveStripsOnlyOurs(t *testing.T) {
	merged, err := mergeHooks([]byte(`{
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/keep-me"}]}]}
}`), codexHookSpecs(codexHook{program: unixShim}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	cleaned, err := removeHooks(merged, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	stop := codexCommands(t, cleaned, "Stop")
	if len(stop) != 1 || !strings.Contains(stop[0], "/opt/keep-me") {
		t.Fatalf("unwire should leave exactly the foreign hook, got %v", stop)
	}
	for _, event := range []string{"SessionStart", "SessionEnd"} {
		if cmds := codexCommands(t, cleaned, event); len(cmds) != 0 {
			t.Errorf("%s still wired after unwire: %v", event, cmds)
		}
	}
}

// Unwiring Codex must not touch Claude's hooks, and vice versa. The two venues share
// mergeHooks/removeHooks, so the ownership predicate is the ONLY thing keeping them
// apart — a single shared predicate would make each venue's uninstall strip the other.
func TestCodexAndClaudePredicatesDoNotOverlap(t *testing.T) {
	claudeDoc, err := mergeWitnessHooks([]byte(`{}`), shellInvocation(unixShim))
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := removeHooks(claudeDoc, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	if cmds := eventCommands(t, stripped, "Stop"); len(cmds) == 0 {
		t.Error("unwiring codex removed a CLAUDE hook")
	}

	codexDoc, err := mergeHooks([]byte(`{}`), codexHookSpecs(codexHook{program: unixShim}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	stripped, err = removeHooks(codexDoc, isWitnessEntry)
	if err != nil {
		t.Fatal(err)
	}
	if cmds := codexCommands(t, stripped, "Stop"); len(cmds) == 0 {
		t.Error("unwiring claude removed a CODEX hook")
	}
}

// The pre-#179 docs told users to hand-write an UNQUOTED absolute path. Wiring must
// ADOPT that entry (replace it) rather than append beside it — otherwise everyone who
// followed the old instructions gets two hooks and every turn imports twice.
func TestCodexAdoptsHandEditedEntryFromTheOldDocs(t *testing.T) {
	handEdited := `{
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "/old/path/to/witness import --agent codex --hook"}]}]
  }
}`
	out, err := mergeHooks([]byte(handEdited), codexHookSpecs(codexHook{program: unixShim}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	cmds := codexCommands(t, out, "Stop")
	if len(cmds) != 1 {
		t.Fatalf("hand-edited entry was duplicated instead of adopted: %v", cmds)
	}
	if strings.Contains(cmds[0], "/old/path") {
		t.Errorf("the stale path survived: %v", cmds)
	}
}

func TestIsCodexWitnessCommandIsNarrow(t *testing.T) {
	ours := []string{
		"'" + unixShim + "' " + codexHookArgs,
		`'/Users/me/My Projects/witness.sh' ` + codexHookArgs,
		`& 'C:\Program Files\witness\witness.exe' ` + codexHookArgs,
		`& 'C:\Users\O''Brien\witness.exe' ` + codexHookArgs,    // PowerShell '' escape
		`'/Users/o'\''brien/hooks/witness.sh' ` + codexHookArgs, // POSIX '\'' escape
		"/absolute/path/to/witness " + codexHookArgs,            // the old docs' unquoted form
		"  '" + unixShim + "' " + codexHookArgs + "  ",          // surrounding whitespace
	}
	for _, c := range ours {
		if !isCodexWitnessCommand(c) {
			t.Errorf("failed to recognize our own hook — a re-wire would duplicate it: %q", c)
		}
	}
	// Foreign commands we are contractually required to preserve. Each is one half of
	// the signal: the right arguments with the wrong program, or the right program with
	// the wrong arguments.
	foreign := []string{
		"/opt/notwitness.sh " + codexHookArgs,
		"/opt/witness-wrapper " + codexHookArgs,
		"/usr/bin/my-witness-shim.sh " + codexHookArgs,
		"'" + unixShim + "' capture",
		"'" + unixShim + "' import --agent opencode --hook",
		"'" + unixShim + "' " + codexHookArgs + " --extra",
		"/usr/local/bin/witness --version",
		// A witness invocation with NO arguments. These are the cases that prove the
		// ARG-TAIL half of the check is load-bearing: drop it and the predicate claims
		// any bare witness command, including a user's own `witness distill` cron hook.
		// Found by mutation testing — without them, deleting the suffix check left the
		// suite green.
		"/usr/local/bin/witness",
		"'" + unixShim + "'",
		unixShim,
		`& 'C:\witness\witness.exe'`,
		"",
	}
	for _, c := range foreign {
		if isCodexWitnessCommand(c) {
			t.Errorf("claimed a foreign hook as ours — unwire would delete someone else's hook: %q", c)
		}
	}
}

func TestPSQuoteDoublesEmbeddedQuote(t *testing.T) {
	for in, want := range map[string]string{
		`C:\witness.exe`:           `'C:\witness.exe'`,
		`C:\O'Brien\witness.exe`:   `'C:\O''Brien\witness.exe'`,
		`C:\a b\witness.exe`:       `'C:\a b\witness.exe'`,
		`C:\'' quoted\witness.exe`: `'C:\'''' quoted\witness.exe'`,
	} {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Windows form must use commandWindows with the PowerShell call operator — NOT the
// `command` field, and NOT Claude's exec-form args, neither of which Codex would run.
// Asserted on EVERY OS, because a runtime.GOOS=="windows" check can only fail when the
// suite is run on Windows, which is how issue #10 shipped a .sh to Windows users while
// reporting success.
func TestCodexWindowsFormUsesPowerShellCommandWindows(t *testing.T) {
	const exe = `C:\Users\me\AppData\Local\witness\witness.exe`
	out, err := mergeHooks([]byte(`{}`), codexHookSpecs(codexHook{program: exe, powerShell: true}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, event := range codexHookEvents {
		entries, _ := hooks[event].([]any)
		if len(entries) != 1 {
			t.Fatalf("%s: want 1 entry, got %d", event, len(entries))
		}
		m, _ := entries[0].(map[string]any)
		hs, _ := m["hooks"].([]any)
		hm, _ := hs[0].(map[string]any)

		if _, present := hm["command"]; present {
			t.Errorf("%s: wrote `command` on the Windows path; Codex would run the POSIX form", event)
		}
		if _, present := hm["args"]; present {
			t.Errorf("%s: wrote Claude's exec-form `args`, which Codex does not support", event)
		}
		cw, _ := hm["commandWindows"].(string)
		if !strings.HasPrefix(cw, "& '") {
			t.Errorf("%s: commandWindows = %q, want the PowerShell call operator (a quoted path will not run without it)", event, cw)
		}
		if !strings.Contains(cw, exe) {
			t.Errorf("%s: commandWindows = %q, want the installed exe path", event, cw)
		}
		if !isCodexWitnessCommand(cw) {
			t.Errorf("%s: we cannot recognize the Windows hook we just wrote, so a re-wire on Windows would duplicate it: %q", event, cw)
		}
	}
	// The JSON encoder must escape the backslashes; a hand-built string would not.
	if !strings.Contains(string(out), `\\`) {
		t.Error("the Windows path was not JSON-escaped, so hooks.json would be invalid or point somewhere else")
	}
}

// Re-wiring on WINDOWS must dedupe too, and a machine that moved between platforms must
// not end up with both forms wired.
//
// Found by mutation testing: isCodexEntry inspects both `command` and `commandWindows`,
// and dropping `commandWindows` left the whole suite green — the Windows shape test
// called isCodexWitnessCommand directly and so never exercised entry-level detection.
// The consequence is the quiet kind: every re-wire on Windows appends another handler,
// and each one re-imports the session on every single turn.
func TestCodexWindowsFormDedupesAndMigrates(t *testing.T) {
	const exe = `C:\witness\witness.exe`
	win := codexHookSpecs(codexHook{program: exe, powerShell: true})

	first, err := mergeHooks([]byte(`{}`), win, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mergeHooks(first, win, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("re-wiring on Windows changed the bytes:\n%s\n---\n%s", first, second)
	}
	for _, event := range codexHookEvents {
		if cmds := codexCommands(t, second, event); len(cmds) != 1 {
			t.Errorf("%s: re-wire on Windows left %d handlers, want 1: %v", event, len(cmds), cmds)
		}
	}

	// Unix form over a Windows-wired file, and back: exactly one handler either way.
	migrated, err := mergeHooks(first, codexHookSpecs(codexHook{program: unixShim}), isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range codexHookEvents {
		cmds := codexCommands(t, migrated, event)
		if len(cmds) != 1 {
			t.Fatalf("%s: migrating Windows->Unix orphaned a handler: %v", event, cmds)
		}
		if strings.Contains(cmds[0], exe) {
			t.Errorf("%s: the Windows handler survived the migration: %v", event, cmds)
		}
	}
	back, err := mergeHooks(migrated, win, isCodexEntry)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range codexHookEvents {
		if cmds := codexCommands(t, back, event); len(cmds) != 1 {
			t.Errorf("%s: migrating Unix->Windows orphaned a handler: %v", event, cmds)
		}
	}
}

// resolveCodexInstall on Windows must point at the INSTALLED exe, never the bash shim.
// A source assertion for the same reason install_opencode_wintarget_test.go is one: the
// Windows branch is behind a build tag, so no test running on macOS/Linux — i.e. CI —
// can execute it.
func TestCodexWindowsTargetIsTheInstalledExeNotTheShim(t *testing.T) {
	src, err := os.ReadFile("install_windows.go")
	if err != nil {
		t.Fatalf("read install_windows.go: %v", err)
	}
	body, ok := funcBody(string(src), "func resolveCodexInstall()")
	if !ok {
		t.Fatal("resolveCodexInstall is missing from install_windows.go — Windows Codex wiring would not compile or would fall back to the Unix path")
	}
	if strings.Contains(body, "repoShim()") {
		t.Error("resolveCodexInstall (Windows) calls repoShim(): Windows has no guaranteed shell, so the hook would never run and capture would fail silently (issue #10)")
	}
	if !strings.Contains(body, "installSelf()") {
		t.Error("resolveCodexInstall (Windows) must installSelf(): hooks.json holds an absolute path, so it must point at a stable install, not a build checkout")
	}
	if !strings.Contains(body, "powerShell: true") {
		t.Error("resolveCodexInstall (Windows) must set powerShell: true, or it writes the POSIX `command` field that Codex will not use on Windows")
	}
}

// funcBody returns the text of the named function, from its signature to the first
// line that is exactly "}".
func funcBody(src, signature string) (string, bool) {
	i := strings.Index(src, signature)
	if i < 0 {
		return "", false
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j], true
	}
	return rest, true
}

// The installer must resolve the Codex home through the importer's own function. If the
// two ever disagreed, witness would wire hooks into one Codex home and import
// conversations from another — and the symptom is an archive that silently stays empty.
func TestCodexHooksPathFollowsTheImporterHome(t *testing.T) {
	for _, key := range []string{"WITNESS_CODEX_HOME", "CODEX_HOME"} {
		dir := t.TempDir()
		t.Setenv("WITNESS_CODEX_HOME", "")
		t.Setenv("CODEX_HOME", "")
		t.Setenv(key, dir)

		home, err := codex.Home()
		if err != nil {
			t.Fatalf("codex.Home: %v", err)
		}
		got, err := codexHooksPath()
		if err != nil {
			t.Fatalf("codexHooksPath: %v", err)
		}
		if want := filepath.Join(home, "hooks.json"); got != want {
			t.Errorf("%s: codexHooksPath = %q, want %q", key, got, want)
		}
		if !strings.HasPrefix(got, dir) {
			t.Errorf("%s=%q was ignored; got %q", key, dir, got)
		}
	}
	// WITNESS_CODEX_HOME wins, matching the importer's precedence.
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("WITNESS_CODEX_HOME", a)
	t.Setenv("CODEX_HOME", b)
	got, err := codexHooksPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, a) {
		t.Errorf("WITNESS_CODEX_HOME must win over CODEX_HOME; got %q", got)
	}
}

// The full round trip through disk: create, re-wire (no-op), unwire. This is the path a
// user actually runs, and it covers what the pure merge tests cannot — MkdirAll, the
// atomic write, and the trust-preserving skip.
func TestCodexWireRoundTripOnDisk(t *testing.T) {
	// A nested path that does not exist yet: a fresh machine has no ~/.codex.
	path := filepath.Join(t.TempDir(), "nested", "hooks.json")
	h := codexHook{program: unixShim}

	changed, err := wireCodexHooks(path, h)
	if err != nil {
		t.Fatalf("first wire: %v", err)
	}
	if !changed {
		t.Error("first wire reported no change, but the file did not exist")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("hooks.json was not created: %v", err)
	}
	for _, event := range codexHookEvents {
		if cmds := codexCommands(t, first, event); len(cmds) != 1 {
			t.Errorf("%s: %v", event, cmds)
		}
	}

	// Re-wiring an already-correct file must report NO change and leave the bytes
	// untouched, or Codex's hash-based hook trust is revoked and capture goes inert.
	changed, err = wireCodexHooks(path, h)
	if err != nil {
		t.Fatalf("re-wire: %v", err)
	}
	if changed {
		t.Error("re-wiring an unchanged file reported a write; that revokes Codex hook trust")
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Errorf("re-wire rewrote the file:\n%s\n---\n%s", first, again)
	}

	changed, err = unwireCodexHooks(path)
	if err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if !changed {
		t.Error("unwire reported no change but hooks were wired")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), codexHookArgs) {
		t.Errorf("witness hooks survived unwire: %s", after)
	}
	// Unwiring twice is a no-op, not an error.
	if changed, err := unwireCodexHooks(path); err != nil || changed {
		t.Errorf("second unwire: changed=%v err=%v, want false/nil", changed, err)
	}
}

// A hooks.json witness cannot parse must FAIL, not be silently replaced. Overwriting it
// would destroy hooks the user wrote by hand, and reporting success while leaving it
// alone is how cmdUninstallClaude once told users they were unwired when they were not.
func TestCodexRefusesToClobberUnparseableHooksFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	const garbage = `{"hooks": {"Stop": [ truncated`
	if err := os.WriteFile(path, []byte(garbage), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wireCodexHooks(path, codexHook{program: unixShim}); err == nil {
		t.Error("wiring over an unparseable hooks.json must fail")
	}
	if _, err := unwireCodexHooks(path); err == nil {
		t.Error("unwiring from an unparseable hooks.json must fail")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != garbage {
		t.Errorf("the user's file was modified despite the failure:\n%s", got)
	}
}

func TestCodexIsARegisteredInstallTarget(t *testing.T) {
	if _, ok := platform.InstallerFor("codex"); !ok {
		t.Fatal("codex is not registered as an install target, so `witness wire codex` cannot dispatch")
	}
	for _, name := range platform.InstallTargets() {
		if !strings.Contains(newWireCmd().Use, name) {
			t.Errorf("wire help omits registered target %q", name)
		}
	}
}

// fakeCaptureOnlyInstaller stands in for a venue that captures but runs no model.
type fakeCaptureOnlyInstaller struct{ called *bool }

func (f fakeCaptureOnlyInstaller) Install() error   { *f.called = true; return nil }
func (f fakeCaptureOnlyInstaller) Uninstall() error { return nil }

// Wiring a capture-only venue must NOT bind it as the distillation runner.
//
// AGENTS.md: "A capture source need not provide a distillation runner." Codex is the
// first one that does not, and bindRunner used to run unconditionally. SetRunner does no
// validation, so it would have written `runner = codex` into config.toml — a value
// RunnerForName cannot resolve, which fails every subsequent drain and silently stops
// the archive distilling. Strictly worse than leaving the runner alone.
//
// Driven through a registered fake rather than "codex" because the real codex installer
// resolves a hook target that does not exist under `go test`; the gate being tested is
// the same one, and it is registry-derived, so the fake exercises it faithfully.
func TestWiringACaptureOnlyVenueDoesNotBindItAsRunner(t *testing.T) {
	const venue = "fakecaptureonly"
	if err := platform.ValidateRunnerName(venue); err == nil {
		t.Fatalf("precondition: %q must not be a valid runner, or this test proves nothing", venue)
	}
	called := false
	platform.RegisterInstaller(venue, fakeCaptureOnlyInstaller{&called})

	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	if err := cmdInstall([]string{venue}); err != nil {
		t.Fatalf("wire %s: %v", venue, err)
	}
	if !called {
		t.Fatal("the installer never ran, so nothing downstream was exercised")
	}

	st, err := store.Open()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if got := st.LoadConfig().Runner; got == venue {
		t.Errorf("wiring a capture-only venue bound runner=%q; the engine cannot resolve it, so every later drain fails and distillation silently stops", got)
	}
}
