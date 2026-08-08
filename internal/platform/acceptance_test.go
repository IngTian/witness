package platform_test

// Executable acceptance guard for issue #21: the engine (internal/distill) and the
// command layer must not DISPATCH on a platform name. This locks the refactor in —
// if someone later adds `switch cfg.Runner` or `EqualFold(agent,"opencode")` back
// into the engine, this test fails in CI instead of the coupling silently returning.
//
// It scopes to genuine dispatch patterns, NOT every mention: comments, config-schema
// defaults, user-facing help/prose, and the one documented backfill literal are
// legitimate and would be maddening to forbid. So we grep for the dispatch SHAPES
// (switch/EqualFold on a platform axis, or spawning a runtime), not the words.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

// dispatchPatterns are the shapes that mean "engine code is branching on a platform
// identity" — the thing the refactor removed.
var dispatchPatterns = []*regexp.Regexp{
	regexp.MustCompile(`switch\s+.*\b(cfg\.)?[Rr]unner\b`),
	regexp.MustCompile(`switch\s+.*\b(target|agent)\b`),
	regexp.MustCompile(`EqualFold\([^)]*\b([Rr]unner|target|agent)\b`),
	regexp.MustCompile(`case\s+"(claude|opencode)"`),
	// Spawning a distillation RUNNER from the engine (claude -p / opencode serve).
	// NOT `claude mcp add` — that's the installer's CLI plumbing (cmd-side, legit),
	// so match the `-p` distill invocation specifically, not any `claude` subcommand.
	regexp.MustCompile(`"claude".*"-p"|"-p".*"claude"`),
	regexp.MustCompile(`StartOpenCodeServer|"opencode".*"serve"`),

	// The shapes below were added after this guard MISSED the one real violation it
	// existed to catch. internal/distill/worker.go asked
	// `platform.ForSession(...).Name() == platform.AgentOpenCode` — engine code deciding
	// behaviour from a platform identity, in the fenced tree, for months. None of the six
	// patterns above matches it: there is no `switch`, no `EqualFold`, no `case "…"`, and no
	// quoted runtime name on the line. A guard with a hole exactly where the violation lives
	// is worse than no guard, because it reads as coverage.
	//
	// So: match the ACT of comparing an identity, not the specific spellings someone happened
	// to use. Each pattern below is a shape a careless author would plausibly produce.

	// `x.Name() == …` / `!=` — the miss above. Any identity equality on a platform value.
	regexp.MustCompile(`\.Name\(\)\s*(==|!=)`),
	// Comparing against the platform/runner NAME CONSTANTS rather than a quoted literal, which
	// is how the same dispatch survives a rename: `== platform.AgentOpenCode`,
	// `!= store.RunnerClaude`, `case platform.AgentClaude:`.
	regexp.MustCompile(`(==|!=)\s*(platform\.Agent|store\.Runner)`),
	regexp.MustCompile(`case\s+(platform\.Agent|store\.Runner)`),
	// A `case` arm naming any registered platform, not just the two hardcoded ones. "file" was
	// invisible to the old pattern even though it is a real registered platform.
	regexp.MustCompile(`case\s+"(claude|opencode|file)"`),
	// A TYPE switch or assertion on a concrete adapter reaches around the capability
	// interfaces entirely — the engine must never name an adapter type.
	regexp.MustCompile(`\.\((\*)?(claude|opencode|file)\.`),
	// Importing an adapter package from the engine at all. The compiler would allow it and
	// every pattern above would still pass.
	regexp.MustCompile(`witness/internal/platform/(claude|opencode|file)"`),
}

// engineDirs are the trees that MUST stay platform-agnostic. Notably absent:
// internal/platform/* (the platform impls — dispatch there is the point) and
// internal/store (the documented backfill literal + config default live there).
func TestEngineHasNoPlatformDispatch(t *testing.T) {
	// Locate the repo root from this test file (internal/platform → up two).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // .../internal/platform → repo root

	engineDirs := []string{
		filepath.Join(root, "internal", "distill"),
		filepath.Join(root, "cmd", "commands"),
	}

	for _, dir := range engineDirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// installers.go legitimately NAMES the platforms when registering their
			// cmd-side installers (RegisterInstaller("claude", ...)) — that's the
			// composition root wiring, not dispatch. Allow it.
			if filepath.Base(path) == "installers.go" || filepath.Base(path) == "root.go" {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(src), "\n") {
				code := stripComment(line)
				if strings.TrimSpace(code) == "" {
					continue
				}
				// Skip obvious user-facing print lines (help/prose), which may contain
				// "claude|opencode" as documentation, not dispatch.
				if strings.Contains(code, "fmt.Print") || strings.Contains(code, "Fprint") {
					continue
				}
				for _, pat := range dispatchPatterns {
					if pat.MatchString(code) {
						rel, _ := filepath.Rel(root, path)
						t.Errorf("platform-name DISPATCH reintroduced into the engine:\n  %s: %s\n  (matched %s) — route through the platform registry instead",
							rel, strings.TrimSpace(line), pat)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}

// stripComment removes a trailing // comment so a mention inside a comment doesn't
// trip the dispatch patterns. Naive (ignores // inside strings) but the patterns
// are specific enough that a // in a string here is a non-issue in practice.
func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

// Runner-name validation must come from the REGISTRY, and both writers of the config key must
// agree.
//
// Before this, `witness config set runner` validated against a hardcoded
// {RunnerClaude, RunnerOpenCode} pair while `witness install <target>` → bindRunner wrote the same
// key with NO validation. Two writers of one value, disagreeing — and a closed set of platform names
// in a system that is otherwise uniformly registry-driven, so a third runtime would be rejected by
// one path and silently accepted by the other.
func TestValidateRunnerNameComesFromTheRegistry(t *testing.T) {
	// Every runner-capable registered platform must validate.
	names := platform.RunnerNames()
	if len(names) < 2 {
		t.Fatalf("expected at least the two built-in runners, got %v", names)
	}
	for _, name := range names {
		if err := platform.ValidateRunnerName(name); err != nil {
			t.Errorf("registered runner %q was rejected: %v", name, err)
		}
	}

	// Empty means "unset, use the default" — the config template ships it empty, so rejecting it
	// would make a fresh install unconfigurable.
	if err := platform.ValidateRunnerName(""); err != nil {
		t.Errorf("an empty runner must be valid (unset): %v", err)
	}
	if err := platform.ValidateRunnerName("   "); err != nil {
		t.Errorf("a blank runner must be treated as unset: %v", err)
	}

	// A typo must FAIL CLOSED, and the message must name the real options rather than a hardcoded
	// pair — that is what keeps it correct when a runtime is added.
	err := platform.ValidateRunnerName("openkode")
	if err == nil {
		t.Fatal("a typo'd runner name must be rejected")
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error should list the registered runner %q; got %v", name, err)
		}
	}

	// A platform that registers but supplies NO RunnerProvider can own sessions without being able
	// to distill, so it must not be accepted as a runner. internal/platform/file is exactly that,
	// and it is registered by the blank import in this package's tests.
	if _, ok := platform.ByName("file"); ok {
		if err := platform.ValidateRunnerName("file"); err == nil {
			t.Error("the `file` platform has no RunnerProvider, so it must not validate as a runner")
		}
		if slicesContains(names, "file") {
			t.Error("RunnerNames listed `file`, which cannot run a model")
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
