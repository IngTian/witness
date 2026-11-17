package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func TestChildEnvironmentDoesNotRedirectCapture(t *testing.T) {
	t.Setenv("CODEX_HOME", "coding-home")
	t.Setenv("CODEX_SQLITE_HOME", "coding-sqlite")
	t.Setenv("CODEX_ACCESS_TOKEN", "must-not-inherit")
	t.Setenv("CODEX_API_KEY", "explicit-key")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	t.Setenv("WITNESS_WORKER", "0")
	env := strings.Join(childEnv("runner-home"), "\n")
	for _, forbidden := range []string{"coding-home", "coding-sqlite", "must-not-inherit", "WITNESS_WORKER=0"} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("leaked %s", forbidden)
		}
	}
	for _, want := range []string{"CODEX_HOME=runner-home", "CODEX_API_KEY=explicit-key", "WITNESS_WORKER=1", "TZ=UTC"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
	if os.Getenv("CODEX_HOME") != "coding-home" {
		t.Fatal("parent capture home changed")
	}
}

func TestWindowsNpmShimResolvesNativeBinaryWithoutShell(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "codex.cmd")
	binary := filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "codex", "codex.exe")
	if _, err := resolveWindowsCLI(shim, "amd64"); err == nil {
		t.Fatal("accepted a missing native CLI")
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWindowsCLI(shim, "amd64"); err != nil || got != binary {
		t.Fatal(got, err)
	}
}

func TestStripCatalogPreservesOtherMetadataAndRefusesUnknownModel(t *testing.T) {
	raw := []byte(`{"etag":"keep","models":[{"slug":"test","tool_mode":"code_mode_only","apply_patch_tool_type":"freeform","shell_type":"shell_command","experimental_supported_tools":["clock"],"supports_search_tool":true,"context_window":1000}]}`)
	b, err := stripCatalog(raw, "test")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Etag   string
		Models []map[string]any
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	m := d.Models[0]
	if d.Etag != "keep" || m["context_window"] != float64(1000) || m["tool_mode"] != "direct" || m["shell_type"] != "disabled" || m["apply_patch_tool_type"] != nil || m["supports_search_tool"] != false || len(m["experimental_supported_tools"].([]any)) != 0 {
		t.Fatalf("catalog = %s", b)
	}
	for _, raw := range [][]byte{raw, []byte(`{}`), []byte(`{"models":[]}`), []byte(`broken`)} {
		if _, err := stripCatalog(raw, "unknown"); err == nil {
			t.Fatal("accepted missing model/catalog")
		}
	}
}

func TestUsageResetOnlyArmsKnownFutureLimits(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		message string
		want    string
	}{
		{"You’ve hit your usage limit. Try again at 1:20 PM.", "2026-11-16T13:20:00Z"},
		{"You've hit your usage limit. Upgrade or try again at Nov 17th, 2026 1:20 AM.", "2026-11-17T01:20:00Z"},
		{"You've hit your usage limit. Try again at Nov 21st, 2026 1:20 AM.", "2026-11-21T01:20:00Z"},
		{"You've hit your usage limit. Try again at 11:20 AM.", ""},
		{"You've hit your usage limit. Try again later.", ""},
		{"Quota exceeded. Check your plan and billing details.", ""},
		{"To use Codex with your ChatGPT plan, upgrade to Plus.", ""},
	} {
		got, ok := usageResetIn(tc.message, now, time.UTC)
		if ok != (tc.want != "") || (ok && got.Format(time.RFC3339) != tc.want) {
			t.Errorf("%s: %s, %v", tc.message, got, ok)
		}
	}
}

func TestUsageResetWindowsWallClock(t *testing.T) {
	now := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	reset, ok := usageResetIn("You've hit your usage limit. Try again at 6:20 PM.", now, time.FixedZone("PST", -8*3600))
	if !ok || reset.UTC().Format(time.RFC3339) != "2026-11-17T02:20:00Z" {
		t.Fatal(reset, ok)
	}
}

