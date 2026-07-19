package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/platform"
	opencodeplugin "github.com/IngTian/witness/internal/platform/opencode/plugin"
	"github.com/IngTian/witness/internal/store"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// scaffoldDefaultDecision decides whether `install` seeds the built-in default lens.
// --no-default always wins (skip). Otherwise: ASK on an interactive terminal (stdin is
// a TTY), defaulting to yes on a bare Enter; and when NON-interactive (npm postinstall,
// CI, piped) scaffold WITHOUT prompting, so scripted installs never hang and a new
// personal user still gets a working setup. Mirrors the interactive precedent of
// `witness cleanup`, but TTY-guarded so it's safe in automation.
func scaffoldDefaultDecision(noDefault bool) bool {
	if noDefault {
		return false
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return true // non-interactive → scaffold the useful default, don't block on a prompt
	}
	fmt.Print("Scaffold the built-in \"default\" person-growth lens? It's the shipped starter lens — you can disable or edit it any time. [Y/n]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "n", "no":
		return false
	default: // "", "y", "yes", anything else → yes (default-on)
		return true
	}
}

func newInstallCmd() *cobra.Command {
	var noDefault bool
	c := &cobra.Command{
		Use:    "install <claude|opencode>",
		Short:  "Install witness integrations.",
		Long:   "Install the Claude Code integration (hooks + MCP) or the OpenCode integration (plugin + MCP). The target is required so install always binds the matching distillation runtime.\n\nInstall also SCAFFOLDS the built-in \"default\" person-growth lens into your archive, since #44 slice 1a made default an ordinary registered lens rather than an always-on built-in. On a terminal it ASKS first; when non-interactive (npm/CI/piped) it scaffolds by default. Pass --no-default to always skip and start with no lenses (register your own).",
		Hidden: os.Getenv("WITNESS_NPM_PACKAGE") == "1",
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdInstall(args, noDefault)
		},
	}
	c.Flags().BoolVar(&noDefault, "no-default", false, "always skip scaffolding the built-in default lens (no interactive prompt; start with no lenses)")
	return c
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "uninstall <claude|opencode>",
		Short:  "Remove witness integrations without deleting data.",
		Long:   "Remove the Claude Code or OpenCode integration. The witness data store and config are left untouched.",
		Hidden: os.Getenv("WITNESS_NPM_PACKAGE") == "1",
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdUninstall(args)
		},
	}
}

// hookCmd / hookEntry mirror the settings.json hooks schema for the entries we own.
// Args is the Claude Code exec form: when present, CC spawns Command directly with
// these args and NO shell (used on Windows, where the bash shim can't run). When
// Args is empty, Command is a shell-form string (Unix, via the witness.sh shim).
type hookCmd struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Async   bool     `json:"async,omitempty"`
}
type hookEntry struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

// hookInvocation is how the installed hooks call witness — the one thing that
// differs by platform. Unix uses shell form through the witness.sh shim; Windows
// uses exec form pointing at the installed witness.exe (no shell). The merge/
// detect/remove logic is otherwise identical, so it stays platform-independent
// and fully unit-testable on any OS; only resolveClaudeInstall (GOOS-split) picks
// which invocation to build.
type hookInvocation struct {
	execForm bool   // true => spawn target directly with args (Windows)
	target   string // shell form: the shim path; exec form: the witness.exe path
}

func shellInvocation(shim string) hookInvocation {
	return hookInvocation{execForm: false, target: shim}
}
func execInvocation(exe string) hookInvocation { return hookInvocation{execForm: true, target: exe} }

// mcpTarget is the executable settings.json / `claude mcp add` should invoke for
// the MCP server: the shim on Unix, the installed exe on Windows.
func (inv hookInvocation) mcpTarget() string { return inv.target }

