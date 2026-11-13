package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The v2 contract the body must keep, each piece observed on OpenCode 2.2.7 and 2.3.0 (the
// evidence is docs/superpowers/specs/2026-11-13-opencode-v2-evidence/plugin.md); behaviour is
// tested in npm/opencode/witness.test.js, which runs this body and the npm copy.
func TestBodyIsAV2Plugin(t *testing.T) {
	body := Source("")
	for _, want := range []string{
		"export default {", `id: "witness"`, // v2 decodes only default: {id, setup}
		"ctx.event.subscribe(",
		`"session.execution.started"`, `"session.execution.succeeded"`,
		`"session.execution.failed"`, `"session.execution.interrupted"`,
		"globalThis.__witnessCaptureV2",          // one state for every Location instance
		"WITNESS_OPENCODE_BIN: process.execPath", // the Windows desktop's CLI is never on PATH
		`process.env.WITNESS_WORKER === "1"`,     // the recursion guard
		`spawnWitness(["worker-kick"])`,          // the quiet-period start goes through the kick gate
		"Bun.spawn([WITNESS_BIN, ...args]",       // an argv array, never a shell string
		`process.env.WITNESS_BIN || "witness"`,   // the unwired fallback
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the plugin body lacks %q", want)
		}
	}
	for _, bad := range []string{
		".properties",      // v1 payloads; v2 carries data.sessionID
		`"session.idle"`,   // accepted by v2's subscribe, never delivered
		`"session.status"`, // likewise
		`"worker", "stop"`, // cleanup has ~5 s before SIGKILL and runs on every reload
		"export const",     // v1's named exports; v2 rejects the module shape
		"IMPORT_GRACE_MS",  // the v1 dispose grace that cannot fit that budget
	} {
		if strings.Contains(body, bad) {
			t.Errorf("the plugin body still contains %q", bad)
		}
	}
}

// npm/opencode/witness.js is generated, so the two copies cannot drift (F113).
func TestNPMPluginIsGeneratedFromTheBody(t *testing.T) {
	prelude, err := os.ReadFile("npm_prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	want := string(prelude) + body
	npm := filepath.Join("..", "..", "..", "..", "npm", "opencode", "witness.js")
	if os.Getenv("WITNESS_WRITE_NPM_PLUGIN") == "1" {
		if err := os.WriteFile(npm, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(npm)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is not npm_prelude.js + witness.js; regenerate it:\n"+
			"  WITNESS_WRITE_NPM_PLUGIN=1 go test ./internal/platform/opencode/plugin/ -run TestNPMPluginIsGeneratedFromTheBody", npm)
	}
}

func TestInstalledShimRecognisesOnlyTheCurrentBody(t *testing.T) {
	for _, shim := range []string{"/r/hooks/witness.sh", `C:\Users\Zeying Tian\AppData\Local\witness\witness.exe`} {
		if got, current := InstalledShim(Source(shim)); got != shim || !current {
			t.Errorf("InstalledShim(Source(%q)) = (%q, %v), want (%q, true)", shim, got, current, shim)
		}
	}
	for name, src := range map[string]string{
		"an edited body":     Source("/r/hooks/witness.sh") + "\n// edit",
		"a v1-shaped plugin": "globalThis.WITNESS_SHIM = \"/r\"\n\nconst plugin = async () => ({})",
		"an empty file":      "",
	} {
		shim, current := InstalledShim(src)
		if current {
			t.Errorf("%s is reported current", name)
		}
		// The v1-shaped case must still return the parsed shim
		if name == "a v1-shaped plugin" && shim != "/r" {
			t.Errorf("v1-shaped plugin: got shim %q, want %q", shim, "/r")
		}
	}
}
