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
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The tool policy is a versioned protocol, not a sandbox policy: transcripts are
// untrusted and must never be offered tools, even read-only ones. New CLI versions
// need the wire contract checked before this range is extended.
const testedVersion = "0.169.0"

var runTimeout = 20 * time.Minute

var commandContext = exec.CommandContext

type runner struct {
	root, binary string
	models       []string
	plan         bool
	meta         store.MetaKV
}

func (Platform) NewRunner(cfg store.Config) platform.Runner {
	return &runner{root: cfg.RuntimeRoot, models: cfg.RunnerModels, meta: cfg.RuntimeMeta, plan: strings.TrimSpace(os.Getenv("CODEX_API_KEY")) == ""}
}

func (r *runner) OnPlanAllowance(string) bool { return r.plan }

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
	if runtime.GOOS != "windows" {
		return path, nil
	}
	return resolveWindowsCLI(path, runtime.GOARCH)
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
	for _, root := range []string{filepath.Join(dir, "node_modules", "@openai", "codex"), filepath.Join(dir, "..", "@openai", "codex")} {
		for _, vendor := range []string{
			filepath.Join(root, "node_modules", "@openai", "codex-win32-"+pkgArch, "vendor"),
			filepath.Join(filepath.Dir(root), "codex-win32-"+pkgArch, "vendor"),
			filepath.Join(root, "vendor"),
		} {
			candidate := filepath.Join(vendor, triple, "codex", "codex.exe")
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("cannot resolve Codex executable behind %s; set WITNESS_CODEX_BINARY to codex.exe", path)
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

func LoginReady(ctx context.Context, root string) bool {
	if platform.ExternalRunnersDisabled() {
		return false
	}
	if strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
		return true
	}
	unlock, err := store.RuntimeLock(ctx, filepath.Join(root, "codex"))
	if err != nil {
		return false
	}
	defer unlock()
	bin, err := FindBinary()
	if err != nil {
		return false
	}
	cmd := commandContext(ctx, bin, "login", "status")
	cmd.Env = childEnv(RunnerHome(root))
	return cmd.Run() == nil
}

// Login signs in directly to witness's own home. No credential file is read or
// copied from the coding account. The same lock covers login, catalog and exec.
func Login(ctx context.Context, root string) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	bin, err := FindBinary()
	if err != nil {
		return err
	}
	unlock, err := store.RuntimeLock(ctx, filepath.Join(root, "codex"))
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.MkdirAll(RunnerHome(root), 0700); err != nil {
		return err
	}
	cmd := commandContext(ctx, bin, "login", "--device-auth")
	cmd.Env = childEnv(RunnerHome(root))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (r *runner) Open(ctx context.Context) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	if r.root == "" {
		return fmt.Errorf("codex runner requires an archive runtime root")
	}
	if err := os.MkdirAll(RunnerHome(r.root), 0700); err != nil {
		return err
	}
	bin, err := FindBinary()
	if err != nil {
		return err
	}
	cmd := commandContext(ctx, bin, "--version")
	cmd.Env = childEnv(RunnerHome(r.root))
	v, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("codex version: %w", err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(string(v), "codex-cli "))
	if version != testedVersion && !strings.HasPrefix(version, testedVersion+"-") {
		return fmt.Errorf("codex %s has no verified tool-free contract; supported CLI: %s (including prereleases)", version, testedVersion)
	}
	if !LoginReady(ctx, r.root) {
		return fmt.Errorf("Codex runner needs its own login: run witness auth codex (or set CODEX_API_KEY)")
	}
	if err := r.ValidateModels(ctx, r.models...); err != nil {
		return err
	}
	r.binary = bin
	return nil
}

func (*runner) ValidateModels(_ context.Context, models ...string) error {
	for _, model := range models {
		if strings.TrimSpace(model) == "" || strings.ContainsAny(model, "/\\ \t\r\n") {
			return fmt.Errorf("Codex requires a bare model name for every stage; set mine_model/review_model to a Codex model slug")
		}
	}
	return nil
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
		"skills.include_instructions=false", "project_doc_max_bytes=0", "project_root_markers=[]",
		"include_permissions_instructions=false", "include_apps_instructions=false",
		"include_collaboration_mode_instructions=false", "include_environment_context=false", "analytics.enabled=false",
	} {
		args = append(args, "-c", setting)
	}
	return args
}

