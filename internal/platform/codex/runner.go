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
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/proc"
	"github.com/IngTian/witness/internal/store"
)

// The tool policy is a versioned protocol, not a sandbox policy: transcripts are
// untrusted and must never be offered tools, even read-only ones. New CLI versions
// need the wire contract checked before this range is extended.
// TestTestedVersionsMatchTheContractMatrixAndTheDocs keeps this list,
// codex-contract.yml's pinned matrix and docs/codex-runner.md equal.
var testedVersions = []string{"0.169.0", "0.169.0-alpha.11.1", "0.170.0", "0.171.0"}

const preflightTimeout = 45 * time.Second

var runTimeout = 20 * time.Minute

// commandContext builds every codex child. proc.HideWindow keeps each from opening a console
// window under the DETACHED_PROCESS worker on Windows; tests swap the var for a stub.
var commandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	proc.HideWindow(cmd)
	return cmd
}

type runner struct {
	root, binary string
	models       []string
	plan         bool
}

func (Platform) NewRunner(cfg store.Config) platform.Runner {
	return &runner{root: cfg.RuntimeRoot, models: cfg.RunnerModels, plan: strings.TrimSpace(os.Getenv("CODEX_API_KEY")) == ""}
}

func (r *runner) OnSharedAllowance(string) bool { return r.plan }

func (Platform) RequiresModel() bool   { return true }
func (*runner) InvocationHint() string { return "codex exec (witness-owned login)" }
func (*runner) Close() error           { return nil }

// RunnerHome never resolves through Home(): that function belongs to the capture
// importer. Changing the parent's CODEX_HOME would redirect capture into the worker.
func RunnerHome(root string) string { return filepath.Join(root, "codex", "home") }

func FindBinary() (string, error) {
	if path := strings.TrimSpace(os.Getenv("WITNESS_CODEX_BINARY")); path != "" {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		return resolveCLI(resolved)
	}
	if path, err := exec.LookPath("codex"); err == nil {
		if resolved, err := resolveCLI(path); err == nil {
			return resolved, nil
		}
	}
	paths := []string{}
	if runtime.GOOS == "darwin" {
		paths = append(paths, "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
			"/Applications/Codex.app/Contents/Resources/codex")
	}
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			paths = append(paths, filepath.Join(base, "Microsoft", "WindowsApps", "codex.exe"))
		}
	}
	for _, path := range paths {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("Codex CLI not found; set WITNESS_CODEX_BINARY to the desktop app's bundled CLI or install codex on PATH")
}

func resolveCLI(path string) (string, error) {
	if runtime.GOOS == "windows" {
		return resolveWindowsCLI(path, runtime.GOARCH)
	}
	return resolveUnixCLI(path, runtime.GOOS, runtime.GOARCH)
}

// Older Node versions launch a separate native child instead of replacing the
// wrapper process. Run that child directly so a deadline stops Codex itself
// before witness releases the login lock.
func resolveUnixCLI(path, goos, arch string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Base(resolved) != "codex.js" {
		return path, nil
	}
	cpu, pkgArch := "x86_64", "x64"
	if arch == "arm64" {
		cpu, pkgArch = "aarch64", "arm64"
	}
	triple := cpu + "-unknown-linux-musl"
	if goos == "darwin" {
		triple = cpu + "-apple-darwin"
	}
	root := filepath.Dir(filepath.Dir(resolved))
	return resolveVendorBinary(path, []string{root}, "codex-"+goos+"-"+pkgArch, triple, "codex")
}

// npm's Windows entry point is a .cmd shim, which os/exec cannot execute as a
// native process. Resolve its shipped executable instead of introducing a shell
// that would re-interpret model names, paths and configuration arguments.
func resolveWindowsCLI(path, arch string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".cmd" && ext != ".bat" && ext != ".ps1" {
		return path, nil
	}
	triple, pkgArch := "x86_64-pc-windows-msvc", "x64"
	if arch == "arm64" {
		triple, pkgArch = "aarch64-pc-windows-msvc", "arm64"
	}
	dir := filepath.Dir(path)
	roots := []string{filepath.Join(dir, "node_modules", "@openai", "codex"), filepath.Join(dir, "..", "@openai", "codex")}
	return resolveVendorBinary(path, roots, "codex-win32-"+pkgArch, triple, "codex.exe")
}

