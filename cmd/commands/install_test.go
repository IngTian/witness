package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	opencodeplugin "github.com/IngTian/witness/internal/platform/opencode/plugin"
)

// helper: how many witness entries does event have, and is `other` preserved?
func eventCommands(t *testing.T, data []byte, event string) []string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	hooks, _ := root["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	var cmds []string
	for _, e := range entries {
		m, _ := e.(map[string]any)
		hs, _ := m["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, ok := hm["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func countWitness(cmds []string) int {
	n := 0
	for _, c := range cmds {
		if strings.Contains(c, "witness.sh") {
			n++
		}
	}
	return n
}

func TestMergeAddsAllFourWitnessHooks(t *testing.T) {
	out, err := mergeWitnessHooks([]byte(`{}`), shellInvocation("/repo/hooks/witness.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if countWitness(eventCommands(t, out, ev)) != 1 {
			t.Errorf("%s: want exactly 1 witness hook", ev)
		}
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	out, _ := mergeWitnessHooks([]byte(`{}`), shellInvocation("/repo/hooks/witness.sh"))
	out2, _ := mergeWitnessHooks(out, shellInvocation("/repo/hooks/witness.sh")) // re-install
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if got := countWitness(eventCommands(t, out2, ev)); got != 1 {
			t.Errorf("%s: re-install should not duplicate, got %d", ev, got)
		}
	}
}

func TestMergePreservesForeignHooksAndSettings(t *testing.T) {
	in := `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"prettier --write"}]}]}}`
	out, err := mergeWitnessHooks([]byte(in), shellInvocation("/repo/hooks/witness.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	json.Unmarshal(out, &root)
	if root["model"] != "opus" {
		t.Errorf("other settings clobbered")
	}
	cmds := eventCommands(t, out, "Stop")
	if countWitness(cmds) != 1 {
		t.Errorf("Stop should have witness hook")
	}
	foundPrettier := false
	for _, c := range cmds {
		if strings.Contains(c, "prettier") {
			foundPrettier = true
		}
	}
	if !foundPrettier {
		t.Errorf("foreign Stop hook must be preserved, got %v", cmds)
	}
}

// --- Windows exec-form hooks (guarded on any OS since the merge logic is
// platform-independent; only resolveClaudeInstall is GOOS-split) --------------

// execCommands returns (command, args, isWitness) for each hook in an event, so
// exec-form assertions can inspect both the exe path and the arg token.
func execCommands(t *testing.T, data []byte, event string) []struct {
	Command string
	Args    []string
} {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	hooks, _ := root["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	var out []struct {
		Command string
		Args    []string
	}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		hs, _ := m["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			var args []string
			if raw, ok := hm["args"].([]any); ok {
				for _, a := range raw {
					if s, ok := a.(string); ok {
						args = append(args, s)
					}
				}
			}
			out = append(out, struct {
				Command string
				Args    []string
			}{cmd, args})
		}
	}
	return out
}

// TestMergeExecFormShape verifies the Windows exec form: command is the bare exe
// path (NOT shell-quoted), the subcommand rides in args, and no shell is implied.
func TestMergeExecFormShape(t *testing.T) {
	exe := `C:\Users\me\AppData\Local\witness\witness.exe`
	out, err := mergeWitnessHooks([]byte(`{}`), execInvocation(exe))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SessionStart": "session-start", "UserPromptSubmit": "capture",
		"Stop": "capture", "SessionEnd": "session-end",
	}
	for ev, token := range want {
		cmds := execCommands(t, out, ev)
		if len(cmds) != 1 {
			t.Fatalf("%s: want 1 hook, got %d", ev, len(cmds))
		}
		if cmds[0].Command != exe {
			t.Errorf("%s: command = %q, want bare exe %q (no quoting)", ev, cmds[0].Command, exe)
		}
		if len(cmds[0].Args) != 1 || cmds[0].Args[0] != token {
			t.Errorf("%s: args = %v, want [%q]", ev, cmds[0].Args, token)
		}
	}
}

// TestMergeExecFormIsIdempotent guards the research-flagged risk: exec-form
// entries must be recognized as ours on re-install, else duplicates accumulate.
func TestMergeExecFormIsIdempotent(t *testing.T) {
	exe := `C:\witness\witness.exe`
	out, _ := mergeWitnessHooks([]byte(`{}`), execInvocation(exe))
	out2, _ := mergeWitnessHooks(out, execInvocation(exe))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if got := len(execCommands(t, out2, ev)); got != 1 {
			t.Errorf("%s: exec-form re-install duplicated, got %d hooks", ev, got)
		}
	}
}

// TestMergeMigratesShellToExecWithoutOrphans is the cross-form dedup contract:
// installing exec form over a prior shell-form install (or vice versa) must
// REPLACE the old witness hook, not leave both. Without isWitnessEntry
// recognizing both forms, a platform switch would orphan the stale entry.
func TestMergeMigratesShellToExecWithoutOrphans(t *testing.T) {
	// Start with a shell-form install.
	shell, _ := mergeWitnessHooks([]byte(`{}`), shellInvocation("/repo/hooks/witness.sh"))
	// Now install exec form over it.
	exe := `C:\witness\witness.exe`
	migrated, _ := mergeWitnessHooks(shell, execInvocation(exe))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		cmds := execCommands(t, migrated, ev)
		if len(cmds) != 1 {
			t.Fatalf("%s: want exactly 1 hook after migration, got %d: %+v", ev, len(cmds), cmds)
		}
		// It must be the exec-form one (args present, exe command), not the shim.
		if len(cmds[0].Args) == 0 || cmds[0].Command != exe {
			t.Errorf("%s: migrated hook is not the exec form: %+v", ev, cmds[0])
		}
		if strings.Contains(cmds[0].Command, "witness.sh") {
			t.Errorf("%s: stale shell-form shim was orphaned: %q", ev, cmds[0].Command)
		}
	}
	// And the reverse: shell over exec must also dedupe to one.
	back, _ := mergeWitnessHooks(migrated, shellInvocation("/repo/hooks/witness.sh"))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if got := len(execCommands(t, back, ev)); got != 1 {
			t.Errorf("%s: exec->shell migration left %d hooks, want 1", ev, got)
		}
	}
}

