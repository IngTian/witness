package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
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
	binary := filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")
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

func TestUnixNpmShimResolvesNativeBinary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node_modules", "@openai")
	shim := filepath.Join(root, "codex", "bin", "codex.js")
	bin := filepath.Join(root, "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
	for _, path := range []string{shim, bin} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveUnixCLI(shim, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	wantFile, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	gotFile, err := os.Stat(got)
	if err != nil || !os.SameFile(gotFile, wantFile) {
		t.Fatalf("resolved %q, want the file at %q: %v", got, bin, err)
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

func setUpChild(t *testing.T) *runner {
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
	return (Platform{}).NewRunner(store.Config{RuntimeRoot: t.TempDir(), RunnerModels: []string{"test"}}).(*runner)
}

func helperRunner(t *testing.T) *runner {
	t.Helper()
	r := setUpChild(t)
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

// A `codex exec` that answered and exited 0 while a grandchild still holds its stdout is a reply:
// WaitDelay closes the pipe and os/exec reports exec.ErrWaitDelay, which only follows a clean exit.
// The preflight still runs through the helper; only the exec is a shell, whose `sleep 3` ends on
// its own.
func TestRunAcceptsAReplyWhileAGrandchildHoldsStdout(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("spawns a shell that leaves a `sleep 3` grandchild holding its stdout")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no `sh` to stand in for codex exec: %v", err)
	}
	r := helperRunner(t)
	helper := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		if !slices.Contains(args, "exec") {
			return helper(ctx, bin, args...)
		}
		return exec.CommandContext(ctx, sh, "-c", `echo '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}'; `+
			`echo '{"type":"turn.completed"}'; sleep 3 &`)
	}
	t.Cleanup(func() { commandContext = helper })
	if got, err := r.Run(context.Background(), "test", "witness-system", "corpus-only"); err != nil || got != "answer" {
		t.Fatalf("Run = %q, %v; want the reply (ErrWaitDelay after exit 0 is success)", got, err)
	}
}

// The preflight's `--version` and `debug models` follow the same rule: exit 0 with a grandchild still
// holding stdout is their output, not a failed preflight. Each `sleep 3` ends on its own.
func TestOpenAcceptsPreflightOutputWhileAGrandchildHoldsStdout(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("spawns shells that leave `sleep 3` grandchildren holding their stdout")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no `sh` to stand in for codex: %v", err)
	}
	r := setUpChild(t)
	helper := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		var out string
		switch joined := strings.Join(args, " "); {
		case joined == "--version":
			out = "codex-cli 0.169.0-alpha.11.1"
		case strings.HasSuffix(joined, "debug models"):
			out = `{"models":[{"slug":"test"}]}`
		default:
			return helper(ctx, bin, args...)
		}
		return exec.CommandContext(ctx, sh, "-c", "echo '"+out+"'; sleep 3 &")
	}
	t.Cleanup(func() { commandContext = helper })
	if err := r.Open(context.Background()); err != nil {
		t.Fatalf("Open = %v; want nil (ErrWaitDelay after exit 0 is the preflight's output)", err)
	}
}

// A plan login's usage-limit refusal is a pause (cmd's gate arms it); with CODEX_API_KEY the same
// refusal is API credit, an ordinary failure that backs off like any other.
func TestRunReportsAPlanRefusalAsAPause(t *testing.T) {
	r := helperRunner(t)
	t.Setenv("WITNESS_CODEX_HELPER_MODE", "quota")
	ctx := context.Background()
	var quota *platform.QuotaExhaustedError
	r.plan = true
	if _, err := r.Run(ctx, "test", "p", "i"); !errors.As(err, &quota) || quota.Runtime != "codex" || !quota.ResetAt.After(time.Now()) {
		t.Fatalf("a plan refusal must be QuotaExhausted{codex, future reset}: %v", err)
	}
	r.plan = false
	if _, err := r.Run(ctx, "test", "p", "i"); err == nil || errors.As(err, &quota) {
		t.Fatalf("with CODEX_API_KEY the refusal must be a plain error: %v", err)
	}
}

