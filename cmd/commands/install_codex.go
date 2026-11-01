package commands

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IngTian/witness/internal/platform/codex"
)

// Codex hook wiring (issue #179).
//
// Codex capture shipped in v0.9.1 able to IMPORT history, but keeping it current
// meant hand-merging a 20-line JSON block into ~/.codex/hooks.json, substituting an
// absolute path, and writing a different form on Windows. Every failure mode there is
// silent: a wrong path, a clobbered sibling hook, or a Windows quoting mistake all
// leave Codex running fine and witness capturing nothing. That is issue #10's shape,
// and it is exactly what the Claude/OpenCode installers exist to prevent.
//
// Codex's hooks.json uses the SAME event → matcher-group → handler-list schema as
// Claude Code's settings.json, so this reuses mergeHooks/removeHooks wholesale and
// supplies only its own specs and ownership predicate. Two things differ:
//
//   - Codex runs `command` through a SHELL and takes an optional Windows-only
//     `commandWindows` (PowerShell) override — not Claude's exec-form {command, args}.
//   - Trust. Non-managed Codex hooks are inert until the user approves them, and Codex
//     records trust against the hook's HASH, so any edit re-triggers review. We
//     therefore skip the write entirely when the bytes are unchanged (see
//     cmdInstallCodex) and tell the user to run /hooks, which install cannot do for them.
//
// NOT done here: registering the witness MCP server for Codex. That lives in
// ~/.codex/config.toml, which also holds account configuration — witness does not
// rewrite a foreign TOML file it does not own. Capture is the whole of #179.

// codexHookArgs is the exact argument tail every Codex hook we write carries. It is
// also the ownership signal: a command ending in this, running a witness executable,
// is one of ours and nobody else's.
const codexHookArgs = "import --agent codex --hook"

// codexHookEvents are the three lifecycle events witness reconciles on. Codex hooks
// carry only a session_id, so witness re-reads that conversation from local history
// rather than treating the hook as a transcript feed; repeating an import is a no-op,
// which is why SessionStart and SessionEnd can safely cover anything Stop missed.
//
// Deliberately NOT UserPromptSubmit (which Claude capture does use): Codex persists a
// turn to local history only once it completes, so firing on submit would reconcile a
// conversation whose newest message does not exist yet.
var codexHookEvents = []string{"SessionStart", "Stop", "SessionEnd"}

// codexHook is how the installed Codex hooks invoke witness — the one platform-
// dependent part, resolved by resolveCodexInstall (GOOS-split).
type codexHook struct {
	program    string // the shim on Unix, the installed witness.exe on Windows
	powerShell bool   // write commandWindows (PowerShell) instead of command
}

// psQuote single-quotes a path for PowerShell, which escapes an embedded single quote
// by DOUBLING it (”) rather than with a backslash as POSIX does. Getting this wrong
// is not cosmetic: a real user directory (C:\Users\O'Brien\...) would produce a
// command PowerShell cannot parse, and a Codex hook that fails is silent.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// codexHookSpecs is the canonical Codex wiring: one handler per lifecycle event, all
// async.
//
// async matters for correctness, not just speed. Codex's SessionEnd timeout defaults
// to ONE second (capped at three) while the others default to 600 — and reconciling a
// large history can exceed a second easily, so a synchronous SessionEnd hook would be
// killed mid-import on exactly the archives that need it most. async also upholds
// witness's standing invariant that capture must never block a session. We give up
// nothing: witness injects no context, which is the only thing an async hook cannot do.
func codexHookSpecs(h codexHook) []hookSpec {
	cmd := hookCmd{Type: "command", Async: true}
	if h.powerShell {
		// The PowerShell call operator (&) is required to run a quoted path.
		cmd.CommandWindows = "& " + psQuote(h.program) + " " + codexHookArgs
	} else {
		cmd.Command = shellQuote(h.program) + " " + codexHookArgs
	}
	specs := make([]hookSpec, 0, len(codexHookEvents))
	for _, event := range codexHookEvents {
		specs = append(specs, hookSpec{Event: event, Entry: hookEntry{Hooks: []hookCmd{cmd}}})
	}
	return specs
}

// isCodexEntry reports whether a parsed hooks.json entry is one of ours, so re-wiring
// replaces it instead of appending a duplicate and unwiring removes it. It checks BOTH
// command fields so a machine that moved between platforms (or a cross-OS test) still
// recognizes what the other form wrote.
func isCodexEntry(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		for _, key := range []string{"command", "commandWindows"} {
			if c, _ := hm[key].(string); isCodexWitnessCommand(c) {
				return true
			}
		}
	}
	return false
}