// TestRemoveWitnessHooksExecForm proves uninstall strips exec-form entries too.
func TestRemoveWitnessHooksExecForm(t *testing.T) {
	in := `{"hooks":{"Stop":[` +
		`{"hooks":[{"type":"command","command":"prettier"}]},` +
		`{"hooks":[{"type":"command","command":"C:\\witness\\witness.exe","args":["capture"]}]}` +
		`]}}`
	out, err := removeWitnessHooks([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	cmds := execCommands(t, out, "Stop")
	if len(cmds) != 1 || cmds[0].Command != "prettier" {
		t.Errorf("want only the foreign prettier hook left, got %+v", cmds)
	}
}

// TestAppendToPathValue locks the PATH-edit contract used by the Windows
// registry path: idempotent (case-insensitive, Windows paths), preserves the
// existing value VERBATIM including %VAR% tokens (the caller writes REG_EXPAND_SZ,
// so we must never flatten — the setx bug), and handles empty / trailing-';'.
func TestAppendToPathValue(t *testing.T) {
	dir := `C:\Users\me\AppData\Local\witness`
	tests := []struct {
		name        string
		current     string
		wantChanged bool
		wantValue   string
	}{
		{"empty PATH", "", true, dir},
		{"append to existing", `C:\Windows`, true, `C:\Windows;` + dir},
		{"already present (exact)", `C:\Windows;` + dir, false, `C:\Windows;` + dir},
		{"already present (case-insensitive)", `C:\Windows;` + `c:\users\me\appdata\local\witness`, false, `C:\Windows;` + `c:\users\me\appdata\local\witness`},
		{"trailing semicolon preserved", `C:\Windows;`, true, `C:\Windows;` + dir},
		{"preserves %VAR% tokens verbatim", `%SystemRoot%\system32;%SystemRoot%`, true, `%SystemRoot%\system32;%SystemRoot%;` + dir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := appendToPathValue(tt.current, dir)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if got != tt.wantValue {
				t.Errorf("value =\n %q\nwant\n %q", got, tt.wantValue)
			}
			// The original value must always be a prefix (we only ever append),
			// so no existing entry — %VAR% or otherwise — is ever rewritten.
			if tt.current != "" && !strings.HasPrefix(got, strings.TrimSuffix(tt.current, ";")) {
				t.Errorf("existing PATH not preserved as prefix: %q", got)
			}
		})
	}
}

func TestIsWitnessBinary(t *testing.T) {
	yes := []string{
		`C:\Users\me\AppData\Local\witness\witness.exe`,
		"/home/me/.local/share/witness/witness",
		"witness.exe",
		"witness",
		`C:\Users\me\WITNESS.EXE`, // case-insensitive (Windows)
	}
	no := []string{
		"prettier", "/usr/bin/node", `C:\tools\notwitness.exe`,
		"witnessing.exe",
		// A DIFFERENT tool named witness-<x>: the installer never writes these
		// (it always copies to a bare witness.exe), so matching them would only
		// risk clobbering a foreign hook. Must NOT match.
		"bin/witness-windows-amd64.exe",
		"/usr/local/bin/witness-cli",
	}
	for _, p := range yes {
		if !isWitnessBinary(p) {
			t.Errorf("isWitnessBinary(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isWitnessBinary(p) {
			t.Errorf("isWitnessBinary(%q) = true, want false", p)
		}
	}
}

// TestExecFormDoesNotStripForeignWitnessHook is the regression guard for the
// review-confirmed bug: a user's UNRELATED hook that invokes a different tool
// named "witness" (e.g. the testifysec supply-chain CLI) with args must be
// preserved verbatim by both merge and remove — not misclassified as ours.
// This runs on every platform (isWitnessEntry has no build tags), so it also
// protects the "Unix byte-identical" claim.
func TestExecFormDoesNotStripForeignWitnessHook(t *testing.T) {
	// A foreign hook: bare "witness" binary, but its arg is NOT one of our tokens.
	foreign := `{"model":"opus","hooks":{"Stop":[{"hooks":[` +
		`{"type":"command","command":"/usr/local/bin/witness","args":["run","--step","build"]}` +
		`]}]}}`

	// remove must leave it untouched.
	out, err := removeWitnessHooks([]byte(foreign))
	if err != nil {
		t.Fatal(err)
	}
	if cmds := execCommands(t, out, "Stop"); len(cmds) != 1 || cmds[0].Command != "/usr/local/bin/witness" {
		t.Errorf("foreign witness hook was stripped by removeWitnessHooks: %+v", cmds)
	}

	// merge (shell form, Unix) must also preserve it while adding ours.
	merged, err := mergeWitnessHooks([]byte(foreign), shellInvocation("/repo/hooks/witness.sh"))
	if err != nil {
		t.Fatal(err)
	}
	foundForeign := false
	for _, c := range execCommands(t, merged, "Stop") {
		if c.Command == "/usr/local/bin/witness" {
			foundForeign = true
		}
	}
	if !foundForeign {
		t.Errorf("foreign witness hook clobbered by mergeWitnessHooks; Stop = %+v", execCommands(t, merged, "Stop"))
	}
}

// The shim path is written into a shell-executed command, so it must be quoted —
// a repo cloned into a path with a space (common on macOS) would otherwise
// word-split and every hook would silently fail.
func TestHookCommandsQuotePathWithSpaces(t *testing.T) {
	shim := "/Users/me/My Projects/claude-witness/hooks/witness.sh"
	out, err := mergeWitnessHooks([]byte(`{}`), shellInvocation(shim))
	if err != nil {
		t.Fatal(err)
	}
	for ev, sub := range map[string]string{
		"SessionStart": "session-start", "UserPromptSubmit": "capture",
		"Stop": "capture", "SessionEnd": "session-end",
	} {
		cmds := eventCommands(t, out, ev)
		if len(cmds) != 1 {
			t.Fatalf("%s: want 1 command, got %v", ev, cmds)
		}
		want := shellQuote(shim) + " " + sub
		if cmds[0] != want {
			t.Errorf("%s command not safely quoted:\n got %q\nwant %q", ev, cmds[0], want)
		}
		// The full path must survive as one shell token (no bare space split).
		if !strings.Contains(cmds[0], shellQuote(shim)) {
			t.Errorf("%s: shim path not quoted as one token: %q", ev, cmds[0])
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/plain/path":  `'/plain/path'`,
		"/has space/x": `'/has space/x'`,
		"/has'quote/x": `'/has'\''quote/x'`,
		"/has$var/x":   `'/has$var/x'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoveWitnessHooks(t *testing.T) {
	in := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"prettier"}]},{"hooks":[{"type":"command","command":"/r/hooks/witness.sh capture"}]}]}}`
	out, err := removeWitnessHooks([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	cmds := eventCommands(t, out, "Stop")
	if countWitness(cmds) != 0 {
		t.Errorf("witness hooks should be gone, got %v", cmds)
	}
	if len(cmds) != 1 || !strings.Contains(cmds[0], "prettier") {
		t.Errorf("foreign hook must remain, got %v", cmds)
	}
}

func TestMergeOpenCodeMCP(t *testing.T) {
	in := []byte(`{"mcp":{"other":{"type":"local","command":["x"]}},"plugin":["p"]}`)
	out, err := mergeOpenCodeMCP(in, "/repo/hooks/witness.sh")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["plugin"] == nil {
		t.Fatalf("foreign config was not preserved: %s", out)
	}
	mcp := root["mcp"].(map[string]any)
	if mcp["other"] == nil {
		t.Fatalf("foreign MCP server was not preserved: %s", out)
	}
	witness := mcp["witness"].(map[string]any)
	if witness["type"] != "local" || witness["enabled"] != true {
		t.Fatalf("bad witness MCP config: %#v", witness)
	}
	cmd := witness["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "/repo/hooks/witness.sh" || cmd[1] != "mcp" {
		t.Fatalf("bad witness command: %#v", cmd)
	}
}

func TestMergeOpenCodeMCPAcceptsJSONC(t *testing.T) {
	in := []byte(`{
		// comments and trailing commas are valid OpenCode config JSONC
		"$schema": "https://opencode.ai/config.json",
		"plugin": ["https://example.test/plugin.js"],
		"mcp": {
			"other": {"type":"local", "command":["x",],},
		},
	}`)
	out, err := mergeOpenCodeMCP(in, "/repo/hooks/witness.sh")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["plugin"] == nil {
		t.Fatalf("foreign JSONC config was not preserved: %s", out)
	}
	mcp := root["mcp"].(map[string]any)
	if mcp["other"] == nil || mcp["witness"] == nil {
		t.Fatalf("expected other and witness MCP servers: %s", out)
	}
}

func TestOpenCodePluginSourceBakesShim(t *testing.T) {
	src := opencodeplugin.Source("/repo/hooks/witness.sh")
	if !strings.Contains(src, `globalThis.WITNESS_SHIM = "/repo/hooks/witness.sh"`) {
		t.Fatalf("installed plugin does not bake the shim path: %s", src)
	}
	if !strings.Contains(src, `process.env.WITNESS_BIN || "witness"`) {
		t.Fatalf("npm plugin should fall back to witness on PATH: %s", src)
	}
	if !strings.Contains(src, `export const Witness = plugin`) || !strings.Contains(src, `export const ClaudeWitness = plugin`) {
		t.Fatalf("installed plugin should preserve both OpenCode export names: %s", src)
	}
	if !strings.Contains(src, `const args = ["import", "--agent", "opencode", "--quiet", "--auto"]`) {
		t.Fatalf("installed plugin should reconcile OpenCode DB before distillation: %s", src)
	}
	if !strings.Contains(src, `type === "session.idle"`) || !strings.Contains(src, `type === "session.status"`) || !strings.Contains(src, `status?.type === "idle"`) {
		t.Fatalf("installed plugin should sync from idle events: %s", src)
	}
	if !strings.Contains(src, `const sessionWaiters = new Map()`) || !strings.Contains(src, `const batchWaiters = claimWaiters(coveredSessions)`) || !strings.Contains(src, `const modernIdleWaiters = new Map()`) {
		t.Fatalf("installed plugin should wait for and deduplicate idle imports: %s", src)
	}
	if !strings.Contains(src, `const IMPORT_GRACE_MS = 5000`) || !strings.Contains(src, `let disposing = false`) || !strings.Contains(src, `waitForIdle()`) {
		t.Fatalf("installed plugin should drain imports gracefully before disposal: %s", src)
	}
}

func TestNpmPackageHidesSourceOnlyInstallCommands(t *testing.T) {
	t.Setenv("WITNESS_NPM_PACKAGE", "1")
	if !newInstallCmd().Hidden || !newUninstallCmd().Hidden {
		t.Fatal("npm package should hide source-checkout install commands")
	}
}

func TestRemoveLegacyOpenCodePluginsKeepsCurrentName(t *testing.T) {
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	current := openCodePluginPath(dir, openCodePluginName)
	legacy := openCodePluginPath(dir, legacyOpenCodePluginName)
	if err := os.WriteFile(current, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeLegacyOpenCodePlugins(dir)

	if _, err := os.Stat(current); err != nil {
		t.Fatalf("current plugin should stay installed: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy plugin should be removed, got %v", err)
	}
}

func TestRemoveAllOpenCodePluginsRemovesCurrentAndLegacyNames(t *testing.T) {
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{openCodePluginName, legacyOpenCodePluginName} {
		if err := os.WriteFile(openCodePluginPath(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removeAllOpenCodePlugins(dir)

	for _, name := range []string{openCodePluginName, legacyOpenCodePluginName} {
		if _, err := os.Stat(openCodePluginPath(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, got %v", name, err)
		}
	}
}

func TestRemoveOpenCodeMCP(t *testing.T) {
	in := []byte(`{"mcp":{"witness":{"type":"local"},"other":{"type":"local"}},"autoupdate":false}`)
	out, err := removeOpenCodeMCP(in)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	mcp := root["mcp"].(map[string]any)
	if mcp["witness"] != nil || mcp["other"] == nil || root["autoupdate"] != false {
		t.Fatalf("unexpected cleaned config: %s", out)
	}
}

func TestRemoveOpenCodeMCPAcceptsJSONC(t *testing.T) {
	in := []byte(`{
		"mcp": {
			"witness": {"type":"local",},
			"other": {"type":"local"}, // keep this one
		},
	}`)
	out, err := removeOpenCodeMCP(in)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	mcp := root["mcp"].(map[string]any)
	if mcp["witness"] != nil || mcp["other"] == nil {
		t.Fatalf("unexpected cleaned config: %s", out)
	}
}
