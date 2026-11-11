package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	opencodeplugin "github.com/IngTian/witness/internal/platform/opencode/plugin"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newWireCmd() *cobra.Command {
	c := &cobra.Command{
		// Built from the installer registry, not written out: a literal list went stale
		// the moment a venue was added, and the staleness is user-facing — the help would
		// omit a target that works, or name one that is not registered.
		Use:     "wire <" + strings.Join(editorNames(), "|") + ">",
		GroupID: groupSetup,
		Short:   "Connect an editor integration (hooks/plugin + MCP) to witness.",
		Long: strings.TrimSpace(`
Wire witness into your editor: Claude Code (hooks + MCP), OpenCode (plugin + MCP)
or Codex (hooks).

This sets up CAPTURE only. It never prompts (safe in CI / as a hook), and it does
not seed content — the built-in "default" lens is added automatically the first
time your archive is opened.

  runner    wire writes a sensible default (matching the editor); change it
            any time with: witness config set runner <claude|opencode>
            Codex runs no model, so wiring it leaves the runner alone.
  codex     Codex hooks are inert until you approve them with /hooks in Codex;
            wiring cannot grant that trust for you.
  default   remove it with 'witness lens disable/deregister default' (it stays
            gone); restore with 'witness lens load-default'; opt a fresh archive
            out entirely with WITNESS_NO_DEFAULT_LENS=1`),
		Hidden: os.Getenv("WITNESS_NPM_PACKAGE") == "1",
		Args:   editorArg("wire"),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdInstall(args)
		},
	}
	return c
}

// editorArg is wire/unwire's argument check, worded for a person: cobra's ExactArgs said "accepts
// 1 arg(s), received 0", which names neither the missing thing nor the choices.
func editorArg(verb string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("which editor? witness %s <%s>", verb, strings.Join(editorNames(), "|"))
		}
		return nil
	}
}

// unknownEditor is the one wording for an editor witness cannot wire, in the registry's order.
func unknownEditor(target string) error {
	return fmt.Errorf("unknown editor %q — want one of: %s", target, strings.Join(editorNames(), ", "))
}

func newUnwireCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unwire <" + strings.Join(editorNames(), "|") + ">",
		GroupID: groupSetup,
		Short:   "Remove an editor integration (data/config untouched).",
		Long:    "Remove the Claude Code, OpenCode or Codex integration. The witness data store and config are left untouched.",
		Hidden:  os.Getenv("WITNESS_NPM_PACKAGE") == "1",
		Args:    editorArg("unwire"),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdUninstall(args)
		},
	}
}

func newInstallCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:     "install",
		GroupID: groupSetup,
		Short:   "Provision a witness archive (no editor; use --path for an isolated instance).",
		Long: strings.TrimSpace(`
Create (or adopt) a witness archive — its database + config — at a location. This
provisions witness itself; it does NOT wire any editor. Run several with different
--path values to keep independent archives (e.g. one per data source).

  --path <dir>   archive location (default: the standard witness data dir)

A bare archive starts with NO lenses: register your own with 'witness lens register',
then feed it with 'witness ingest'. To connect an editor instead, use 'witness wire'.`),
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return cmdProvision(path) },
	}
	c.Flags().StringVar(&path, "path", "", "archive location (default: the standard data dir)")
	return c
}

// hookCmd / hookEntry mirror the settings.json hooks schema for the entries we own.
// Args is the Claude Code exec form: when present, CC spawns Command directly with
// these args and NO shell (used on Windows, where the bash shim can't run). When
// Args is empty, Command is a shell-form string (Unix, via the witness.sh shim).
// CommandWindows is the CODEX form: Codex runs `command` through a shell and takes an
// optional Windows-only `commandWindows` override (PowerShell) instead of Claude's
// exec-form Args. omitempty keeps it out of every document we write for Claude Code.
type hookCmd struct {
	Type           string   `json:"type"`
	Command        string   `json:"command,omitempty"`
	CommandWindows string   `json:"commandWindows,omitempty"`
	Args           []string `json:"args,omitempty"`
	Async          bool     `json:"async,omitempty"`
	// Timeout is seconds, Codex-only. Used for SessionEnd, which Codex refuses to run
	// asynchronously and defaults to a 1-second budget. See codexHookSpecs.
	Timeout int `json:"timeout,omitempty"`
}
type hookEntry struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