// shellQuote single-quotes a path for safe use in a shell-executed hook command,
// POSIX-escaping any embedded single quote (close the quote, backslash-escape the
// quote, reopen). Claude Code runs hook commands through a shell, so an absolute
// path containing a space or shell metacharacter (e.g. ~/My Projects/...) would
// otherwise word-split and the hook would silently fail. The plugin path
// (hooks/hooks.json) uses double quotes because it needs ${CLAUDE_PLUGIN_ROOT}
// expanded; here the shim is an already-resolved literal path, so single quotes
// (no expansion) are safest.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// witnessHookSpecs is the canonical hook wiring for the given invocation. On Unix
// (shell form) it renders `'<shim>' <token>` command strings, kept in lockstep
// with hooks/hooks.json (the plugin-install path uses that file). On Windows
// (exec form) it renders {command: <exe>, args: [<token>]} so CC spawns the
// binary directly with no shell.
func witnessHookSpecs(inv hookInvocation) []struct {
	Event string
	Entry hookEntry
} {
	// hook builds one command for a subcommand token, in the right form.
	hook := func(token string, async bool) hookCmd {
		if inv.execForm {
			return hookCmd{Type: "command", Command: inv.target, Args: []string{token}, Async: async}
		}
		return hookCmd{Type: "command", Command: shellQuote(inv.target) + " " + token, Async: async}
	}
	return []struct {
		Event string
		Entry hookEntry
	}{
		{"SessionStart", hookEntry{Matcher: "startup|clear|compact|resume", Hooks: []hookCmd{hook("session-start", false)}}},
		{"UserPromptSubmit", hookEntry{Hooks: []hookCmd{hook("capture", true)}}},
		{"Stop", hookEntry{Hooks: []hookCmd{hook("capture", true)}}},
		{"SessionEnd", hookEntry{Hooks: []hookCmd{hook("session-end", true)}}},
	}
}

// witnessHookTokens is the exact set of subcommand tokens our installed hooks
// emit (see witnessHookSpecs). isWitnessEntry keys exec-form detection on these
// so we only ever match hooks WE wrote, never a foreign tool.
var witnessHookTokens = map[string]bool{"session-start": true, "capture": true, "session-end": true}

// isWitnessEntry reports whether a parsed hook entry is one of ours — so
// re-install/uninstall can find and replace them idempotently. It must recognize
// BOTH invocation forms, else a re-install (or a shell<->exec migration) would
// fail to dedupe and orphan duplicate hooks:
//   - shell form (Unix): the command's FIRST token basename is exactly "witness.sh"
//     AND its LAST token is one of our subcommand tokens.
//   - exec form (Windows): command basename is the witness binary AND the first
//     arg is one of our own subcommand tokens.
//
// Both matches are intentionally narrow (issue #49 I3/I4): matching a bare "witness"
// basename — or a naked `strings.Contains(c, "witness.sh")` substring — would strip
// a user's UNRELATED hook that merely CONTAINS the string (e.g. "/opt/notwitness.sh
// deploy", "/opt/witness.showcase/run.sh", or the testifysec "witness" CLI). Those
// are foreign hooks we are contractually required to preserve. Keying on an exact
// basename PLUS one of OUR tokens matches only what witnessHookSpecs actually writes.
func isWitnessEntry(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		c, _ := hm["command"].(string)
		if isWitnessShimCommand(c) {
			return true // shell-form shim
		}
		if isWitnessBinary(c) && firstArgIsWitnessToken(hm["args"]) {
			return true // exec-form binary invoking one of our subcommands
		}
	}
	return false
}

// isWitnessShimCommand reports whether a shell-form command string is one WE wrote:
// `'<path>/witness.sh' <token>`. It requires the first whitespace-separated token's
// basename to equal exactly "witness.sh" AND the last token to be one of our
// subcommand tokens — so a foreign command that merely contains the substring
// "witness.sh" (e.g. "notwitness.sh", "witness.sha256") is NOT matched. Mirrors the
// exec-form basename-exact + token discipline (issue #49 I3).
func isWitnessShimCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return false
	}
	// The shim path is shell-quoted by shellQuote (single quotes); strip them so the
	// basename check sees the real path. A bare unquoted path also works.
	first := strings.Trim(fields[0], "'\"")
	base := first
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if !strings.EqualFold(base, "witness.sh") {
		return false
	}
	return witnessHookTokens[fields[len(fields)-1]]
}

// isWitnessBinary reports whether an exec-form command path's basename is the
// witness binary (witness or witness.exe), matched by basename so it works
// whether the installer wrote a plain "witness.exe" or the path is absolute.
// Uses both path separators because a Windows path may be inspected on any OS.
// Note: this is deliberately EXACT ("witness"), not a "witness-" prefix — the
// installer always copies to a bare witness.exe, so the prefix form matched
// nothing we write while risking foreign "witness-*" false positives.
func isWitnessBinary(cmd string) bool {
	base := cmd
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return strings.EqualFold(base, "witness") || strings.EqualFold(base, "witness.exe")
}

// firstArgIsWitnessToken reports whether the exec-form args list begins with one
// of our own subcommand tokens (session-start/capture/session-end).
func firstArgIsWitnessToken(args any) bool {
	list, ok := args.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	tok, _ := list[0].(string)
	return witnessHookTokens[tok]
}

