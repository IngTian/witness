package commands

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The Claude Code capture path rides on a NAME CONTRACT: install writes hook
// commands like `'<shim>' capture` / `session-start` / `session-end`, the shim
// forwards those tokens verbatim to the binary, and the binary must have a cobra
// command of that exact name — otherwise the hook fires, the binary errors with
// "unknown command", and capture silently stops while every unit test stays green
// (the refactor renamed all these commands with zero test tying the two sides).
//
// This locks the contract: every subcommand token install emits, plus the two
// tokens spawned internally (`worker` via spawnDetached, `mcp` via the MCP
// registration), must resolve to a registered command on the real root.
func TestHookCommandTokensAreRegisteredCommands(t *testing.T) {
	root := newRootCmd()

	// Run the contract for BOTH invocation forms. Shell form (Unix) carries the
	// token as the trailing word of the command string; exec form (Windows)
	// carries it in Args. Either way the token must be a real cobra command.
	for _, tc := range []struct {
		name string
		inv  hookInvocation
	}{
		{"shell form (unix shim)", shellInvocation("/repo/hooks/witness.sh")},
		{"exec form (windows exe)", execInvocation(`C:\witness\witness.exe`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := map[string]bool{}
			for _, spec := range witnessHookSpecs(tc.inv) {
				for _, h := range spec.Entry.Hooks {
					emitted[hookToken(t, h)] = true
				}
			}
			// Sanity: install must emit the three hook entry points we expect. If the
			// hook wiring changes, update this list deliberately (that's the point).
			for _, want := range []string{"session-start", "capture", "session-end"} {
				if !emitted[want] {
					t.Errorf("witnessHookSpecs no longer emits %q — hook wiring changed", want)
				}
			}
			// Every emitted token, plus every token the binary spawns FOR ITSELF, must
			// resolve. The self-spawned set is DISCOVERED from the source rather than
			// hardcoded — see selfSpawnedTokens for why the old hardcoded {"worker","mcp"}
			// guarded the wrong names.
			tokens := map[string]bool{"mcp": true}
			for _, tok := range selfSpawnedTokens(t) {
				tokens[tok] = true
			}
			for tok := range emitted {
				tokens[tok] = true
			}
			for tok := range tokens {
				assertRegistered(t, root, tok)
			}
		})
	}
}

// hookToken extracts the subcommand token from an emitted hook, handling both
// forms: exec form (Windows) puts it in Args; shell form (Unix) puts it as the
// trailing word of the `'<shim>' <token>` command string.
func hookToken(t *testing.T, h hookCmd) string {
	t.Helper()
	if len(h.Args) > 0 {
		return h.Args[len(h.Args)-1]
	}
	fields := strings.Fields(h.Command)
	if len(fields) < 2 {
		t.Fatalf("hook command %q has no subcommand token", h.Command)
	}
	return fields[len(fields)-1]
}