func resolveVendorBinary(shim string, roots []string, pkg, triple, executable string) (string, error) {
	for _, root := range roots {
		for _, vendor := range []string{
			filepath.Join(root, "node_modules", "@openai", pkg, "vendor"),
			filepath.Join(filepath.Dir(root), pkg, "vendor"),
			filepath.Join(root, "vendor"),
		} {
			candidate := filepath.Join(vendor, triple, "bin", executable)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("cannot resolve Codex executable behind %s; set WITNESS_CODEX_BINARY to the native CLI", shim)
}

// childEnv has one credential override, CODEX_API_KEY. In particular a desktop
// access token, SQLite home, provider URL or managed config must not cross over.
func childEnv(home string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if (strings.HasPrefix(upper, "CODEX_") && upper != "CODEX_API_KEY") ||
			strings.HasPrefix(upper, "OPENAI_") || upper == "WITNESS_WORKER" || upper == "TZ" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CODEX_HOME="+home, "WITNESS_WORKER=1", "TZ=UTC")
}

// LoginReady is bounded by wire's context. Report lock contention separately
// so a healthy login is never mistaken for a missing credential.
func LoginReady(ctx context.Context, root string) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	bin, err := FindBinary()
	if err != nil {
		return err
	}
	unlock, err := loginLock(ctx, root)
	if errors.Is(err, context.DeadlineExceeded) {
		return platform.ErrRunnerBusy
	}
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	env, err := runnerEnv(dir, root)
	if err != nil {
		return err
	}
	if err := checkVersion(ctx, bin, dir, env); err != nil {
		return err
	}
	return checkLogin(ctx, bin, dir, env)
}

func checkLogin(ctx context.Context, bin, dir string, env []string) error {
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "CODEX_API_KEY") && strings.TrimSpace(value) != "" {
			return nil
		}
	}
	cmd := commandContext(ctx, bin, "login", "status")
	cmd.Dir, cmd.Env = dir, env
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Codex runner needs its own ChatGPT login: run witness auth codex (or set CODEX_API_KEY)")
	}
	return nil
}

func loginLock(ctx context.Context, root string) (func(), error) {
	if root == "" {
		return nil, fmt.Errorf("Codex runner requires an archive runtime root")
	}
	return store.RuntimeLock(ctx, filepath.Join(root, "codex"))
}

// Login signs in directly to witness's own home. No credential file is read or
// copied from the coding account. The same lock covers login, catalog and exec.
func Login(ctx context.Context, root string) error {
	return loginCommand(ctx, root, false)
}

func loginCommand(ctx context.Context, root string, logout bool) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	bin, err := FindBinary()
	if err != nil {
		return err
	}
	unlock, err := loginLock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	home, err := filepath.Abs(RunnerHome(root))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	dir, err := scratchDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var env []string
	for _, v := range childEnv(home) {
		if key, _, _ := strings.Cut(v, "="); !strings.EqualFold(key, "CODEX_API_KEY") {
			env = append(env, v)
		}
	}
	args := []string{"logout"}
	if !logout {
		versionCtx, cancel := context.WithTimeout(ctx, preflightTimeout)
		err := checkVersion(versionCtx, bin, dir, env)
		cancel()
		if err != nil {
			return err
		}
		args = []string{"login", "--device-auth"}
	}
	cmd := commandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (r *runner) Open(ctx context.Context) error {
	bin, err := r.validate(ctx, r.models, false)
	if err == nil {
		r.binary = bin
	}
	return err
}

func (r *runner) ValidateModels(ctx context.Context, models ...string) error {
	_, err := r.validate(ctx, models, true)
	return err
}

// Use the same checks in doctor and at Open. Run checks again under the lock,
// since the CLI, managed configuration or login can change during a drain.
func (r *runner) validate(ctx context.Context, models []string, diagnostic bool) (string, error) {
	if platform.ExternalRunnersDisabled() {
		return "", fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	if err := validateModelNames(models...); err != nil {
		return "", err
	}
	bin, err := FindBinary()
	if err != nil {
		return "", err
	}
	lockCtx := ctx
	if diagnostic {
		var cancel context.CancelFunc
		lockCtx, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
	}
	unlock, err := loginLock(lockCtx, r.root)
	if errors.Is(err, context.DeadlineExceeded) {
		if diagnostic {
			return "", platform.ErrRunnerBusy
		}
		return "", fmt.Errorf("waiting for Codex login lock: %v", err)
	}
	if err != nil {
		return "", err
	}
	defer unlock()
	preflightCtx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	dir, err := scratchDir()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env, err := runnerEnv(dir, r.root)
	if err != nil {
		return "", err
	}
	raw, err := prepareCatalog(preflightCtx, bin, dir, env)
	if err != nil {
		return "", preflightError(ctx, err)
	}
	_, err = stripCatalog(raw, models...)
	return bin, err
}

func validModelName(model string) bool {
	return strings.TrimSpace(model) != "" && !strings.ContainsAny(model, "/\\ \t\r\n")
}

func validateModelNames(models ...string) error {
	for _, model := range models {
		if !validModelName(model) {
			return fmt.Errorf("Codex requires a bare model name for every stage; set mine_model/review_model to a Codex model slug")
		}
	}
	return nil
}

func checkVersion(ctx context.Context, bin, dir string, env []string) error {
	version, err := cliVersion(ctx, bin, dir, env)
	if err != nil {
		return err
	}
	if !supportsVersion(version) {
		return fmt.Errorf("Codex CLI %s has no verified tool-free contract; install one of %s and point WITNESS_CODEX_BINARY at it", version, strings.Join(testedVersions, ", "))
	}
	return nil
}

// Contract tests can exercise the newest CLI without admitting its version in production.
var supportsVersion = func(version string) bool {
	return slices.Contains(testedVersions, version)
}

func cliVersion(ctx context.Context, bin, dir string, env []string) (string, error) {
	cmd := commandContext(ctx, bin, "--version")
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = time.Second
	v, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) { // a clean exit; only a grandchild held stdout (see Run)
		return "", childError(ctx, "version", err, "")
	}
	return strings.TrimSpace(strings.TrimPrefix(string(v), "codex-cli ")), nil
}