// appendToPathValue returns the PATH-style, ';'-separated string with dir added,
// and whether a change was needed. Idempotent: a case-insensitive match of dir
// among existing entries (Windows paths are case-insensitive) means no change.
// Pure string logic, split out from the Windows registry code so it is unit-
// testable on any OS. The existing value is preserved VERBATIM (including any
// %VAR% tokens) — the caller writes it back as REG_EXPAND_SZ, which is why we
// must never flatten it (the setx bug).
func appendToPathValue(current, dir string) (string, bool) {
	for _, e := range strings.Split(current, ";") {
		if strings.EqualFold(strings.TrimSpace(e), dir) {
			return current, false // already present
		}
	}
	if current == "" {
		return dir, true
	}
	// Preserve a trailing ';' shape rather than doubling it.
	if strings.HasSuffix(current, ";") {
		return current + dir, true
	}
	return current + ";" + dir, true
}

// mergeWitnessHooks adds the witness hooks to a settings.json document, replacing
// any existing witness entries (idempotent, across BOTH invocation forms) and
// preserving all other hooks and settings verbatim.
func mergeWitnessHooks(data []byte, inv hookInvocation) ([]byte, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse settings: %w", err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, spec := range witnessHookSpecs(inv) {
		existing, _ := hooks[spec.Event].([]any)
		kept := []any{}
		for _, e := range existing {
			if !isWitnessEntry(e) {
				kept = append(kept, e) // preserve foreign hooks
			}
		}
		kept = append(kept, spec.Entry)
		hooks[spec.Event] = kept
	}
	root["hooks"] = hooks
	return json.MarshalIndent(root, "", "  ")
}

// removeWitnessHooks strips our hook entries, leaving foreign hooks and other
// settings intact. Empty event arrays are dropped.
func removeWitnessHooks(data []byte) ([]byte, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse settings: %w", err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	for event, v := range hooks {
		entries, _ := v.([]any)
		kept := []any{}
		for _, e := range entries {
			if !isWitnessEntry(e) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return json.MarshalIndent(root, "", "  ")
}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func opencodeDir() string {
	if d := os.Getenv("OPENCODE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode")
}

func opencodeConfigPath() string {
	if p := os.Getenv("OPENCODE_CONFIG"); strings.TrimSpace(p) != "" {
		return p
	}
	dir := opencodeDir()
	jsonPath := filepath.Join(dir, "opencode.json")
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath
	}
	jsoncPath := filepath.Join(dir, "opencode.jsonc")
	if _, err := os.Stat(jsoncPath); err == nil {
		return jsoncPath
	}
	return jsonPath
}

const (
	openCodePluginName       = "witness.js"
	legacyOpenCodePluginName = "claude-witness.js"
)

func openCodePluginPath(dir, name string) string {
	return filepath.Join(dir, "plugins", name)
}

func removeLegacyOpenCodePlugins(dir string) {
	_ = os.Remove(openCodePluginPath(dir, legacyOpenCodePluginName))
}

func removeAllOpenCodePlugins(dir string) {
	for _, name := range []string{openCodePluginName, legacyOpenCodePluginName} {
		_ = os.Remove(openCodePluginPath(dir, name))
	}
}

// repoShim resolves <repo>/hooks/witness.sh from the running binary at <repo>/bin/.
func repoShim() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	repo := filepath.Dir(filepath.Dir(exe)) // bin/ -> repo
	shim := filepath.Join(repo, "hooks", "witness.sh")
	if _, err := os.Stat(shim); err != nil {
		return "", fmt.Errorf("shim not found at %s (run from a built working copy: make build): %w", shim, err)
	}
	return shim, nil
}