// selfSpawnedTokens discovers, FROM THE SOURCE, every subcommand token that is invoked on the
// witness binary by something OTHER than a Claude Code hook — the tokens no hook spec emits and
// so nothing else in this file would cover.
//
// It is derived rather than hardcoded because the hardcoded list guarded the WRONG names. The
// test asserted {"worker", "mcp"} while the real tokens are "worker-run" (5 sites),
// "worker-wakeup", and "worker-kick". A bare `worker` command DOES exist (worker_group.go), so
// the assertion passed while guarding nothing: renaming or removing `worker-run` would have
// left CI green and silently broken EVERY distillation trigger — capture's post-turn kick, the
// OpenCode plugin's quiet-period kick, ingest's kick, and the backoff wakeup. That is exactly
// the class of silent breakage this test exists to prevent, which is why the token set must
// not be maintained by hand.
//
// Three channels are scanned, because the tokens reach the binary three different ways and a
// scan covering only the first would have missed two of them:
//
//  1. a direct literal spawn — spawnDetached("worker-run", …);
//  2. an INJECTED spawner — scheduleWorkerWakeupWith takes a spawn func, so the call site reads
//     spawn("worker-wakeup", …) with no spawnDetached in sight;
//  3. the OpenCode plugin JS, which spawns the binary itself — spawnWitness(["worker-kick"]).
//     This one crosses a language boundary, so no Go-only scan can see it.
func selfSpawnedTokens(t *testing.T) []string {
	t.Helper()
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// The plugin JS lives outside this package; both copies must stay in lockstep, and either
	// is enough to discover the token.
	jsFiles := []string{
		filepath.Join("..", "..", "internal", "platform", "opencode", "plugin", "witness.js"),
		filepath.Join("..", "..", "npm", "opencode", "witness.js"),
	}

	patterns := []*regexp.Regexp{
		// (1) direct: spawnDetached("worker-run", …) / spawnDetachedOK("worker-wakeup", …)
		regexp.MustCompile(`spawnDetached(?:OK)?\(\s*"([a-z][a-z0-9-]*)"`),
		// (2) injected spawner: spawn("worker-wakeup", …)
		regexp.MustCompile(`\bspawn\(\s*"([a-z][a-z0-9-]*)"`),
		// (3) plugin JS: spawnWitness(["worker-kick"]) — also matches the import/mcp args
		regexp.MustCompile(`spawnWitness\(\[\s*"([a-z][a-z0-9-]*)"`),
	}

	seen := map[string]bool{}
	var out []string
	scan := func(body string, required bool, name string) int {
		hits := 0
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				hits++
				if tok := m[1]; !seen[tok] {
					seen[tok] = true
					out = append(out, tok)
				}
			}
		}
		if required && hits == 0 {
			t.Errorf("no spawn tokens found in %s — the scan patterns have drifted and this "+
				"guard is going blind", name)
		}
		return hits
	}

	goHits := 0
	for _, f := range goFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		goHits += scan(string(b), false, f)
	}
	if goHits == 0 {
		t.Fatal("found no self-spawned tokens in the Go source — the scan patterns have drifted " +
			"from the spawn helpers, so this guard is silently inert")
	}
	jsFound := false
	for _, f := range jsFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			continue // a missing copy is covered by the plugin-parity tests, not here
		}
		if scan(string(b), true, f) > 0 {
			jsFound = true
		}
	}
	if !jsFound {
		t.Error("no plugin-JS spawn tokens found: the OpenCode plugin invokes the binary across " +
			"a language boundary, so if this scan misses it those tokens are unguarded")
	}
	sort.Strings(out)
	return out
}

// The discovery itself must not silently go blind: the tokens we KNOW are spawned today have
// to be found. If a spawn site is renamed away, update this list deliberately — same contract
// as the hook-entry-point sanity check above.
func TestSelfSpawnedTokenDiscoveryFindsTheKnownSpawns(t *testing.T) {
	got := map[string]bool{}
	for _, tok := range selfSpawnedTokens(t) {
		got[tok] = true
	}
	// The three tokens the OLD hardcoded list did not cover. Each is a real distillation
	// trigger: worker-run (capture's post-turn kick, ingest, observations), worker-wakeup (the
	// backoff retry), worker-kick (the OpenCode plugin's quiet-period start).
	for _, want := range []string{"worker-run", "worker-wakeup", "worker-kick"} {
		if !got[want] {
			t.Errorf("the scan no longer finds %q; if that spawn site was renamed, update this "+
				"list — otherwise the hook-contract guard has gone blind (found: %v)", want, got)
		}
	}
	// `worker` IS legitimately discovered — the plugin runs `worker stop --auto-only` on dispose
	// — so it stays covered as a group whose subcommand path must resolve. What was wrong before
	// was guarding ONLY that, since it told us nothing about the three tokens above.
	if !got["worker"] {
		t.Error(`"worker" should still be discovered: the plugin invokes "worker stop --auto-only"`)
	}
}

// assertRegistered fails if cobra's root cannot resolve name to a real command.
func assertRegistered(t *testing.T, root *cobra.Command, name string) {
	t.Helper()
	cmd, _, err := root.Find([]string{name})
	if err != nil {
		t.Errorf("hook token %q does not resolve to a registered command: %v", name, err)
		return
	}
	if cmd == nil || cmd.Name() != name {
		got := "<nil>"
		if cmd != nil {
			got = cmd.Name()
		}
		t.Errorf("hook token %q resolved to %q, not a command of that name (Find fell back to root)", name, got)
	}
}