func TestEventsRejectToolsAndPartialOrFailedAnswers(t *testing.T) {
	good := "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"answer\"}}\n{\"type\":\"turn.completed\"}\n"
	if answer, err := parseEvents([]byte(good), time.Now()); err != nil || answer != "answer" {
		t.Fatal(answer, err)
	}
	for _, raw := range []string{
		`{"type":"item.started","item":{"type":"command_execution"}}` + "\n" + good,
		`{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}`,
		`{"type":"turn.failed","error":{"message":"quota exhausted"}}`,
		`garbage`, "",
	} {
		if _, err := parseEvents([]byte(raw), time.Now()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func helperRunner(t *testing.T) *runner {
	t.Helper()
	t.Setenv(platform.DisableExternalRunnersEnv, "0")
	t.Setenv("CODEX_API_KEY", "dummy")
	t.Setenv("WITNESS_CODEX_HELPER", "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_CODEX_BINARY", bin)
	previous := commandContext
	commandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, bin, append([]string{"-test.run=^TestCodexChildHelper$", "--"}, args...)...)
	}
	t.Cleanup(func() { commandContext = previous })
	r := (Platform{}).NewRunner(store.Config{RuntimeRoot: t.TempDir(), RunnerModels: []string{"test"}}).(*runner)
	if err := r.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunPassesIsolatedInstructionsAndCorpusThroughRealChild(t *testing.T) {
	r := helperRunner(t)
	if got, err := r.Run(context.Background(), "test", "witness-system", "corpus-only"); err != nil || got != "answer" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestRunAttributesDeadlineAndCallerCancellation(t *testing.T) {
	r := helperRunner(t)
	t.Setenv("WITNESS_CODEX_HELPER_MODE", "hang")
	previous := runTimeout
	runTimeout = 150 * time.Millisecond
	t.Cleanup(func() { runTimeout = previous })
	if _, err := r.Run(context.Background(), "test", "witness-system", "corpus-only"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Run(ctx, "test", "witness-system", "corpus-only"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestRunPublishesAndRechecksPlanPauseUnderLoginLock(t *testing.T) {
	r := helperRunner(t)
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r.meta, r.plan = st, true
	t.Setenv("WITNESS_CODEX_HELPER_MODE", "quota")
	var quota *platform.QuotaExhaustedError
	if _, err := r.Run(context.Background(), "test", "witness-system", "corpus-only"); !errors.As(err, &quota) {
		t.Fatalf("not a plan refusal: %v", err)
	}
	if st.MetaString(platform.UsagePauseKey("codex")) == "" {
		t.Fatal("refusal was not published before unlock")
	}
	previous := commandContext
	commandContext = func(context.Context, string, ...string) *exec.Cmd { t.Fatal("paused call spawned a child"); return nil }
	defer func() { commandContext = previous }()
	if _, err := r.Run(context.Background(), "test", "p", "i"); !errors.As(err, &quota) {
		t.Fatalf("pause not rechecked: %v", err)
	}
}

func TestDisabledRunnerNeverSpawns(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "1")
	r := &runner{}
	if err := r.Open(context.Background()); err == nil {
		t.Fatal("opened disabled runner")
	}
	if _, err := r.Run(context.Background(), "test", "p", "i"); err == nil {
		t.Fatal("ran disabled runner")
	}
	if err := Login(context.Background(), t.TempDir()); err == nil {
		t.Fatal("logged in disabled runner")
	}
}

// A subprocess, rather than a prebuilt exec.Cmd assertion, exercises Run's actual
// stdin, environment, prompt files, catalog and timeout attribution on every OS.
func TestCodexChildHelper(t *testing.T) {
	if os.Getenv("WITNESS_CODEX_HELPER") != "1" {
		return
	}
	args := strings.Join(os.Args, " ")
	if strings.Contains(args, "--version") {
		fmt.Println("codex-cli 0.169.0-alpha.11.1")
		os.Exit(0)
	}
	if strings.Contains(args, "debug models") {
		fmt.Println(`{"models":[{"slug":"test","tool_mode":"code_mode_only"}]}`)
		os.Exit(0)
	}
	if os.Getenv("WITNESS_CODEX_HELPER_MODE") == "hang" {
		time.Sleep(time.Minute)
		os.Exit(1)
	}
	if os.Getenv("WITNESS_CODEX_HELPER_MODE") == "quota" {
		location := time.UTC
		if runtime.GOOS == "windows" {
			location = time.Local
		}
		message := "You've hit your usage limit. Try again at " + time.Now().Add(24*time.Hour).In(location).Format("Jan 2, 2006 3:04 PM") + "."
		b, _ := json.Marshal(map[string]any{"type": "turn.failed", "error": map[string]string{"message": message}})
		fmt.Println(string(b))
		os.Exit(1)
	}
	if !strings.Contains(args, "--ignore-user-config --ignore-rules") || !strings.Contains(args, "--ephemeral") || os.Getenv("TZ") != "UTC" || os.Getenv("WITNESS_WORKER") != "1" {
		os.Exit(2)
	}
	for _, arg := range os.Args {
		if key, value, ok := strings.Cut(arg, "="); ok && key == "model_instructions_file" {
			var path string
			if json.Unmarshal([]byte(value), &path) != nil {
				os.Exit(3)
			}
			b, _ := os.ReadFile(filepath.FromSlash(path))
			if string(b) != "witness-system" {
				os.Exit(4)
			}
		}
	}
	var input bytes.Buffer
	_, _ = input.ReadFrom(os.Stdin)
	if input.String() != "corpus-only" {
		os.Exit(5)
	}
	fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`)
	fmt.Println(`{"type":"turn.completed"}`)
	os.Exit(0)
}