// hookSpec is one event's worth of wiring we own. Named (rather than an anonymous
// struct) because two venues now build these: Claude Code's settings.json and Codex's
// hooks.json use the SAME event → matcher-group → handler-list schema, so they share
// mergeHooks/removeHooks and differ only in the specs and the ownership predicate.
type hookSpec struct {
	Event string
	Entry hookEntry
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

// shellQuote single-quotes a path for safe use in a shell-executed hook command,
// POSIX-escaping any embedded single quote (close the quote, backslash-escape the
// quote, reopen). Claude Code runs hook commands through a shell, so an absolute
// path containing a space or shell metacharacter (e.g. ~/My Projects/...) would
// otherwise word-split and the hook would silently fail. The shim is an
// already-resolved literal path, so single quotes (no expansion) are safest.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// witnessHookSpecs is the canonical hook wiring for the given invocation. On Unix
// (shell form) it renders `'<shim>' <token>` command strings. On Windows
// (exec form) it renders {command: <exe>, args: [<token>]} so CC spawns the
// binary directly with no shell.
func witnessHookSpecs(inv hookInvocation) []hookSpec {
	// hook builds one command for a subcommand token, in the right form.
	hook := func(token string, async bool) hookCmd {
		if inv.execForm {
			return hookCmd{Type: "command", Command: inv.target, Args: []string{token}, Async: async}
		}
		return hookCmd{Type: "command", Command: shellQuote(inv.target) + " " + token, Async: async}
	}
	return []hookSpec{
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
	// Split off the FIRST word the way a shell would, honoring the quoting shellQuote
	// applied. Splitting on raw whitespace would break the round trip for a shim path
	// containing a space (~/My Projects/... — the very case shellQuote exists to protect):
	// fields[0] would be `'/Users/me/My`, its basename `My`, and witness would fail to
	// recognize a hook it wrote itself — so every re-install appended a duplicate (capture
	// firing N times per prompt) and unwire stripped nothing (uninstall silently leaving
	// witness wired in).
	first, rest, ok := splitFirstShellWord(cmd)
	if !ok || rest == "" {
		return false
	}
	base := first
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if !strings.EqualFold(base, "witness.sh") {
		return false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return false
	}
	return witnessHookTokens[fields[len(fields)-1]]
}

// splitFirstShellWord returns the first shell word of s (unquoted) and the remainder.
// It handles the single-quoted form shellQuote writes — including its POSIX escape for
// an embedded quote ('\”) — plus double quotes and bare unquoted words, which is the
// full set of shapes a hook command we wrote (or a human hand-edited) can take. ok is
// false only for an empty/blank command.
func splitFirstShellWord(s string) (word, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			return b.String(), strings.TrimLeft(s[i:], " \t"), true
		case c == '\'':
			i++ // opening quote
			for i < len(s) && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
			i++ // closing quote
			// shellQuote renders an embedded quote as '\'' — after the closing quote that
			// leaves \'' ; consume the escaped quote and let the reopened quote continue.
			if i+1 < len(s) && s[i] == '\\' && s[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
			}
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				b.WriteByte(s[i])
				i++
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), "", true
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
	return mergeHooks(data, witnessHookSpecs(inv), isWitnessEntry)
}

// removeWitnessHooks strips our hook entries, leaving foreign hooks and other
// settings intact. Empty event arrays are dropped.
func removeWitnessHooks(data []byte) ([]byte, error) {
	return removeHooks(data, isWitnessEntry)
}

// mergeHooks adds specs to a hooks document, replacing any entry isOurs matches
// (idempotent) and preserving everything else verbatim. Shared by Claude Code's
// settings.json and Codex's hooks.json — the two formats agree on the hooks subtree,
// so only the specs and the ownership predicate differ.
//
// isOurs is a PARAMETER, not a hardcoded isWitnessEntry, because the two venues are
// recognized by different evidence: Claude's hooks carry our subcommand tokens
// (session-start/capture/session-end), Codex's carry `import --agent codex`. Sharing
// one predicate across both would make each venue's uninstall strip the other's hooks.
func mergeHooks(data []byte, specs []hookSpec, isOurs func(any) bool) ([]byte, error) {
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
	for _, spec := range specs {
		existing, _ := hooks[spec.Event].([]any)
		kept := []any{}
		for _, e := range existing {
			if !isOurs(e) {
				kept = append(kept, e) // preserve foreign hooks
			}
		}
		kept = append(kept, spec.Entry)
		hooks[spec.Event] = kept
	}
	root["hooks"] = hooks
	return json.MarshalIndent(root, "", "  ")
}

// removeHooks strips entries isOurs matches, leaving foreign hooks and every other
// setting intact. Empty event arrays are dropped, and an emptied hooks subtree is
// removed entirely.
func removeHooks(data []byte, isOurs func(any) bool) ([]byte, error) {
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
			if !isOurs(e) {
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

// removeAllOpenCodePlugins deletes our plugin and its legacy name, returning the paths it actually
// removed — so unwire reports what happened instead of "(if it was present)" on every run.
func removeAllOpenCodePlugins(dir string) (removed []string) {
	for _, name := range []string{openCodePluginName, legacyOpenCodePluginName} {
		p := openCodePluginPath(dir, name)
		if os.Remove(p) == nil {
			removed = append(removed, p)
		}
	}
	return removed
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

// cmdProvision opens (creating) an archive at path without wiring any editor. It opts
// the archive OUT of the person-growth default-lens auto-seed — a provisioned archive is
// a blank engine the caller fills with its own lens(es). (wire, by contrast, leaves the
// env unset so an editor user DOES get the default lens.)
//
// It says whether it CREATED the archive or adopted one already there, and its next steps carry
// WITNESS_HOME when the archive is not the one every command would use anyway: printed bare, they
// acted on the DEFAULT archive, because nothing but WITNESS_HOME selects another.
func cmdProvision(path string) error {
	prevHome := os.Getenv("WITNESS_HOME")
	if strings.TrimSpace(path) != "" {
		if err := os.Setenv("WITNESS_HOME", path); err != nil {
			return err
		}
	}
	// Suppress the person default on this fresh archive (read live by the first-open
	// seed hook, see defaultseed.go). Set BEFORE Open.
	_ = os.Setenv(noDefaultLensEnv, "1")
	existed := false
	if strings.TrimSpace(path) != "" {
		_, err := os.Stat(filepath.Join(path, "witness.db"))
		existed = err == nil
	}
	st, err := store.Open()
	if err != nil {
		if strings.TrimSpace(path) != "" {
			return fmt.Errorf("cannot create an archive at %s: %w", tildePath(path), err)
		}
		return err
	}
	defer st.Close()
	root, _ := filepath.Abs(st.Root)
	sessions := dim("(" + countOf(st.Stats(activeLensNames(st)).Sessions, "session", "sessions") + ")")
	switch {
	case strings.TrimSpace(path) == "": // the default data dir, which Open may have created or adopted
		fmt.Printf("%s archive ready at %s %s\n", okGlyph(), tildePath(root), sessions)
	case existed:
		fmt.Printf("%s archive adopted at %s %s\n", okGlyph(), tildePath(root), sessions)
	default:
		fmt.Printf("%s archive created at %s %s\n", okGlyph(), tildePath(root), dim("(no lenses yet)"))
	}
	prefix := ""
	if strings.TrimSpace(path) != "" && filepath.Clean(prevHome) != filepath.Clean(root) {
		prefix = "WITNESS_HOME=" + shellArg(root) + " "
		fmt.Println(dim("  every command uses it when WITNESS_HOME points there:"))
	}
	fmt.Println(hint("add a lens", prefix+"witness lens register <name> <dir>"))
	fmt.Println(hint("feed it", prefix+"witness ingest --file <records.ndjson>"))
	return nil
}

// cmdInstall is `witness wire <editor>`. The target is required (enforced by cobra
// ExactArgs) so wiring binds the matching distillation runtime into config.toml when
// that editor can run a model (Codex cannot; see below). Since #102 wiring is a pure MECHANISM — hooks/plugin + MCP
// + bind runner — and never prompts nor seeds content: the built-in default lens is
// auto-seeded once by the first-open hook (which fires the moment bindRunner opens the
// store) and is fully deletable. Keeping install prompt-free is what makes it safe to
// run as a hook subprocess and in CI, where a stray prompt would hang the session.
//
// The screen is a title, one glyph row per part (the installer's, then runner and lenses), and the
// result line LAST. The result used to sit in the middle ("done — restart Claude Code" above the
// runner and lens lines), so the installer returns it as a wireOutcome for cmdInstall to print.
func cmdInstall(args []string) error {
	target := installTarget(args)
	ed, ok := editors[target]
	if !ok {
		return unknownEditor(target)
	}
	fmt.Println(title("wire "+target, "", ""))
	fmt.Println()
	out, err := ed.wire()
	if err != nil {
		return err
	}
	// Bind the default runner only if this venue can actually RUN a model. A capture
	// source need not provide a distillation runner (AGENTS.md), and Codex is the first
	// that does not: writing runner=codex would put a value in config.toml that
	// RunnerForName cannot resolve, so every later drain would fail and the archive
	// would silently stop distilling — a worse outcome than not wiring a runner at all.
	//
	// The test is ValidateRunnerName, which derives from the registry, rather than a
	// target == "codex" check: a venue that later gains a RunnerProvider starts binding
	// with no edit here, and one that loses it stops. That is the same reasoning the
	// function was introduced for — two writers of one config key must not disagree.
	if platform.ValidateRunnerName(target) == nil {
		if err := bindRunner(target); err != nil {
			return err
		}
	} else {
		// The runner it leaves alone is the EFFECTIVE one (ResolveRunner), not the raw config field.
		using := "the configured runner"
		if st, err := store.Open(); err == nil {
			using = st.ResolvedConfig().Runner
			st.Close()
		}
		fmt.Println(resultRow(skipGlyph(), "runner", "unchanged", target+" runs no model; lenses use "+using))
	}
	// Report the resulting lens state. The first-open hook auto-seeds the built-in
	// "default" lens on a fresh archive, and every enabled lens mines an LLM on every
	// session — real, recurring cost — so surface it explicitly here rather than leaving
	// it to a background slog line the user never sees. Read ACTUAL state (default is
	// deletable and won't be re-seeded once removed, so it may legitimately be absent).
	// Best-effort: never fail an otherwise-good install over a status line.
	lenses := reportEnabledLenses()

	fmt.Println()
	fmt.Println(out.glyph + " " + out.line)
	for _, h := range out.hints {
		fmt.Println(hint(h[0], h[1]))
	}
	fmt.Println(hint("change runner", "witness config set runner <"+strings.Join(platform.RunnerNames(), "|")+">"))
	switch len(lenses) {
	case 0:
		fmt.Println(hint("add a lens", "witness lens load-default"))
	case 1:
		fmt.Println(hint("drop a lens", "witness lens disable "+lenses[0]))
	default:
		fmt.Println(hint("drop a lens", "witness lens disable <name>"))
	}
	if out.note != "" {
		fmt.Println(dim("  " + out.note))
	}
	return nil
}

// wireOutcome is how an editor's installer closes the wire screen: the result line (glyph + text),
// the next steps that belong to it, and a dim closing note. cmdInstall prints it last.
type wireOutcome struct {
	glyph, line string
	hints       [][2]string
	note        string
}

// pullOnlyNote closes the wire screen of an editor whose agents get the MCP tools.
const pullOnlyNote = "the profile is pull-only: agents read it over MCP, you with `witness profile`"

// reportEnabledLenses prints the enabled-lens row after install so the user knows what
// will run (and cost) on every session — the transparency the silent auto-seed otherwise
// lacks — and returns the names for the next-step hints. Best-effort and read-only.
func reportEnabledLenses() []string {
	st, err := store.Open()
	if err != nil {
		return nil
	}
	defer st.Close()
	enabled := st.LoadConfig().EnabledLenses
	if len(enabled) == 0 {
		fmt.Println(resultRow(warnGlyph(), "lenses", "none enabled", "nothing will be distilled yet"))
		return nil
	}
	note := "mines every session"
	if len(enabled) > 1 {
		note = "each mines every session"
	}
	fmt.Println(resultRow(enabledGlyph(true), "lenses", strings.Join(enabled, ", "), note))
	return enabled
}

// bindRunner pins config.toml's runner field to the integration that was just
// wired, so distillation uses the same agent runtime the user installed for.
// store.Open() already laid down the full template if config was missing; here
// we only flip runner. Best-effort: a store open failure (rare; Open mkdirs) is
// reported on its row but does not fail install, since hooks/plugin are already in place.
func bindRunner(target string) error {
	st, err := store.Open()
	if err != nil {
		fmt.Println(resultRow(warnGlyph(), "runner", "not set", "could not open the archive: "+err.Error()))
		return nil
	}
	defer st.Close()
	// "unchanged" only when an earlier wire or `config set runner` had already bound this runner:
	// a fresh archive resolves to the template's "claude" without anyone having chosen it.
	unchanged := st.RunnerBound() && st.ResolvedConfig().Runner == target
	if err := st.SetRunner(target); err != nil {
		return fmt.Errorf("write runner to config: %w", err)
	}
	note := "the default"
	if unchanged {
		note = "the default, unchanged"
	}
	fmt.Println(resultRow(okGlyph(), "runner", target, note))
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
func cmdInstallClaude() (wireOutcome, error) {
	inv, err := resolveClaudeInstall()
	if err != nil {
		return wireOutcome{}, err
	}
	settings := filepath.Join(claudeDir(), "settings.json")
	if err := os.MkdirAll(claudeDir(), 0o755); err != nil {
		return wireOutcome{}, err
	}
	data, err := os.ReadFile(settings)
	if err != nil && !os.IsNotExist(err) {
		return wireOutcome{}, err
	}
	merged, err := mergeWitnessHooks(data, inv)
	if err != nil {
		return wireOutcome{}, err
	}
	if err := store.WriteFileAtomic(settings, merged); err != nil {
		return wireOutcome{}, err
	}
	fmt.Println(resultRow(okGlyph(), "hooks", tildePath(settings), pluralize(len(witnessHookSpecs(inv)), "event", "events")))

	out := wireOutcome{glyph: okGlyph(), line: "wired — restart Claude Code (or open /hooks) to load the hooks", note: pullOnlyNote}
	// Register the MCP server (idempotent: skip if already present). Both probes are
	// bounded — see claudeCLITimeout for why: an unbounded `claude` child hung
	// `witness wire` forever and orphaned itself to PID 1, unreapable.
	if list, _ := claudeCLIOutput("mcp", "list"); mcpServerRegistered(list, "witness") {
		fmt.Println(resultRow(okGlyph(), "MCP server", "witness", "already registered"))
	} else if err := claudeCLIRun("mcp", "add", "-s", "user", "witness", // inv.target is what settings.json / `claude mcp add` should invoke for the MCP
		// server: the shim on Unix, the installed exe on Windows. (This was an mcpTarget()
		// method that returned exactly this field; the per-platform choice happens where the
		// hookInvocation is CONSTRUCTED, not on read, so the accessor added a name without
		// adding a decision.)
		inv.target, "mcp"); err != nil {
		// Capture works without it, so this is a warning, not a failure — but the result line
		// must not read as full success, which the old "done — restart Claude Code" did.
		fmt.Println(resultRow(warnGlyph(), "MCP server", "not registered", mcpAddFailure(err)))
		out = wireOutcome{glyph: warnGlyph(), line: "wired, with a warning — capture works, but agents cannot read the profile yet",
			hints: [][2]string{{"register MCP", mcpAddCommand(inv)}}}
	} else {
		fmt.Println(resultRow(okGlyph(), "MCP server", "witness", "registered with Claude Code"))
	}
	return out, nil
}

// mcpAddFailure says why `claude mcp add` failed. It used to guess "is `claude` on PATH?" for every
// failure, including the ones where claude ran and said no.
func mcpAddFailure(err error) string {
	var ee *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "`claude` is not on PATH"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("`claude mcp add` did not finish within %s", claudeCLITimeout)
	case errors.As(err, &ee):
		return fmt.Sprintf("`claude mcp add` exited %d", ee.ExitCode())
	}
	return "`claude mcp add` failed: " + err.Error()
}

// mcpAddCommand is the registration to run by hand, quoted for the shell it will be pasted into.
func mcpAddCommand(inv hookInvocation) string {
	target := shellQuote(inv.target)
	if inv.execForm { // Windows: PowerShell or cmd, neither of which reads POSIX single quotes the same
		target = `"` + inv.target + `"`
	}
	return "claude mcp add -s user witness " + target + " mcp"
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
//
// `target` is what the plugin and the MCP entry invoke, resolved per-platform by
// resolveOpenCodeInstall (issue #10): the in-repo witness.sh shim on Unix, and the
// self-installed witness.exe under %LOCALAPPDATA% on Windows, which has no guaranteed
// shell to run a .sh. Everything downstream is platform-agnostic — the plugin spawns an
// argv array (no shell), and both the plugin source and the MCP config JSON-encode the
// path, so a Windows path's backslashes are escaped correctly by construction.
func cmdInstallOpenCode() (wireOutcome, error) {
	// Before anything is copied or written: on Windows resolveOpenCodeInstall installs the
	// bundle, and a refusal must leave the machine as it found it.
	if err := openCodeV2Check(); err != nil {
		return wireOutcome{}, err
	}
	target, err := resolveOpenCodeInstall()
	if err != nil {
		return wireOutcome{}, err
	}
	dir := opencodeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wireOutcome{}, err
	}
	config := opencodeConfigPath()
	data, err := os.ReadFile(config)
	if err != nil && !os.IsNotExist(err) {
		return wireOutcome{}, err
	}
	merged, err := mergeOpenCodeMCP(data, target)
	if err != nil {
		return wireOutcome{}, err
	}
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		return wireOutcome{}, err
	}

	plugins := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		return wireOutcome{}, err
	}
	pluginPath := openCodePluginPath(dir, openCodePluginName)
	if err := store.WriteFileAtomic(pluginPath, []byte(opencodeplugin.Source(target))); err != nil {
		return wireOutcome{}, err
	}
	removeLegacyOpenCodePlugins(dir)
	fmt.Println(resultRow(okGlyph(), "plugin", tildePath(pluginPath), ""))

	if err := store.WriteFileAtomic(config, merged); err != nil {
		return wireOutcome{}, err
	}
	fmt.Println(resultRow(okGlyph(), "MCP server", "witness", "in "+tildePath(config)))
	return wireOutcome{glyph: okGlyph(), line: "wired — restart OpenCode so the plugin and MCP server load", note: pullOnlyNote}, nil
}

// cmdUninstall removes the witness hooks/plugin and MCP server (idempotent).
// Data, config, and the runner setting are left untouched. Like wire, it is a title, a glyph row
// per part, and one result line that says whether anything was actually removed.
func cmdUninstall(args []string) error {
	target := installTarget(args)
	ed, ok := editors[target]
	if !ok {
		return unknownEditor(target)
	}
	fmt.Println(title("unwire "+target, "", ""))
	fmt.Println()
	removed, warning, err := ed.unwire()
	if err != nil {
		return err
	}
	fmt.Println()
	// A part that could not be checked makes it a warning: "✓ unwired" under a "! MCP server not
	// removed" row claimed more than happened.
	switch {
	case warning != "" && removed:
		fmt.Println(warnGlyph() + " unwired, but " + warning + " — data and config are untouched")
	case warning != "":
		fmt.Println(warnGlyph() + " nothing removed, and " + warning + " — data and config are untouched")
	case !removed:
		fmt.Println(skipGlyph() + " nothing to remove — data and config are untouched")
	default:
		fmt.Println(okGlyph() + " unwired — data and config are untouched")
	}
	if removed {
		fmt.Println(hint("reconnect", "witness wire "+target))
	}
	return nil
}

// cmdUninstallClaude reports, per part, what it removed, whether it removed anything, and — as
// warning — a part it could not check.
func cmdUninstallClaude() (removed bool, warning string, err error) {
	// Report what actually happened. This used to swallow BOTH the parse error and the
	// write error and print "hooks removed" regardless — so an unparseable settings.json,
	// or a read-only ~/.claude, left every witness hook wired while the user was told they
	// were gone. They then delete the binary, and Claude Code fires a hook for a command
	// that no longer exists on every single turn. Mirrors cmdUninstallOpenCode, which
	// already surfaces both errors.
	settings := filepath.Join(claudeDir(), "settings.json")
	data, err := os.ReadFile(settings)
	switch {
	case err == nil:
		cleaned, err := removeWitnessHooks(data)
		if err != nil {
			return false, "", fmt.Errorf("%s: %w (edit it by hand to remove the witness hooks)", settings, err)
		}
		// Compare before writing, the way codex does: re-marshalling an already-clean file
		// produces equal JSON, and "hooks removed" on every re-run hid whether there were any.
		if !jsonEqual(data, cleaned) {
			if err := store.WriteFileAtomic(settings, cleaned); err != nil {
				return false, "", fmt.Errorf("write %s: %w", settings, err)
			}
			fmt.Println(resultRow(okGlyph(), "hooks", "removed from "+tildePath(settings), ""))
			removed = true
		} else {
			fmt.Println(resultRow(skipGlyph(), "hooks", "none in "+tildePath(settings), ""))
		}
	case os.IsNotExist(err):
		fmt.Println(resultRow(skipGlyph(), "hooks", "none — there is no "+tildePath(settings), ""))
	default:
		return false, "", fmt.Errorf("read %s: %w", settings, err)
	}
	// The MCP removal stays best-effort: `claude` may not be on PATH, and it exits
	// non-zero when the server simply isn't registered — neither is a failure to report.
	// It is a package var so tests never spawn the real CLI (see removeClaudeMCPServer).
	switch err := removeClaudeMCP(); {
	case err == nil:
		fmt.Println(resultRow(okGlyph(), "MCP server", "removed", ""))
		removed = true
	case errors.Is(err, exec.ErrNotFound):
		fmt.Println(resultRow(warnGlyph(), "MCP server", "not checked", "`claude` is not on PATH"))
		warning = "the MCP server was not checked"
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Println(resultRow(warnGlyph(), "MCP server", "not removed", fmt.Sprintf("`claude mcp remove witness` did not finish within %s", claudeCLITimeout)))
		fmt.Println(subHint("by hand", "claude mcp remove witness"))
		warning = "the MCP server may still be registered"
	default: // claude ran and refused: the server is not registered
		fmt.Println(resultRow(skipGlyph(), "MCP server", "not registered", ""))
	}
	return removed, warning, nil
}

// jsonEqual reports whether two JSON documents hold the same value, whatever their formatting.
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// claudeCLITimeout bounds every `claude` subprocess wiring runs (`claude mcp list/add/remove`).
//
// This is not hypothetical caution. The call used to be a bare exec.Command(...).Run() with
// no deadline and no way to kill the child, and I watched 366 of them accumulate on a real
// machine and pin the CPU: each invocation never exited, and several had reparented to PID 1
// (init) — so they outlived the process that spawned them and nothing would ever reap them.
// `witness unwire claude` would simply hang forever in that state, with no output and no
// way out but Ctrl-C, and every retry would leak another immortal child.
//
// CommandContext + a deadline makes the child killable, so the worst case is a bounded wait
// and a best-effort skip rather than an unbounded pile of orphans.
const claudeCLITimeout = 20 * time.Second

// removeClaudeMCP is the seam that keeps `claude mcp remove` out of tests. Overridden in
// tests so uninstall can be exercised without spawning the real CLI — which is exactly how
// the orphan pile above got created.
var removeClaudeMCP = removeClaudeMCPServer

// removeClaudeMCPServer runs `claude mcp remove witness` and returns its error for the caller to
// word: nil removed it, exec.ErrNotFound means no claude, DeadlineExceeded means it wedged, and any
// other failure is claude saying the server was not registered.
func removeClaudeMCPServer() error { return claudeCLIRun("mcp", "remove", "witness") }

// claudeCLICommand builds a BOUNDED `claude` invocation. Every call into the Claude CLI must
// go through here (enforced by TestUninstallDoesNotSpawnClaudeWithoutAContext): exec.Command
// has no cancellation, so a `claude` that never exits can neither be killed nor reaped, and
// it takes the witness command down with it. Stdio is detached so the child can't block on a
// pipe nobody drains — another way a "best-effort" call becomes a permanent hang.
func claudeCLICommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = nil
	return cmd
}

// claudeCLIRun runs a bounded `claude` subcommand, discarding its output. A deadline
// overrun is reported as context.DeadlineExceeded so callers can distinguish "the CLI is
// wedged" from "the CLI said no".
func claudeCLIRun(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), claudeCLITimeout)
	defer cancel()
	cmd := claudeCLICommand(ctx, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("claude %s timed out after %s: %w", strings.Join(args, " "), claudeCLITimeout, context.DeadlineExceeded)
	}
	return err
}