// isCodexWitnessCommand reports whether a hook command string is one WE wrote: it must
// END with our exact argument tail AND run a program whose basename is a witness
// executable.
//
// Both halves are load-bearing. The arg tail alone would match a user's own wrapper
// script that shells out to witness — a foreign hook we are contractually required to
// preserve (issue #49 I3/I4). The basename alone would match any witness invocation,
// including a hook the user wrote for a different purpose.
//
// Note what this does NOT need to do: parse the path's quoting. Only the LAST path
// segment is compared, and an escaped quote — POSIX '\” or PowerShell ” — always
// sits before the final separator, so trimming the outer quotes is enough for both
// flavors. A hand-edited UNQUOTED path from the pre-#179 docs matches too, which is
// what makes `witness wire codex` adopt an existing manual install rather than
// duplicate it.
func isCodexWitnessCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasSuffix(cmd, codexHookArgs) {
		return false
	}
	prog := strings.TrimSuffix(cmd, codexHookArgs)
	prog = strings.TrimSpace(prog)
	prog = strings.TrimSpace(strings.TrimPrefix(prog, "&")) // PowerShell call operator
	prog = strings.Trim(prog, `'"`)
	if i := strings.LastIndexAny(prog, `/\`); i >= 0 {
		prog = prog[i+1:]
	}
	return strings.EqualFold(prog, "witness") ||
		strings.EqualFold(prog, "witness.exe") ||
		strings.EqualFold(prog, "witness.sh")
}

// codexHooksPath is ~/.codex/hooks.json, with the Codex home resolved through the
// IMPORTER's own function so the two can never disagree about which Codex home is
// in play (see codex.Home).
func codexHooksPath() (string, error) {
	home, err := codex.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "hooks.json"), nil
}

// wireCodexHooks merges our hooks into the hooks.json at path, creating the directory
// if needed and writing atomically. It reports whether the file CHANGED.
//
// Split out from cmdInstallCodex so the part that touches disk is testable: on Unix
// cmdInstallCodex resolves its target through repoShim, which cannot succeed under
// `go test` (the test binary lives in a build temp dir), so a test driving the command
// end-to-end can only ever assert the resolution error — leaving the atomic write and
// the trust-preserving skip below unexercised.
func wireCodexHooks(path string, h codexHook) (changed bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	merged, err := mergeHooks(data, codexHookSpecs(h), isCodexEntry)
	if err != nil {
		return false, fmt.Errorf("%s: %w (fix or remove the file, then re-run)", path, err)
	}
	// Skip a no-op write. This is not an optimization: Codex records hook trust against
	// the hook's HASH, so rewriting an already-correct file would re-trigger approval and
	// leave capture silently inert until the user noticed and ran /hooks again. Our
	// output is byte-stable (Go marshals map keys sorted), so an unchanged wiring
	// produces identical bytes and we leave the file — and its trust — alone.
	if bytes.Equal(data, merged) {
		return false, nil
	}
	if err := writeFileAtomic(path, merged); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// cmdInstallCodex merges the witness hooks into ~/.codex/hooks.json, idempotently and
// preserving every foreign hook.
func cmdInstallCodex() error {
	h, err := resolveCodexInstall()
	if err != nil {
		return err
	}
	path, err := codexHooksPath()
	if err != nil {
		return err
	}
	changed, err := wireCodexHooks(path, h)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("hooks already wired in %s (unchanged, so Codex's hook trust is preserved)\n", path)
		return nil
	}
	fmt.Printf("hooks wired into %s\n", path)
	fmt.Printf("  %s on %s\n", h.program, strings.Join(codexHookEvents, ", "))
	// The one step installing cannot do. Codex hooks are inert until the exact
	// definition is approved, so stating this plainly is the difference between working
	// capture and an archive that stays empty with no error anywhere.
	fmt.Println("REQUIRED: run /hooks inside Codex and approve the witness hooks — until you do,")
	fmt.Println("          Codex will not run them and nothing will be captured.")
	// Deliberately NOT a "Codex has no runner" note here: cmdInstall already prints that
	// for any venue without a RunnerProvider, derived from the registry. Saying it twice
	// is how two copies of one message drift apart.
	return nil
}

// unwireCodexHooks strips our entries from the hooks.json at path, reporting whether
// the file changed. Split out for the same reason as wireCodexHooks.
func unwireCodexHooks(path string) (changed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	cleaned, err := removeHooks(data, isCodexEntry)
	if err != nil {
		return false, fmt.Errorf("%s: %w (edit it by hand to remove the witness hooks)", path, err)
	}
	if bytes.Equal(data, cleaned) {
		return false, nil
	}
	if err := writeFileAtomic(path, cleaned); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// cmdUninstallCodex strips our entries from hooks.json, leaving foreign hooks and the
// rest of the file intact. Data and config are untouched.
//
// It reports what ACTUALLY happened rather than printing success unconditionally — the
// bug cmdUninstallClaude had, where an unparseable or unwritable file left every hook
// wired while the user was told they were gone, then deleted the binary and had Codex
// fire a missing command on every turn.
func cmdUninstallCodex() error {
	path, err := codexHooksPath()
	if err != nil {
		return err
	}
	changed, err := unwireCodexHooks(path)
	switch {
	case err == nil && changed:
		fmt.Printf("hooks removed from %s\n", path)
	case err == nil:
		fmt.Printf("no witness hooks in %s\n", path)
	case os.IsNotExist(err):
		fmt.Printf("no %s, so no hooks to remove\n", path)
	default:
		return err
	}
	return nil
}
