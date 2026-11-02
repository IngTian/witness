package commands

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureBoth swaps os.Stdout and os.Stderr for pipes around fn. A pipe is not a terminal, so a
// progress line takes its log form: one plain line per Where.
func captureBoth(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	read := func(f **os.File) func() string {
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		prev := *f
		*f = w
		out := make(chan string, 1)
		go func() { b, _ := io.ReadAll(r); out <- string(b) }()
		return func() string { _ = w.Close(); *f = prev; s := <-out; _ = r.Close(); return s }
	}
	doneOut, doneErr := read(&os.Stdout), read(&os.Stderr)
	err = fn()
	return doneOut(), doneErr(), err
}

func codexRollouts(t *testing.T, ids ...string) {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("WITNESS_CODEX_HOME", home)
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		body := `{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n" +
			`{"type":"response_item","timestamp":"2026-10-31T12:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
		if err := os.WriteFile(filepath.Join(home, "sessions", id+".jsonl"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// A foreground `witness import` shows how far it is on STDERR; stdout is byte-for-byte the summary
// it always printed, so a script parsing it sees no difference.
func TestImportShowsProgressOnStderrOnly(t *testing.T) {
	codexRollouts(t, "a", "b")
	out, errOut, err := captureBoth(t, func() error { return cmdImport([]string{"--agent", "codex", "--no-kick"}) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "import codex: imported 2 raw record(s) from 2 session(s)\n" +
		"background distillation not requested (--no-kick)\n"; out != want {
		t.Fatalf("stdout changed:\n%q\nwant\n%q", out, want)
	}
	if !strings.HasPrefix(errOut, "  codex · importing 0/2 · ") || strings.Count(errOut, "\n") != 1 {
		t.Fatalf("want one log-form progress line for the 2 rollouts, got %q", errOut)
	}
}

// Everything that imports in the background stays exactly as silent as before: the OpenCode
// plugin's --quiet import, and the Codex capture hook, which must also still answer "{}".
func TestImportProgressStaysOutOfBackgroundImports(t *testing.T) {
	codexRollouts(t, "a", "b")
	out, errOut, err := captureBoth(t, func() error { return cmdImport([]string{"--agent", "codex", "--quiet", "--no-kick"}) })
	if err != nil || out != "" || errOut != "" {
		t.Fatalf("--quiet: stdout %q, stderr %q, %v", out, errOut, err)
	}

	codexRollouts(t, "hooked")
	out, errOut, err = captureBoth(t, func() error {
		return cmdImportHook("codex", strings.NewReader(`{"session_id":"hooked"}`))
	})
	if err != nil || out != "{}\n" || errOut != "" {
		t.Fatalf("hook: stdout %q, stderr %q, %v", out, errOut, err)
	}
}

// An import with nothing to read draws nothing: Codex with no conversations reports a total of 0,
// and Claude (hook-fed, so its import is a no-op) reports nothing at all.
func TestImportWithNothingToReadDrawsNothing(t *testing.T) {
	codexRollouts(t)
	for _, agent := range []string{"codex", "claude"} {
		_, errOut, err := captureBoth(t, func() error { return cmdImport([]string{"--agent", agent, "--no-kick"}) })
		if err != nil || errOut != "" {
			t.Fatalf("%s: stderr %q, %v", agent, errOut, err)
		}
	}
}