func stripCatalog(raw []byte, model string) ([]byte, error) {
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("Codex model catalog: %w", err)
	}
	var models []map[string]any
	if err := json.Unmarshal(catalog["models"], &models); err != nil || len(models) == 0 {
		return nil, fmt.Errorf("Codex model catalog has no models")
	}
	found := false
	for _, m := range models {
		if m["slug"] == model {
			found = true
		}
		m["apply_patch_tool_type"], m["shell_type"], m["tool_mode"] = nil, "disabled", "direct"
		m["experimental_supported_tools"], m["supports_search_tool"] = []any{}, false
	}
	if !found {
		return nil, fmt.Errorf("Codex model %q is absent from this login's catalog", model)
	}
	catalog["models"], _ = json.Marshal(models)
	return json.Marshal(catalog)
}

func configPath(key, path string) string {
	// JSON strings are TOML basic strings too, after normalizing Windows paths.
	b, _ := json.Marshal(filepath.ToSlash(path))
	return key + "=" + string(b)
}

func (r *runner) Run(ctx context.Context, model, prompt, input string) (string, error) {
	if platform.ExternalRunnersDisabled() {
		return "", fmt.Errorf("codex runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	if r.binary == "" {
		return "", fmt.Errorf("codex runner is not open")
	}
	if err := r.ValidateModels(ctx, model); err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	unlock, err := store.RuntimeLock(callCtx, filepath.Join(r.root, "codex"))
	if err != nil {
		return "", err
	}
	defer unlock()
	// Calls already admitted by cmd's gate can be waiting here when another
	// process exhausts the login. Recheck the same durable pause under the login
	// lock, and publish refusals before releasing it, so they send nothing.
	if r.plan && r.meta != nil {
		if reset, err := time.Parse(time.RFC3339, r.meta.MetaString(platform.UsagePauseKey("codex"))); err == nil && reset.After(time.Now()) {
			return "", &platform.QuotaExhaustedError{Runtime: "codex", ResetAt: reset}
		}
	}
	// Keep the refresh lock until the process has exited, including on cancellation.
	dir, err := os.MkdirTemp("", "witness-codex-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return "", fmt.Errorf("Codex scratch directory must be outside a git repository")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	cwd := filepath.Join(dir, "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		return "", err
	}
	env := childEnv(RunnerHome(r.root))
	cmd := commandContext(callCtx, r.binary, append(isolationArgs(), "debug", "models")...)
	cmd.Env, cmd.Dir = env, cwd
	var catErr bytes.Buffer
	cmd.Stderr = &catErr
	raw, err := cmd.Output()
	if err != nil {
		return "", childError(callCtx, "catalog", err, catErr.String())
	}
	catalog, err := stripCatalog(raw, model)
	if err != nil {
		return "", err
	}
	catPath, promptPath := filepath.Join(dir, "models.json"), filepath.Join(dir, "system.md")
	if err := os.WriteFile(catPath, catalog, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(promptPath, []byte(prompt), 0600); err != nil {
		return "", err
	}
	args := append(isolationArgs(), "-c", configPath("model_catalog_json", catPath), "-c", configPath("model_instructions_file", promptPath))
	args = append(args, "exec", "--ignore-user-config", "--ignore-rules", "--json", "--ephemeral", "--skip-git-repo-check", "-C", cwd, "-s", "read-only", "-m", model, "-")
	cmd = commandContext(callCtx, r.binary, args...)
	cmd.Env, cmd.Dir, cmd.Stdin = env, cwd, strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	if callCtx.Err() != nil {
		return "", childError(callCtx, "exec", err, stderr.String())
	}
	answer, eventErr := parseEvents(out.Bytes(), time.Now())
	if eventErr != nil {
		var quota *platform.QuotaExhaustedError
		if errors.As(eventErr, &quota) {
			if !r.plan {
				return "", errors.New(eventErr.Error())
			}
			if r.meta != nil {
				if err := r.meta.SetMetaString(platform.UsagePauseKey("codex"), quota.ResetAt.UTC().Format(time.RFC3339)); err != nil {
					return "", fmt.Errorf("codex could not persist the usage pause: %w", err)
				}
			}
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
