package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	out, err := mergeWitnessHooks([]byte(`{}`), "/repo/hooks/witness.sh")
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
	out, _ := mergeWitnessHooks([]byte(`{}`), "/repo/hooks/witness.sh")
	out2, _ := mergeWitnessHooks(out, "/repo/hooks/witness.sh") // re-install
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if got := countWitness(eventCommands(t, out2, ev)); got != 1 {
			t.Errorf("%s: re-install should not duplicate, got %d", ev, got)
		}
	}
}

func TestMergePreservesForeignHooksAndSettings(t *testing.T) {
	in := `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"prettier --write"}]}]}}`
	out, err := mergeWitnessHooks([]byte(in), "/repo/hooks/witness.sh")
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

// The shim path is written into a shell-executed command, so it must be quoted —
// a repo cloned into a path with a space (common on macOS) would otherwise
// word-split and every hook would silently fail.
func TestHookCommandsQuotePathWithSpaces(t *testing.T) {
	shim := "/Users/me/My Projects/claude-witness/hooks/witness.sh"
	out, err := mergeWitnessHooks([]byte(`{}`), shim)
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

// TestOpenCodePluginBodyInSync guards against the two copies of the OpenCode
// plugin drifting: the installed source (built from the embedded openCodePluginBody)
// and the repo's local-test copy at .opencode/plugins/claude-witness.js. They
// legitimately differ only in how the SHIM constant is resolved; everything from
// the first `function ` onward must be byte-identical.
func TestOpenCodePluginBodyInSync(t *testing.T) {
	body := func(src, where string) string {
		i := strings.Index(src, "function ")
		if i < 0 {
			t.Fatalf("%s: no plugin body found (missing `function `)", where)
		}
		return strings.TrimSpace(src[i:])
	}

	installed := openCodePluginSource("/repo/hooks/witness.sh")
	committed, err := os.ReadFile(filepath.Join("..", "..", ".opencode", "plugins", "claude-witness.js"))
	if err != nil {
		t.Fatalf("read local-test plugin: %v", err)
	}
	if got, want := body(string(committed), "local-test copy"), body(installed, "installed source"); got != want {
		t.Fatalf(".opencode/plugins/claude-witness.js body has drifted from the embedded openCodePluginBody.\n"+
			"Edit cmd/witness/opencode_plugin.js and sync the local-test copy's body to match.\n\nlocal-test:\n%s\n\ninstalled:\n%s", got, want)
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