func scratchDir() (string, error) {
	dir, err := os.MkdirTemp("", "witness-codex-")
	if err != nil {
		return "", err
	}
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); !os.IsNotExist(err) {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("Codex scratch directory must be outside a git repository")
		}
		if filepath.Dir(p) == p {
			return dir, nil
		}
	}
}

func runnerEnv(dir, root string) ([]string, error) {
	home := RunnerHome(root)
	if strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
		// A key-backed call does not need the saved ChatGPT login. Keep it outside
		// the subprocess home so even subcommands that ignore the key cannot use it.
		home = filepath.Join(dir, "home")
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	return childEnv(home), nil
}

func prepareCatalog(ctx context.Context, bin, dir string, env []string) ([]byte, error) {
	if err := checkVersion(ctx, bin, dir, env); err != nil {
		return nil, err
	}
	if err := checkManagedConfig(ctx, bin, dir, env); err != nil {
		return nil, err
	}
	if err := checkLogin(ctx, bin, dir, env); err != nil {
		return nil, err
	}
	cmd := commandContext(ctx, bin, append(isolationArgs(), "debug", "models")...)
	cmd.Env, cmd.Dir = env, dir
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) { // a clean exit; only a grandchild held stdout (see Run)
		return nil, childError(ctx, "catalog", err, stderr.String())
	}
	return raw, nil
}

// These flags remove configurable tools and all implicit user/project context.
// Catalog tools (apply_patch, exec/wait, clock) need stripCatalog as well.
func isolationArgs() []string {
	args := []string{}
	for _, setting := range []string{
		"features.shell_tool=false", "features.view_image=false", `web_search="disabled"`,
		"tools.experimental_request_user_input.enabled=false", "agents.enabled=false",
		"features.multi_agent=false", "features.goals=false", "features.image_generation=false",
		"features.apps=false", "features.plugins=false", "features.tool_suggest=false",
		"features.sleep_tool=false", "features.hooks=false", "features.skill_search=false",
		"skills.include_instructions=false", "skills.bundled.enabled=false",
		"features.reasoning_effort_override=false", "project_doc_max_bytes=0", "project_root_markers=[]",
		"include_permissions_instructions=false", "include_apps_instructions=false",
		"include_collaboration_mode_instructions=false", "include_environment_context=false", "analytics.enabled=false",
	} {
		args = append(args, "-c", setting)
	}
	return args
}

func stripCatalog(raw []byte, requested ...string) ([]byte, error) {
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("Codex model catalog: %w", err)
	}
	var models []map[string]any
	if err := json.Unmarshal(catalog["models"], &models); err != nil || len(models) == 0 {
		return nil, fmt.Errorf("Codex model catalog has no models")
	}
	found := make(map[string]bool, len(models))
	for _, m := range models {
		slug, ok := m["slug"].(string)
		if !ok || slug == "" || found[slug] {
			return nil, fmt.Errorf("Codex model catalog contains an invalid or duplicate slug")
		}
		found[slug] = true
		m["apply_patch_tool_type"], m["shell_type"], m["tool_mode"] = nil, "disabled", "direct"
		m["experimental_supported_tools"], m["supports_search_tool"] = []any{}, false
	}
	for _, model := range requested {
		if !found[model] {
			return nil, fmt.Errorf("Codex model %q is absent from this login's catalog", model)
		}
	}
	catalog["models"], _ = json.Marshal(models)
	return json.Marshal(catalog)
}

func configPath(key, path string) string {
	// JSON strings are TOML basic strings too, after normalizing Windows paths.
	b, _ := json.Marshal(filepath.ToSlash(path))
	return key + "=" + string(b)
}