func TestDisabledRunnerNeverSpawns(t *testing.T) {
	r := helperRunner(t)
	t.Setenv(platform.DisableExternalRunnersEnv, "1")
	previous := commandContext
	commandContext = func(context.Context, string, ...string) *exec.Cmd {
		t.Fatal("disabled runner spawned a child")
		return nil
	}
	defer func() { commandContext = previous }()
	if err := r.Open(context.Background()); err == nil {
		t.Fatal("opened disabled runner")
	}
	if _, err := r.Run(context.Background(), "test", "p", "i"); err == nil {
		t.Fatal("ran disabled runner")
	}
	if r.ValidateModels(context.Background(), "test") == nil || LoginReady(context.Background(), r.root) == nil {
		t.Fatal("disabled runner was reported ready")
	}
	if err := Login(context.Background(), r.root); err == nil {
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
		version := os.Getenv("WITNESS_CODEX_HELPER_VERSION")
		if version == "" {
			version = "0.169.0-alpha.11.1"
		}
		fmt.Println("codex-cli " + version)
		os.Exit(0)
	}
	mode := os.Getenv("WITNESS_CODEX_HELPER_MODE")
	if strings.Contains(args, "app-server --listen stdio://") {
		servePolicyFixture(mode)
		os.Exit(0)
	}
	if strings.Contains(args, "login status") && mode == "logged-out" {
		os.Exit(1)
	}
	if strings.Contains(args, "login status") || strings.Contains(args, "login --device-auth") || strings.HasSuffix(args, " logout") {
		os.Exit(0)
	}
	if strings.Contains(args, "debug models") {
		if mode == "bad-catalog" {
			fmt.Println(`{"models":[]}`)
			os.Exit(0)
		}
		fmt.Println(`{"models":[{"slug":"test","tool_mode":"code_mode_only","shell_type":"shell_command","experimental_supported_tools":["clock"],"apply_patch_tool_type":"freeform","supports_search_tool":true}]}`)
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
	for _, flag := range []string{
		"--ignore-user-config", "--ignore-rules", "--ephemeral", "--json", "--skip-git-repo-check", "-s read-only", "-m test -",
		"features.shell_tool=false", "features.view_image=false", `web_search="disabled"`,
		"tools.experimental_request_user_input.enabled=false", "agents.enabled=false", "features.multi_agent=false", "features.goals=false", "features.image_generation=false",
		"features.apps=false", "features.plugins=false", "features.tool_suggest=false", "features.sleep_tool=false", "features.hooks=false", "features.skill_search=false",
		"skills.include_instructions=false", "skills.bundled.enabled=false", "features.reasoning_effort_override=false", "project_doc_max_bytes=0", "project_root_markers=[]",
		"include_permissions_instructions=false", "include_apps_instructions=false", "include_collaboration_mode_instructions=false", "include_environment_context=false", "analytics.enabled=false",
	} {
		if !strings.Contains(args, flag) {
			fmt.Fprintln(os.Stderr, "missing", flag)
			os.Exit(2)
		}
	}
	if os.Getenv("TZ") != "UTC" || os.Getenv("WITNESS_WORKER") != "1" || os.Getenv("CODEX_HOME") == "" {
		os.Exit(2)
	}
	var prompt, catalog bool
	for _, arg := range os.Args {
		key, value, _ := strings.Cut(arg, "=")
		if key != "model_instructions_file" && key != "model_catalog_json" {
			continue
		}
		var path string
		if json.Unmarshal([]byte(value), &path) != nil {
			os.Exit(3)
		}
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			os.Exit(4)
		}
		if key == "model_instructions_file" {
			prompt = string(b) == "witness-system"
			continue
		}
		var d struct {
			Models []struct {
				Slug     string
				ToolMode string   `json:"tool_mode"`
				Shell    string   `json:"shell_type"`
				Search   bool     `json:"supports_search_tool"`
				Tools    []string `json:"experimental_supported_tools"`
				Patch    *string  `json:"apply_patch_tool_type"`
			}
		}
		if json.Unmarshal(b, &d) == nil && len(d.Models) == 1 {
			m := d.Models[0]
			catalog = m.Slug == "test" && m.ToolMode == "direct" && m.Shell == "disabled" && !m.Search && m.Tools != nil && len(m.Tools) == 0 && m.Patch == nil
		}
	}
	if !catalog || !prompt {
		fmt.Fprintln(os.Stderr, "bad prompt or catalog")
		os.Exit(4)
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

func servePolicyFixture(mode string) {
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var req rpcRequest
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(5)
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"userAgent":"fixture"}`
		case "config/read":
			result = `{"layers":[{"name":{"type":"system"},"config":{}}]}`
			if mode == "managed-config" {
				result = `{"layers":[{"name":{"type":"mdm"},"config":{"features":{"shell_tool":true}}}]}`
			}
			if mode == "missing-layers" {
				result = `{}`
			}
		case "configRequirements/read":
			result = `{"requirements":null}`
			if mode == "managed-requirements" {
				result = `{"requirements":{"featureRequirements":{"shell_tool":true}}}`
			}

		default:
			continue
		}
		fmt.Printf(`{"id":%d,"result":%s}`+"\n", req.ID, result)
	}
}

func TestPreflightReportsUnusableRuntime(t *testing.T) {
	for _, tc := range []struct{ mode, version, model, want string }{
		{"", "0.169.0-alpha.11", "test", "no verified tool-free contract"},
		{"", "0.172.0", "test", "no verified tool-free contract"},
		{"", "0.169.0", "test", ""},
		{"", "0.170.0", "test", ""},
		{"", "0.171.0", "test", ""},
		{"managed-config", "", "test", "mdm config layer"},
		{"managed-requirements", "", "test", "managed requirements"},
		{"missing-layers", "", "test", "did not report config layers"},
		{"logged-out", "", "test", "needs its own ChatGPT login"},
		{"bad-catalog", "", "test", "catalog has no models"},
		{"", "", "missing", "absent from this login's catalog"},
	} {
		t.Run(tc.mode+tc.version+tc.model, func(t *testing.T) {
			r := setUpChild(t)
			r.plan = true
			t.Setenv("CODEX_API_KEY", "")
			t.Setenv("WITNESS_CODEX_HELPER_MODE", tc.mode)
			t.Setenv("WITNESS_CODEX_HELPER_VERSION", tc.version)
			checkPreflightError(t, r.ValidateModels(context.Background(), tc.model), tc.want)
			r.models = []string{tc.model}
			checkPreflightError(t, r.Open(context.Background()), tc.want)
		})
	}
	r := setUpChild(t)
	t.Setenv("WITNESS_CODEX_BINARY", filepath.Join(t.TempDir(), "missing-codex.exe"))
	if r.ValidateModels(context.Background(), "test") == nil {
		t.Fatal("doctor accepted a missing binary")
	}
}

func checkPreflightError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Fatalf("want %q, got %v", want, err)
	}
}

func TestLoginAndReadinessHoldTheLoginLock(t *testing.T) {
	r := setUpChild(t)
	t.Setenv("CODEX_API_KEY", "")
	previous := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		assertLoginLockHeld(t, r.root)
		return previous(ctx, bin, args...)
	}
	if err := Login(context.Background(), r.root); err != nil {
		t.Fatal(err)
	}
	if err := (Platform{}).ReadyToBind(context.Background(), r.root); err != nil {
		t.Fatal(err)
	}
	if err := (Platform{}).Logout(context.Background(), r.root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_CODEX_HELPER_MODE", "logged-out")
	if (Platform{}).ReadyToBind(context.Background(), r.root) == nil {
		t.Fatal("logged out home reported ready")
	}
}

func TestRunRechecksManagedPolicyBeforeReceivingCorpus(t *testing.T) {
	r := helperRunner(t)
	t.Setenv("WITNESS_CODEX_HELPER_MODE", "managed-requirements")
	previous := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		for _, arg := range args {
			if arg == "exec" {
				t.Fatal("Codex exec started with managed requirements")
			}
		}
		return previous(ctx, bin, args...)
	}
	_, err := r.Run(context.Background(), "test", "witness-system", "corpus-only")
	checkPreflightError(t, err, "managed requirements")
}

func assertLoginLockHeld(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	release, err := loginLock(ctx, root)
	if err == nil {
		release()
		t.Fatal("Codex subprocess started without the login lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestRunWaitsForLoginLockBeforeStartingGenerationDeadline(t *testing.T) {
	r := helperRunner(t)
	previousTimeout := runTimeout
	runTimeout = 500 * time.Millisecond
	defer func() { runTimeout = previousTimeout }()
	release, err := loginLock(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(release)
	defer release()
	released := make(chan struct{})
	timer := time.AfterFunc(750*time.Millisecond, func() {
		release()
		close(released)
	})
	defer timer.Stop()
	previous := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		assertLoginLockHeld(t, r.root)
		return previous(ctx, bin, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, "test", "witness-system", "corpus-only"); err != nil {
		t.Fatal(err)
	}
	// The competing holder is gone after the first Run. This second invocation
	// proves that Run itself takes the lock before spawning any subprocess.
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := r.Run(ctx, "test", "witness-system", "corpus-only"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenQueuesAndDiagnosticsReportBusy(t *testing.T) {
	r := setUpChild(t)
	release, err := loginLock(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(release)
	defer release()
	if err := r.ValidateModels(context.Background(), "test"); !errors.Is(err, platform.ErrRunnerBusy) {
		t.Fatalf("doctor while locked: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := LoginReady(ctx, r.root); !errors.Is(err, platform.ErrRunnerBusy) {
		t.Fatalf("wire while locked: %v", err)
	}
	ctxOpen, cancelOpen := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancelOpen()
	if err := r.Open(ctxOpen); err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, platform.ErrRunnerBusy) {
		t.Fatalf("Open lock wait was classified as a model result: %v", err)
	}
	releasedAt := make(chan time.Time, 1)
	timer := time.AfterFunc(200*time.Millisecond, func() {
		releasedAt <- time.Now()
		release()
	})
	defer timer.Stop()
	previous := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		if len(args) == 1 && args[0] == "--version" {
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Before((<-releasedAt).Add(preflightTimeout)) {
				t.Fatal("Open started the preflight budget before acquiring the login lock")
			}
		}
		return previous(ctx, bin, args...)
	}
	if err := r.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoginAndReadinessRejectUnverifiedCLI(t *testing.T) {
	r := setUpChild(t)
	t.Setenv("WITNESS_CODEX_HELPER_VERSION", "0.172.0")
	previous := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		if len(args) != 1 || args[0] != "--version" {
			t.Fatalf("unverified binary reached %v", args)
		}
		return previous(ctx, bin, args...)
	}
	checkPreflightError(t, LoginReady(context.Background(), r.root), "no verified tool-free contract")
	checkPreflightError(t, Login(context.Background(), r.root), "no verified tool-free contract")
}

func TestTestedVersionsMatchTheContractMatrixAndTheDocs(t *testing.T) {
	// Extract versions from codex-contract.yml
	ymlPath := filepath.Join("..", "..", "..", ".github", "workflows", "codex-contract.yml")
	ymlData, err := os.ReadFile(ymlPath)
	if err != nil {
		t.Fatal(err)
	}
	matrixPattern := regexp.MustCompile(`\|\| '(\[[^']*\])'`)
	matches := matrixPattern.FindAllSubmatch(ymlData, -1)
	if len(matches) != 1 {
		t.Fatalf("codex-contract.yml matrix pattern not found or matched multiple times (got %d matches)", len(matches))
	}
	var ci []string
	if err := json.Unmarshal(matches[0][1], &ci); err != nil {
		t.Fatalf("unmarshal contract matrix: %v", err)
	}

	// Extract versions from docs/codex-runner.md
	docsPath := filepath.Join("..", "..", "..", "docs", "codex-runner.md")
	docsData, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Scan only the "Use Codex CLI" line for supported versions
	linePattern := regexp.MustCompile(`(?m)^Use Codex CLI (.*)$`)
	lineMatch := linePattern.FindSubmatch(docsData)
	if lineMatch == nil {
		t.Fatal("docs/codex-runner.md: 'Use Codex CLI' line not found")
	}
	versionPattern := regexp.MustCompile(`\*\*([^*]+)\*\*`)
	versionMatches := versionPattern.FindAllSubmatch(lineMatch[1], -1)
	var docs []string
	digitsPattern := regexp.MustCompile(`\d`)
	for _, match := range versionMatches {
		version := string(match[1])
		// Only include version numbers (contains dots and digits)
		if strings.Contains(version, ".") && digitsPattern.MatchString(version) {
			docs = append(docs, version)
		}
	}
	if len(docs) == 0 {
		t.Fatal("docs/codex-runner.md: no versions found on 'Use Codex CLI' line")
	}

	if !slices.Equal(ci, testedVersions) {
		t.Errorf("codex-contract.yml pins %q, runner.go %q", ci, testedVersions)
	}
	if !slices.Equal(docs, testedVersions) {
		t.Errorf("docs/codex-runner.md lists %q, runner.go %q", docs, testedVersions)
	}
}
