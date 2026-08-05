package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `witness uninstall claude` must not claim success when the hooks are still wired.
//
// It used to `_ =` away BOTH removeWitnessHooks's parse error and writeFileAtomic's error,
// print "hooks removed from ...", and return nil. So on an unparseable settings.json — or a
// read-only ~/.claude — every witness hook stayed installed while the user was told they
// were gone. They then delete the binary, and Claude Code fires a hook for a missing
// command on every turn.
func TestUninstallClaudeReportsFailureInsteadOfClaimingSuccess(t *testing.T) {
	t.Run("unparseable settings.json", func(t *testing.T) {
		dir := withClaudeDir(t)
		settings := filepath.Join(dir, "settings.json")
		// Valid JSON is required to know which hooks are ours; truncated JSON is not.
		if err := os.WriteFile(settings, []byte(`{"hooks": {`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := cmdUninstallClaude()
		if err == nil {
			t.Fatal("uninstall reported success on an unparseable settings.json — the hooks are still wired")
		}
		if !strings.Contains(err.Error(), "settings.json") {
			t.Errorf("the error must name the file the user has to fix, got %q", err)
		}
		// It must also leave the file alone rather than truncating it further.
		got, readErr := os.ReadFile(settings)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != `{"hooks": {` {
			t.Errorf("a failed uninstall must not modify settings.json; got %q", got)
		}
	})

	t.Run("unwritable settings.json", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := withClaudeDir(t)
		settings := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(settings, []byte(`{"hooks":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// The atomic write stages a temp file in this directory, so removing write
		// permission on the DIRECTORY is what blocks it (the file mode would not).
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := cmdUninstallClaude(); err == nil {
			t.Fatal("uninstall reported success when settings.json could not be written")
		}
	})

	t.Run("no settings.json at all is not an error", func(t *testing.T) {
		withClaudeDir(t)
		if err := cmdUninstallClaude(); err != nil {
			t.Fatalf("a missing settings.json means nothing to remove, not a failure: %v", err)
		}
	})

	t.Run("the ordinary case still succeeds and strips our hooks", func(t *testing.T) {
		dir := withClaudeDir(t)
		settings := filepath.Join(dir, "settings.json")
		wired := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/x/hooks/witness.sh capture"}]}]}}`
		if err := os.WriteFile(settings, []byte(wired), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cmdUninstallClaude(); err != nil {
			t.Fatalf("uninstall on a normal install: %v", err)
		}
		got, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "witness") {
			t.Errorf("witness hooks survived a successful uninstall: %s", got)
		}
	})
}

// withClaudeDir points claudeDir() at a scratch directory and stubs out the
// `claude mcp remove` subprocess, returning the directory.
//
// The stub is not optional hygiene. Before it existed, every call to cmdUninstallClaude in
// this test spawned a REAL `claude mcp remove witness`, and those children never exited —
// 366 of them accumulated on a developer machine and pinned the CPU, several reparented to
// PID 1 so nothing would reap them. A unit test must never shell out to the user's CLI.
func withClaudeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if claudeDir() != dir {
		t.Fatalf("claudeDir() = %q, want the scratch dir %q — this test would otherwise touch the real ~/.claude", claudeDir(), dir)
	}
	prev := removeClaudeMCP
	removeClaudeMCP = func() {}
	t.Cleanup(func() { removeClaudeMCP = prev })
	return dir
}
