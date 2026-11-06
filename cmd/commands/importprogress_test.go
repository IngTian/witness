package commands

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
	out, errOut, err := captureBoth(t, func() error { return cmdImport(importOpts{agent: "codex", noKick: true}) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "[ok] codex: imported 2 records from 2 sessions (not distilling: --no-kick)\n"; out != want {
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
	out, errOut, err := captureBoth(t, func() error { return cmdImport(importOpts{agent: "codex", quiet: true, noKick: true}) })
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
		_, errOut, err := captureBoth(t, func() error { return cmdImport(importOpts{agent: agent, noKick: true}) })
		if err != nil || errOut != "" {
			t.Fatalf("%s: stderr %q, %v", agent, errOut, err)
		}
	}
}

// On a terminal the line is cleared BEFORE the summary prints; otherwise the summary lands on the
// row still holding "codex · importing 1/1". A test process has no terminal, so this line is a
// terminal-mode one writing to the same stream as stdout, as both do on a real terminal.
func TestImportClearsTheLineBeforeTheSummary(t *testing.T) {
	codexRollouts(t, "a")
	out, _, err := captureBoth(t, func() error {
		onATerminal(t)
		return cmdImport(importOpts{agent: "codex", noKick: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := strings.Index(out, "[ok] codex: ")
	if !regexp.MustCompile(`codex .*?importing 1/1`).MatchString(out) || !regexp.MustCompile(`\r +\r\[ok\] codex: `).MatchString(out) ||
		strings.Contains(out[summary:], "\r") {
		t.Fatalf("the line must be drawn, then cleared, then the summary printed: %q", out)
	}
}

// onATerminal makes the next progress line a terminal-mode one that writes to stdout, as stderr
// and stdout share one terminal for a person running the command. Call it inside captureBoth.
func onATerminal(t *testing.T) {
	prev := startImportProgress
	t.Cleanup(func() { startImportProgress = prev })
	startImportProgress = func() *progressLine { return &progressLine{w: os.Stdout, tty: true, start: time.Now()} }
}

// `witness ingest` counts the SESSIONS it writes — known exactly once the input is parsed and
// grouped, so here 3 records in 2 sessions are 2 — one per session written, and clears the line
// before its summary. --quiet stays silent, and stdout is only the summary.
//
// The second ingest of the same file writes nothing, so no worker starts: its summary must say
// "nothing new", where it used to say "ingested 0 record(s) … distilling in the background".
func TestIngestShowsProgressPerSession(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts")) // the default lens loads, so a worker has work
	input := filepath.Join(t.TempDir(), "docs.ndjson")
	body := `{"text":"one","id":"a","session":"s"}` + "\n" + `{"text":"two","id":"b","session":"s"}` + "\n" +
		`{"text":"alone","id":"c"}` + "\n" + "not json\n"
	if err := os.WriteFile(input, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ingest := func(args ...string) error {
		c := newIngestCmd()
		c.SetArgs(append([]string{"--file", input}, args...))
		return c.Execute()
	}

	out, errOut, err := captureBoth(t, func() error { return ingest() }) // a pipe: one log line on stderr
	if err != nil || out != "[!] ingested 3 records into 2 sessions, distilling in the background\n"+
		"  skipped 1 line — not JSON, or no \"text\"\n"+
		"  -> watch it      witness status\n" ||
		!strings.HasPrefix(errOut, "  ingest · sessions 0/2 · ") || strings.Count(errOut, "\n") != 1 {
		t.Fatalf("pipe: stdout %q, stderr %q, %v", out, errOut, err)
	}

	out, errOut, err = captureBoth(t, func() error { onATerminal(t); return ingest() })
	if err != nil || errOut != "" {
		t.Fatal(err, errOut)
	}
	var frames []string
	for _, m := range regexp.MustCompile(`ingest .*?sessions (\d+/\d+)`).FindAllStringSubmatch(out, -1) {
		frames = append(frames, m[1])
	}
	const summary = "[-] nothing new — every record in docs.ndjson is already archived\n" +
		"  skipped 1 line — not JSON, or no \"text\"\n"
	if strings.Join(frames, " ") != "0/2 1/2 2/2" || !regexp.MustCompile(`\r +\r`+regexp.QuoteMeta(summary)+`$`).MatchString(out) {
		t.Fatalf("want frames 0/2 1/2 2/2, the line cleared, then the summary: %q", out)
	}

	out, errOut, err = captureBoth(t, func() error { return ingest("--quiet") }) // still a terminal line

	if err != nil || out != "" || errOut != "" {
		t.Fatalf("--quiet: stdout %q, stderr %q, %v", out, errOut, err)
	}
}
