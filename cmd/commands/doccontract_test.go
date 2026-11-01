package commands

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The docs ride on a NAME CONTRACT, the same way the Claude hooks do
// (hookcontract_test.go): every `witness …` invocation a reader is told to TYPE must resolve
// against the real cobra tree, and its positional arguments must satisfy that command's own Args
// validator.
//
// WHY THIS EXISTS. The README told users to run `witness install claude` and
// `witness ingest <path>`. Neither exists — wiring an editor is `witness wire <claude|opencode>`,
// `install` provisions an ARCHIVE and takes cobra.NoArgs, and `ingest` reads NDJSON from stdin or
// --file with no positional at all. Those were the first two commands a new user types.
//
// Nothing caught it, for two compounding reasons:
//
//   - cobra exits 0 on an unknown subcommand and prints "unknown command" to stdout, so neither
//     eyeballing nor an exit-code check notices.
//   - five source comments still named the PRE-v0.5.0 spelling (`witness install <target>` was
//     what wiring used to be called), including the doc on the Installer interface itself — so
//     reading the code to learn the command taught the wrong answer.
//
// surface_test.go pins that `install` and `wire` EXIST. This pins that the docs use the right one.
//
// Precision matters more than reach here: a doc test with false positives gets disabled, and then
// it protects nothing. So flag arity comes from the real FlagSet rather than guesswork, bracketed
// `[optional]` groups are stripped (omitting them must still be valid), and a bare `witness foo`
// with no arguments is treated as a NAME REFERENCE — prose naming a command — and only checked
// for existence.
func TestDocumentedCommandsResolve(t *testing.T) {
	// Any language tag: matching only sh/bash/console mispairs fences around a ```json block
	// and swallows the prose between them.
	fence := regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")
	span := regexp.MustCompile("`(witness [^`]+)`")
	optional := regexp.MustCompile(`\[[^\]]*\]`)
	word := regexp.MustCompile(`^[a-z][a-z-]*$`)

	files := docFiles(t)
	type cited struct{ file, inv string }
	var all []cited
	for _, rel := range files {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		for _, m := range fence.FindAllStringSubmatch(text, -1) {
			for _, line := range strings.Split(m[1], "\n") {
				line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
				if strings.HasPrefix(line, "witness ") {
					all = append(all, cited{rel, line})
				}
			}
		}
		for _, m := range span.FindAllStringSubmatch(text, -1) {
			if !strings.Contains(m[1], "\n") { // a wrapped span is prose, not a command line
				all = append(all, cited{rel, strings.TrimSpace(m[1])})
			}
		}
	}
	if len(all) < 20 {
		t.Fatalf("found only %d documented invocations — the extractor is broken, which would "+
			"make this test pass vacuously", len(all))
	}

	existence, arity := 0, 0
	for _, c := range all {
		// Omitting an [optional] group must leave a valid command line, so check that form.
		inv := strings.TrimSpace(optional.ReplaceAllString(c.inv, ""))
		tokens := strings.Fields(strings.TrimPrefix(inv, "witness "))
		if len(tokens) == 0 || !word.MatchString(tokens[0]) {
			continue // prose that happens to begin "witness …"
		}

		root := newRootCmd() // fresh each time: Find/flag parsing mutates state
		cmd, rest, err := root.Find(tokens)
		if err != nil || cmd == root {
			t.Errorf("%s documents `%s`, but %q is not a witness command", c.file, c.inv, tokens[0])
			continue
		}
		existence++

		// Split rest into flags and positionals, consulting the REAL FlagSet for arity.
		var pos []string
		for i := 0; i < len(rest); i++ {
			tk := rest[i]
			if !strings.HasPrefix(tk, "-") {
				pos = append(pos, tk)
				continue
			}
			name := strings.TrimLeft(strings.SplitN(tk, "=", 2)[0], "-")
			f := cmd.Flags().Lookup(name)
			if f == nil {
				f = cmd.Flags().ShorthandLookup(name)
			}
			if f == nil {
				t.Errorf("%s documents `%s`, but %q has no flag %q",
					c.file, c.inv, cmd.CommandPath(), tk)
				continue
			}
			// A non-boolean flag consumes the next token as its value.
			if f.Value.Type() != "bool" && !strings.Contains(tk, "=") && i+1 < len(rest) {
				i++
			}
		}

		// A bare command name with nothing after it is a reference, not an instruction.
		if len(pos) == 0 && len(rest) == 0 {
			continue
		}
		if cmd.Args != nil {
			if err := cmd.Args(cmd, pos); err != nil {
				t.Errorf("%s documents `%s`, but %q rejects %v: %v",
					c.file, c.inv, cmd.CommandPath(), pos, err)
				continue
			}
		}
		arity++
	}

	if existence < 20 || arity < 10 {
		t.Fatalf("checked too few invocations (%d existence, %d arity) — extractor likely broken",
			existence, arity)
	}
	t.Logf("verified %d documented invocations (%d with arguments) across %d files",
		existence, arity, len(files))
}