// claudeCLIOutput runs a bounded `claude` subcommand and returns its combined output. A
// timeout yields the empty string, which the caller treats as "not registered" — the
// idempotent path then attempts an add, which is itself bounded and merely reports failure.
func claudeCLIOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeCLITimeout)
	defer cancel()
	out, err := claudeCLICommand(ctx, args...).CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("claude %s timed out after %s: %w", strings.Join(args, " "), claudeCLITimeout, context.DeadlineExceeded)
	}
	return string(out), err
}

// cmdUninstallOpenCode reports, per part, what it removed. It used to print three "removed (if it
// was present)" lines on every run, and to rewrite opencode.json even when witness was not in it —
// which, for a .jsonc config, also dropped the user's comments.
func cmdUninstallOpenCode() (removed bool, err error) {
	dir := opencodeDir()
	config := opencodeConfigPath()
	switch data, err := os.ReadFile(config); {
	case err == nil:
		root, err := parseOpenCodeConfig(data)
		if err != nil {
			return false, err
		}
		if mcp, _ := root["mcp"].(map[string]any); mcp["witness"] == nil {
			fmt.Println(resultRow(skipGlyph(), "MCP server", "none in "+tildePath(config), ""))
			break
		}
		cleaned, err := removeOpenCodeMCP(data)
		if err != nil {
			return false, err
		}
		if err := store.WriteFileAtomic(config, cleaned); err != nil {
			return false, err
		}
		fmt.Println(resultRow(okGlyph(), "MCP server", "removed from "+tildePath(config), ""))
		removed = true
	case os.IsNotExist(err):
		fmt.Println(resultRow(skipGlyph(), "MCP server", "none — there is no "+tildePath(config), ""))
	default:
		return false, err
	}
	gone := removeAllOpenCodePlugins(dir)
	for _, p := range gone {
		fmt.Println(resultRow(okGlyph(), "plugin", "removed "+tildePath(p), ""))
	}
	if len(gone) == 0 {
		fmt.Println(resultRow(skipGlyph(), "plugin", "none in "+tildePath(filepath.Join(dir, "plugins")), ""))
	}
	return removed || len(gone) > 0, nil
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