// cmdInstall wires witness into Claude Code or OpenCode. The target is required
// (enforced by cobra ExactArgs) so install always binds the matching distillation
// runtime into config.toml.
func cmdInstall(args []string, noDefault bool) error {
	target := installTarget(args)
	in, ok := platform.InstallerFor(target)
	if !ok {
		return fmt.Errorf("unknown install target %q (want %s)", target, strings.Join(platform.InstallTargets(), " or "))
	}
	if err := in.Install(); err != nil {
		return err
	}
	if err := bindRunner(target); err != nil {
		return err
	}
	// Scaffold the built-in default person-growth lens (the tool preset). Since #44
	// slice 1a default is an ordinary registered lens with no always-on status, a fresh
	// install must seed+enable it explicitly or the archive starts with zero lenses. The
	// decision: --no-default always skips; otherwise ASK on a terminal, and scaffold by
	// default when non-interactive (npm/CI/piped) so scripted installs never hang and a
	// new personal user gets a working setup. Best-effort: a seeding failure shouldn't
	// fail the whole install (the integration is already wired). The pre-1a migration
	// hook handles EXISTING archives separately.
	if scaffoldDefaultDecision(noDefault) {
		st, err := store.Open()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witness: installed, but could not open the archive to scaffold the default lens: %v\n", err)
			return nil
		}
		defer st.Close()
		// Rerunning `install` is the natural "restore the default lens" gesture, so make
		// it idempotently ensure default is BOTH registered AND enabled — not just skip
		// when it happens to be registered. That covers all three states a user can be in:
		//   - deregistered (gone from the registry) → seedDefaultLens re-registers + enables;
		//   - disabled (registered, not enabled)    → seedDefaultLens re-enables (register
		//                                              overwrites the same bundled def, harmless);
		//   - present + enabled                      → a harmless no-op re-seed.
		// seedDefaultLens = RegisterLens(bundled default) + EnableLens, both idempotent.
		already := slices.Contains(st.RegisteredLenses(), store.LensDefault) &&
			slices.Contains(st.LoadConfig().EnabledLenses, store.LensDefault)
		if err := seedDefaultLens(st); err != nil {
			fmt.Fprintf(os.Stderr, "witness: installed, but could not scaffold the default lens (re-run `witness install` to retry): %v\n", err)
			return nil
		}
		if !already {
			fmt.Println("scaffolded the built-in 'default' person-growth lens (disable it any time with `witness lens disable default`, or re-run install with --no-default to skip).")
		}
	}
	return nil
}

// bindRunner pins config.toml's runner field to the integration that was just
// wired, so distillation uses the same agent runtime the user installed for.
// store.Open() already laid down the full template if config was missing; here
// we only flip runner. Best-effort: a store open failure (rare; Open mkdirs) is
// logged but does not fail install, since hooks/plugin are already in place.
func bindRunner(target string) error {
	st, err := store.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witness: could not open store to set runner: %v\n", err)
		return nil
	}
	defer st.Close()
	if err := st.SetRunner(target); err != nil {
		return fmt.Errorf("write runner to config: %w", err)
	}
	fmt.Printf("runner set to %s in witness config.toml\n", target)
	return nil
}

func installTarget(args []string) string {
	return strings.ToLower(strings.TrimSpace(args[0]))
}

// cmdInstallClaude wires the witness hooks into the user's Claude settings.json
// and registers the MCP server, both idempotently. The only platform-specific
// step is resolveClaudeInstall (GOOS-split): Unix returns a shell-form invocation
// through the in-repo witness.sh shim; Windows copies the binary + assets to a
// stable install dir and returns an exec-form invocation pointing at it. Every-
// thing below is platform-independent.
func cmdInstallClaude() error {
	inv, err := resolveClaudeInstall()
	if err != nil {
		return err
	}
	settings := filepath.Join(claudeDir(), "settings.json")
	if err := os.MkdirAll(claudeDir(), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(settings)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, err := mergeWitnessHooks(data, inv)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(settings, merged); err != nil {
		return err
	}
	fmt.Printf("hooks wired into %s\n", settings)

	// Register the MCP server (idempotent: skip if already present).
	if out, _ := exec.Command("claude", "mcp", "list").CombinedOutput(); !mcpServerRegistered(string(out), "witness") {
		if err := exec.Command("claude", "mcp", "add", "-s", "user", "witness", inv.mcpTarget(), "mcp").Run(); err != nil {
			fmt.Fprintf(os.Stderr, "witness: could not register MCP server (is `claude` on PATH?): %v\n", err)
		} else {
			fmt.Println("MCP server 'witness' registered")
		}
	} else {
		fmt.Println("MCP server 'witness' already registered")
	}
	fmt.Println("done — restart Claude Code (or open /hooks) to load the hooks.")
	fmt.Println("note: witness collects everywhere; the profile is never injected into a session.")
	fmt.Println("      agents read it on demand via the witness MCP tools;")
	fmt.Println("      you can read it yourself with `witness profile`.")
	return nil
}

// mcpServerRegistered reports whether `claude mcp list` output already lists a
// server of exactly `name`. `claude mcp list` prints one server per line as
// "<name>: <command...>", so we match the token before the first colon per line —
// NOT a whole-output substring (issue #54 minor): a plain Contains(out, "witness")
// treated an unrelated server like "eyewitness" or "witness-notes" as already-
// registered and silently skipped registering our own.
func mcpServerRegistered(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		serverName, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.TrimSpace(serverName) == name {
			return true
		}
	}
	return false
}