// execArgs writes the instructions, and the stripped catalog unless it is nil, into dir
// and returns the argv of the generation call. Run and the self-check share it, so the
// check judges the exact request a transcript would ride in.
func execArgs(dir, cwd, model, prompt string, catalog []byte) ([]string, error) {
	promptPath := filepath.Join(dir, "system.md")
	if err := os.WriteFile(promptPath, []byte(prompt), 0600); err != nil {
		return nil, err
	}
	args := isolationArgs()
	if catalog != nil {
		catPath := filepath.Join(dir, "models.json")
		if err := os.WriteFile(catPath, catalog, 0600); err != nil {
			return nil, err
		}
		args = append(args, "-c", configPath("model_catalog_json", catPath))
	}
	args = append(args, "-c", configPath("model_instructions_file", promptPath))
	return append(args, "exec", "--ignore-user-config", "--ignore-rules", "--json", "--ephemeral", "--skip-git-repo-check", "-C", cwd, "-s", "read-only", "-m", model, "-"), nil
}

func (r *runner) Run(ctx context.Context, model, prompt, input string) (string, error) {
	if platform.ExternalRunnersDisabled() {
		return "", fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	if r.binary == "" {
		return "", fmt.Errorf("codex runner is not open")
	}
	if err := validateModelNames(model); err != nil {
		return "", err
	}
	unlock, err := loginLock(ctx, r.root)
	if err != nil {
		return "", err
	}
	defer unlock()
	// Keep the refresh lock until the process has exited, including on cancellation.
	dir, err := scratchDir()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env, err := runnerEnv(dir, r.root)
	if err != nil {
		return "", err
	}
	preflightCtx, stop := context.WithTimeout(ctx, preflightTimeout)
	raw, err := prepareCatalog(preflightCtx, r.binary, dir, env)
	stop()
	if err != nil {
		return "", preflightError(ctx, err)
	}
	cwd := filepath.Join(dir, "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		return "", err
	}
	catalog, err := stripCatalog(raw, model)
	if err != nil {
		return "", err
	}
	args, err := execArgs(dir, cwd, model, prompt, catalog)
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := commandContext(callCtx, r.binary, args...)
	cmd.Env, cmd.Dir, cmd.Stdin = env, cwd, strings.NewReader(input)
	cmd.WaitDelay = time.Second
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	// os/exec returns ErrWaitDelay only when the child itself exited 0 (an exit status or a kill
	// wins), so the output is complete and only a grandchild held the pipe.
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if callCtx.Err() != nil {
		return "", childError(callCtx, "exec", err, stderr.String())
	}
	answer, eventErr := parseEvents(out.Bytes(), time.Now())
	if eventErr != nil {
		var quota *platform.QuotaExhaustedError
		if errors.As(eventErr, &quota) && !r.plan {
			return "", errors.New(eventErr.Error()) // API credit, not a plan allowance: no pause
		}
		if err != nil {
			return "", fmt.Errorf("%w (codex exec: %v; %s)", eventErr, err, platform.StderrExcerpt(stderr.String()))
		}
		return "", eventErr
	}
	if err != nil {
		return "", childError(callCtx, "exec", err, stderr.String())
	}
	return answer, nil
}

// A failed preflight sent no generation request and must not narrow the drain.
func preflightError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return fmt.Errorf("Codex preflight: %v", err)
}

func childError(ctx context.Context, stage string, err error, stderr string) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return fmt.Errorf("codex %s: %w (%s)", stage, err, platform.StderrExcerpt(stderr))
}

func parseEvents(raw []byte, now time.Time) (string, error) {
	s := bufio.NewScanner(bytes.NewReader(raw))
	s.Buffer(make([]byte, 4096), 16<<20)
	var answer, failure string
	completed := false
	for s.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct{ Type, Text, Message string } `json:"item"`
		}
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			return "", fmt.Errorf("codex invalid event: %w", err)
		}
		switch e.Type {
		case "turn.completed":
			completed = true
		case "turn.failed":
			failure = e.Error.Message
			if failure == "" {
				failure = "turn failed"
			}
		case "error":
			failure = e.Message
		}
		if e.Type == "item.completed" && e.Item.Type == "agent_message" {
			answer = e.Item.Text
		}
		// Fail closed if the CLI ever drifts from the contract. No tool output can
		// be accepted as an extraction, even when the model later answers normally.
		if strings.HasPrefix(e.Type, "item.") && e.Item.Type != "" && e.Item.Type != "agent_message" && e.Item.Type != "reasoning" && e.Item.Type != "error" {
			return "", fmt.Errorf("codex tool-free contract violated: %s", e.Item.Type)
		}
		if e.Item.Type == "error" {
			failure = e.Item.Message
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	if completed && answer != "" {
		return answer, nil
	} // transient reconnects may precede success
	if failure != "" {
		if reset, ok := usageReset(failure, now); ok {
			return "", &platform.QuotaExhaustedError{Runtime: "codex", ResetAt: reset}
		}
		return "", errors.New("codex: " + platform.StderrExcerpt(failure))
	}
	return "", fmt.Errorf("codex returned no completed assistant answer")
}
