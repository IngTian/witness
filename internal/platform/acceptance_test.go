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