// install.sh is the FIRST command a new user runs, and the docs must invoke it the way
// it actually works. Two assertions, both from a real defect shipped in v0.10.0:
//
//  1. Every documented `install.sh <target>` must pass a target the script ACCEPTS,
//     read out of the script's own guard rather than hardcoded here.
//  2. No doc may pipe it into a shell. The README said
//     `curl -fsSL .../install.sh | sh`, which fails three ways at once: no target is
//     passed (the script exits 1 with a usage message), it `cd`s to
//     `$(dirname "${BASH_SOURCE[0]}")` which is a bash-ism that is empty when read from
//     a pipe, and it BUILDS FROM SOURCE so a standalone copy has no repo to build. The
//     very first instruction in the project failed for every new user.
//
// TestDocumentedCommandsResolve did not catch it because it only inspects `witness …`
// invocations. A doc contract that covers one entry point and not the one users hit
// first reads as coverage it does not have.
func TestDocumentedInstallScriptInvocations(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	// Accepted targets come from the guard's case arm: `claude|opencode|codex) ;;`.
	accepted := map[string]bool{}
	guard := regexp.MustCompile(`(?m)^\s*([a-z|]+)\)\s*;;\s*$`).FindStringSubmatch(string(script))
	if guard == nil {
		t.Fatal("could not read install.sh's accepted targets — the guard changed shape, so this test proves nothing")
	}
	for _, name := range strings.Split(guard[1], "|") {
		accepted[name] = true
	}

	// The USAGE message must list exactly those targets. These are two separate literals
	// in the script, and a usage line that drifts from the guard sends users to a target
	// the script then rejects — the same class of defect as a doc naming a command that
	// does not exist, just one file over.
	usage := regexp.MustCompile(`usage: \./install\.sh <([a-z|]+)>`).FindStringSubmatch(string(script))
	if usage == nil {
		t.Fatal("install.sh has no recognizable usage message to cross-check the guard against")
	}
	advertised := map[string]bool{}
	for _, name := range strings.Split(usage[1], "|") {
		advertised[name] = true
	}
	if !reflect.DeepEqual(accepted, advertised) {
		t.Errorf("install.sh's guard accepts %v but its usage message advertises %v",
			keysOf(accepted), keysOf(advertised))
	}

	// Adding a venue means one arm in the guard and one in EACH of the two switches that
	// follow (the step-4 label, and the closing "what to do next" message). Forgetting
	// the label arm dies loudly under `set -u`; forgetting the closing one just prints
	// nothing, so a Codex user would never be told to approve the hooks — and unapproved
	// Codex hooks capture nothing, silently. Each target must own an arm in both.
	for name := range accepted {
		arms := regexp.MustCompile(`(?m)^\s*`+regexp.QuoteMeta(name)+`\)`).FindAllString(string(script), -1)
		if len(arms) < 2 {
			t.Errorf("install.sh accepts %q but gives it %d case arm(s) after the guard; "+
				"it needs one for the step-4 label and one for the closing instructions", name, len(arms))
		}
	}

	invocation := regexp.MustCompile(`(?:\./)?install\.sh([^\n` + "`" + `]*)`)
	checked := 0
	for _, rel := range docFiles(t) {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		for _, line := range strings.Split(text, "\n") {
			if !strings.Contains(line, "install.sh") {
				continue
			}
			// (2) never piped into a shell.
			if regexp.MustCompile(`install\.sh.*\|\s*(sh|bash)\b`).MatchString(line) {
				t.Errorf("%s pipes install.sh into a shell, which cannot work (it needs a "+
					"target and builds from a checkout):\n  %s", rel, strings.TrimSpace(line))
				continue
			}
			// (1) any invocation WITH arguments must name an accepted target. A bare
			// mention in prose ("install.sh handles this") carries no target and is fine.
			m := invocation.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			args := strings.Fields(strings.TrimSpace(m[1]))
			if len(args) == 0 || strings.HasPrefix(args[0], "#") {
				continue
			}
			checked++
			if !accepted[args[0]] {
				t.Errorf("%s documents `install.sh %s`, but install.sh accepts only %v:\n  %s",
					rel, args[0], keysOf(accepted), strings.TrimSpace(line))
			}
		}
	}
	t.Logf("checked %d documented install.sh invocations against accepted targets %v", checked, keysOf(accepted))
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// docFiles is the README plus every docs/*.md, relative to this package (cmd/commands).
func docFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	out := []string{filepath.Join(root, "README.md")}
	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatalf("read docs/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, filepath.Join(root, "docs", e.Name()))
		}
	}
	sort.Strings(out)
	return out
}