// cmdInstallOpenCode installs a global OpenCode plugin that mirrors completed
// OpenCode sessions into witness, and registers the same MCP server OpenCode-side.
func cmdInstallOpenCode() error {
	shim, err := repoShim()
	if err != nil {
		return err
	}
	dir := opencodeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	config := opencodeConfigPath()
	data, err := os.ReadFile(config)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, err := mergeOpenCodeMCP(data, shim)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		return err
	}

	plugins := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		return err
	}
	pluginPath := openCodePluginPath(dir, openCodePluginName)
	if err := writeFileAtomic(pluginPath, []byte(opencodeplugin.Source(shim))); err != nil {
		return err
	}
	removeLegacyOpenCodePlugins(dir)
	fmt.Printf("OpenCode plugin installed at %s\n", pluginPath)

	if err := writeFileAtomic(config, merged); err != nil {
		return err
	}
	fmt.Printf("OpenCode MCP server 'witness' registered in %s\n", config)
	fmt.Println("done — restart OpenCode so the plugin and MCP server load.")
	return nil
}

// cmdUninstall removes the witness hooks/plugin and MCP server (idempotent).
// Data, config, and the runner setting are left untouched.
func cmdUninstall(args []string) error {
	target := installTarget(args)
	in, ok := platform.InstallerFor(target)
	if !ok {
		return fmt.Errorf("unknown uninstall target %q (want %s)", target, strings.Join(platform.InstallTargets(), " or "))
	}
	return in.Uninstall()
}

func cmdUninstallClaude() error {
	settings := filepath.Join(claudeDir(), "settings.json")
	if data, err := os.ReadFile(settings); err == nil {
		if cleaned, err := removeWitnessHooks(data); err == nil {
			_ = writeFileAtomic(settings, cleaned)
			fmt.Printf("hooks removed from %s\n", settings)
		}
	}
	_ = exec.Command("claude", "mcp", "remove", "witness").Run()
	fmt.Println("MCP server 'witness' removed (if it was present)")
	return nil
}

func cmdUninstallOpenCode() error {
	dir := opencodeDir()
	config := opencodeConfigPath()
	configUpdated := false
	if data, err := os.ReadFile(config); err == nil {
		cleaned, err := removeOpenCodeMCP(data)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(config, cleaned); err != nil {
			return err
		}
		configUpdated = true
	} else if !os.IsNotExist(err) {
		return err
	}
	removeAllOpenCodePlugins(dir)
	fmt.Printf("OpenCode plugin removed from %s (if it was present)\n", openCodePluginPath(dir, openCodePluginName))
	fmt.Printf("Legacy OpenCode plugin removed from %s (if it was present)\n", openCodePluginPath(dir, legacyOpenCodePluginName))
	if configUpdated {
		fmt.Println("OpenCode MCP server 'witness' removed from OpenCode config (if it was present)")
	} else {
		fmt.Println("OpenCode config not found; no MCP entry removed")
	}
	return nil
}

func mergeOpenCodeMCP(data []byte, shim string) ([]byte, error) {
	root, err := parseOpenCodeConfig(data)
	if err != nil {
		return nil, err
	}
	if root["$schema"] == nil {
		root["$schema"] = "https://opencode.ai/config.json"
	}
	mcp, _ := root["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	mcp["witness"] = map[string]any{
		"type":    "local",
		"command": []any{shim, "mcp"},
		"enabled": true,
	}
	root["mcp"] = mcp
	return json.MarshalIndent(root, "", "  ")
}

func removeOpenCodeMCP(data []byte) ([]byte, error) {
	root, err := parseOpenCodeConfig(data)
	if err != nil {
		return nil, err
	}
	mcp, _ := root["mcp"].(map[string]any)
	delete(mcp, "witness")
	if len(mcp) == 0 {
		delete(root, "mcp")
	} else {
		root["mcp"] = mcp
	}
	return json.MarshalIndent(root, "", "  ")
}

func parseOpenCodeConfig(data []byte) (map[string]any, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(normalizeJSONC(data), &root); err != nil {
		return nil, fmt.Errorf("parse opencode config: %w", err)
	}
	return root, nil
}

func normalizeJSONC(data []byte) []byte {
	return removeTrailingCommas(stripJSONComments(data))
}

func stripJSONComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(data) && data[i+1] == '/' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, data[i])
			}
			continue
		}
		if c == '/' && i+1 < len(data) && data[i+1] == '*' {
			i += 2
			for i < len(data)-1 && !(data[i] == '*' && data[i+1] == '/') {
				if data[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i++
			continue
		}
		out = append(out, c)
	}
	return out
}

func removeTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && isJSONWhitespace(data[j]) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}

// writeFileAtomic writes via temp + rename so a crash can't leave a half-written
// settings.json (which would silently disable ALL of the user's settings).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
