package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The screens of the commands that change something (wire/unwire, the worker, compact, merge,
// import, ingest, export, cleanup, install --path). Each test here pins a behaviour a pty audit
// found wrong — a false claim, a suggested command that does not exist, a typo that exited 0, a
// cause that reached only witness.log. Tests run with stdout piped, so they see the plain form.

func workerRunCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c, _, err := newWorkerCmd().Find([]string{"run"})
	if err != nil || c.Name() != "run" {
		t.Fatalf("worker run not found: %v", err)
	}
	return c
}

// `witness worker stpo` printed the group's help and exited 0, so a typo read as success.
func TestWorkerGroupRejectsATypo(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"worker", "stpo"})
	var err error
	_ = captureStdout(t, func() { err = root.Execute() })
	if err == nil || !strings.Contains(err.Error(), `unknown command "stpo" for "witness worker"`) ||
		!strings.Contains(err.Error(), "did you mean stop") {
		t.Fatalf("a typo'd subcommand must fail, with a suggestion; got %v", err)
	}
	root = newRootCmd()
	root.SetArgs([]string{"worker"})
	out := captureStdout(t, func() { err = root.Execute() })
	if err != nil || !strings.Contains(out, "Available Commands") {
		t.Fatalf("bare `witness worker` must still print its help and succeed: %v\n%s", err, out)
	}
}

// ingest's result says what happened. It printed "distilling in the background" every time,
// including after writing nothing — when no worker is started.
func TestIngestNeverClaimsDistillingWhenNothingWasWritten(t *testing.T) {
	ingest := func(t *testing.T, body string) string {
		t.Helper()
		input := filepath.Join(t.TempDir(), "notes.ndjson")
		if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c := newIngestCmd()
		c.SetArgs([]string{"--file", input})
		out, _, err := captureBoth(t, func() error { return c.Execute() })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for name, tc := range map[string]struct{ body, want string }{
		"empty file":     {"", "[!] notes.ndjson holds no records\n"},
		"only malformed": {"not json\n{\"text\":\"\"}\n", "[!] notes.ndjson holds no records\n  skipped 2 lines"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WITNESS_HOME", t.TempDir())
			if out := ingest(t, tc.body); !strings.HasPrefix(out, tc.want) || strings.Contains(out, "distilling") {
				t.Fatalf("got:\n%s\nwant prefix:\n%s", out, tc.want)
			}
		})
	}
	t.Run("no lens enabled", func(t *testing.T) {
		t.Setenv("WITNESS_HOME", t.TempDir())
		t.Setenv(noDefaultLensEnv, "1")
		out := ingest(t, `{"text":"one","id":"a"}`+"\n")
		if !strings.Contains(out, "ingested 1 record into 1 session — no lens is enabled") || strings.Contains(out, "distilling in the background") {
			t.Fatalf("a worker with no lens distills nothing; the line must say so:\n%s", out)
		}
	})
}

// A runner that is down for every call: the shape of "claude is not installed", "the provider is
// down", or WITNESS_DISABLE_EXTERNAL_RUNNERS.
type downFakePlatform struct{ leakFakePlatform }

func (downFakePlatform) Name() string                           { return "fakedown" }
func (downFakePlatform) SessionPrefix() string                  { return "fakedown:" }
func (downFakePlatform) NewRunner(store.Config) platform.Runner { return downFakeRunner{} }

type downFakeRunner struct{ leakFakeRunner }

func (downFakeRunner) Close() error { return nil }
func (downFakeRunner) Run(context.Context, string, string, string) (string, error) {
	return "", errors.New("provider down: 503 Service Unavailable")
}

var registerDownFake sync.Once

// compact asked "Apply this plan?" over a plan of nothing when every model call failed, and the
// reason was only in witness.log. Now: no prompt, a non-zero exit, and the cause on the screen.
func TestCompactFailsWithTheCauseWhenNothingCouldBePlanned(t *testing.T) {
	compactFixture(t)
	registerDownFake.Do(func() { platform.Register(downFakePlatform{}) })
	st := openBase(t)
	if err := st.SetConfigString("runner", "fakedown"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var asked bool
	var err error
	_ = captureStdout(t, func() { err = cmdCompact(false, "", answer(true, &asked)) })
	if asked {
		t.Error("it asked to apply a plan that does not exist")
	}
	if err == nil || !strings.Contains(err.Error(), "compaction could not plan") ||
		!strings.Contains(err.Error(), "provider down: 503 Service Unavailable") {
		t.Fatalf("want a failure naming the model error, got %v", err)
	}
	if a, ar := compactCounts(t); a != 3 || ar != 0 {
		t.Fatalf("a failed plan changed the facets: %d active, %d archived", a, ar)
	}
}

func TestCompactRefusalsReadAsWords(t *testing.T) {
	got := refusals(map[string]int{"merge_too_small": 1, "absorb_unseen_facet": 2})
	if want := "2 absorb unseen facet · 1 merge too small"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// One busy-worker message for compact, worker run and merge: the pid, and both ways forward.
// worker run used to print its "distilling the full backlog" banner before finding the lock taken.
func TestABusyWorkerIsOneMessage(t *testing.T) {
	export := mergeFixture(t, nil)
	st := openBase(t)
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take the worker lock")
	}
	defer unlock()
	if err := st.SetMetaString("worker_pid", "4123"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { _ = cmdCompact(false, "", answer(false, nil)) })
	if !strings.HasPrefix(out, "[!] a distillation worker is running (pid 4123) — compact skipped\n") ||
		!strings.Contains(out, "witness status") || !strings.Contains(out, "witness worker stop") {
		t.Errorf("compact:\n%s", out)
	}
	var runErr error
	out = captureStdout(t, func() { runErr = cmdDistillBackfill(false, "", "", false) })
	if runErr != nil || !strings.HasPrefix(out, "[!] a distillation worker is running (pid 4123)") || strings.Contains(out, "distilling the backlog") {
		t.Errorf("worker run must check the lock before its banner (err %v):\n%s", runErr, out)
	}
	err := cmdMerge(export, true, nil)
	if err == nil || !strings.Contains(err.Error(), "(pid 4123) — nothing was merged") || !strings.Contains(err.Error(), "witness worker stop") {
		t.Errorf("merge must fail with the same message: %v", err)
	}
}

func TestCandidatesRefusesAnUnknownLens(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv(noDefaultLensEnv, "1")
	_, errOut, err := captureBoth(t, func() error { return cmdCandidates("nope") })
	if err == nil || !strings.Contains(err.Error(), `lens "nope" is not registered`) {
		t.Fatalf("an unknown lens reported success: %v", err)
	}
	if errOut != "" {
		t.Errorf("nothing should run for a lens that does not exist, but it drew %q", errOut)
	}
}

// Observations were cut by BYTES, splitting a multi-byte character into U+FFFD.
func TestTruncateCutsRunesNotBytes(t *testing.T) {
	got := truncate("ab—cd—ef", 4)
	if !utf8.ValidString(got) || got != "ab—…" {
		t.Fatalf("truncate = %q", got)
	}
}

// Every lens row leads with a glyph so the names line up — in a pipe too, where the glyphs are
// ASCII of different widths ("+" against "[!]"): there they used to start 2 columns apart.
func TestMergeLensRowsLineUpInPlainOutput(t *testing.T) {
	f := &mergeFiles{srcDir: "/x", register: []mergeLens{{"math", "/x/lenses/math"}},
		differ: []mergeLens{{"default", "/x/lenses/default"}}, disabled: []string{"notes"}, unknown: []string{"old"}}
	out := captureStdout(t, func() { printMergeFiles("/root", f) })
	col := -1
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  lens") {
			continue
		}
		name := -1
		for _, n := range []string{"math", "default", "notes", "old"} {
			if i := strings.Index(line, " "+n+"  "); i >= 0 {
				name = i
			}
		}
		if col == -1 {
			col = name
		} else if name != col {
			t.Fatalf("lens names start at different columns (%d, %d):\n%s", col, name, out)
		}
	}
	if col == -1 {
		t.Fatalf("no lens rows:\n%s", out)
	}
}

// The no-op import of a hook-fed agent still kicks the worker when work is pending; the watch hint
// it prints then needs the line to say so, or "nothing to import" sits over "watch it".
func TestImportOfAHookFedAgentSaysWhenItStartedTheWorker(t *testing.T) {
	out := captureStdout(t, func() { printImportResult(platform.ImportStats{Agent: "claude"}, true, false) })
	if !strings.HasPrefix(out, "[-] claude: nothing to import — it is captured live by its hooks (distilling in the background)\n") ||
		!strings.Contains(out, "witness status") {
		t.Fatalf("got:\n%s", out)
	}
}

// A cause row with no cause is dropped, not printed as a bare label.
func TestAnEmptyCauseRowIsDropped(t *testing.T) {
	err := errLines("compaction could not plan", errRow("cause", ""), errHint("diagnose", "witness doctor"))
	if strings.Contains(err.Error(), "cause") {
		t.Fatalf("a bare cause label explains nothing:\n%v", err)
	}
}

// `worker review` said "profile regenerated" whatever regenerateProfile did. When the review lands
// but the narrative cannot be regenerated — here its prompts cannot be found — it says so, with why.
func TestReviewSaysWhenTheProfileWasNotRegenerated(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv(noDefaultLensEnv, "1")
	t.Setenv("WITNESS_PROMPTS", filepath.Join(t.TempDir(), "none"))
	out, err := captureStdoutErr(t, func() error { return cmdReviewFull(false) })
	if err != nil || !strings.HasPrefix(out, "[!] review complete — the profile was NOT regenerated") ||
		!strings.Contains(out, "cause") || strings.Contains(out, "profile regenerated\n") {
		t.Fatalf("(%v)\n%s", err, out)
	}
}

func TestMergeNamesAMissingSource(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	err := cmdMerge(filepath.Join(t.TempDir(), "nope"), true, nil)
	if err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "witness export --all") ||
		strings.Contains(err.Error(), "stat ") {
		t.Fatalf("got %v", err)
	}
}

func TestImportSaysWhatItDidAndWhy(t *testing.T) {
	codexRollouts(t)
	out, err := captureStdoutErr(t, func() error { return cmdImport([]string{"--agent", "claude", "--no-kick"}) })
	if err != nil || !strings.HasPrefix(out, "[-] claude: nothing to import — it is captured live by its hooks") ||
		!strings.Contains(out, "witness wire claude") {
		t.Errorf("claude import is a no-op by design and must say so:\n%s (%v)", out, err)
	}
	out, err = captureStdoutErr(t, func() error { return cmdImport([]string{"--agent", "codex", "--no-kick"}) })
	if err != nil || !strings.HasPrefix(out, "[ok] codex: up to date — nothing new since the last import") {
		t.Errorf("an empty codex import:\n%s (%v)", out, err)
	}

	agents := strings.Join(editorNames(), ", ")
	for _, agent := range []string{"vscode", "file"} { // file is pushed in by `witness ingest`, never imported
		if err := cmdImport([]string{"--agent", agent}); err == nil || err.Error() != `unknown agent "`+agent+`" — want one of: `+agents {
			t.Errorf("unknown agent %s: %v", agent, err)
		}
	}
	if err := cmdImport(nil); err == nil || !strings.HasPrefix(err.Error(), "which agent? witness import --agent <") {
		t.Errorf("bare import: %v", err)
	}
	t.Setenv("WITNESS_CODEX_HOME", filepath.Join(t.TempDir(), "nocodex"))
	if err := cmdImport([]string{"--agent", "codex"}); err == nil || !strings.Contains(err.Error(), "nocodex does not exist") ||
		!strings.Contains(err.Error(), "is codex installed here?") || strings.Contains(err.Error(), "stat ") {
		t.Errorf("a missing Codex home: %v", err)
	}
}

func TestWireNamesTheEditorChoices(t *testing.T) {
	want := strings.Join(editorNames(), "|")
	for _, c := range []*cobra.Command{newWireCmd(), newUnwireCmd()} {
		err := c.Args(c, nil)
		if err == nil || err.Error() != "which editor? witness "+c.Name()+" <"+want+">" {
			t.Errorf("%s with no editor: %v", c.Name(), err)
		}
	}
	for _, run := range []func([]string) error{cmdInstall, cmdUninstall} {
		err := run([]string{"vscode"})
		if err == nil || err.Error() != `unknown editor "vscode" — want one of: `+strings.Join(editorNames(), ", ") {
			t.Errorf("unknown editor: %v", err)
		}
	}
}

// The codex-style "runs no model" row names the runner the lenses will ACTUALLY use, resolved the
// way the worker resolves it — not the raw config field.
func TestWiringACaptureOnlyVenueNamesTheEffectiveRunner(t *testing.T) {
	const venue = "fakecaptureonly2"
	called := false
	registerFakeCaptureOnlyEditor(t, venue, &called)
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv(noDefaultLensEnv, "1")
	t.Setenv("WITNESS_RUNNER", "opencode") // unbound archive: the env decides, as for an npm OpenCode user
	out, err := captureStdoutErr(t, func() error { return cmdInstall([]string{venue}) })
	if err != nil || !strings.Contains(out, "runner        unchanged ("+venue+" runs no model; lenses use opencode)") {
		t.Fatalf("got (%v):\n%s", err, out)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); !strings.HasPrefix(lines[0], "witness wire "+venue) {
		t.Errorf("the screen opens with its title:\n%s", out)
	}
}

// unwire said "removed (if it was present)" on every run. It now says what it removed.
func TestUnwireSaysWhetherAnythingWasRemoved(t *testing.T) {
	t.Run("claude, nothing wired", func(t *testing.T) {
		withClaudeDir(t)
		removeClaudeMCP = func() error { return errors.New("exit status 1") } // claude: "No MCP server named witness"
		out, err := captureStdoutErr(t, func() error { return cmdUninstall([]string{"claude"}) })
		if err != nil || !strings.Contains(out, "[-]  hooks") || !strings.Contains(out, "[-]  MCP server    not registered") ||
			!strings.Contains(out, "[-] nothing to remove") {
			t.Fatalf("(%v)\n%s", err, out)
		}
	})
	t.Run("claude, wired", func(t *testing.T) {
		dir := withClaudeDir(t)
		wired := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/hooks/witness.sh capture"}]}]}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(wired), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdoutErr(t, func() error { return cmdUninstall([]string{"claude"}) })
		if err != nil || !strings.Contains(out, "[ok] hooks         removed from") || !strings.Contains(out, "[ok] unwired") ||
			!strings.Contains(out, "witness wire claude") {
			t.Fatalf("(%v)\n%s", err, out)
		}
	})
	t.Run("claude not on PATH", func(t *testing.T) {
		withClaudeDir(t)
		removeClaudeMCP = func() error { return &exec.Error{Name: "claude", Err: exec.ErrNotFound} }
		out, _ := captureStdoutErr(t, func() error { return cmdUninstall([]string{"claude"}) })
		// The row warns, so the result must too: "[-] nothing to remove" under a "[!] not checked"
		// row claimed the MCP server was known to be gone.
		if !strings.Contains(out, "[!]  MCP server    not checked (`claude` is not on PATH)") ||
			!strings.Contains(out, "[!] nothing removed, and the MCP server was not checked") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("claude mcp remove wedged", func(t *testing.T) {
		dir := withClaudeDir(t)
		wired := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/hooks/witness.sh capture"}]}]}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(wired), 0o600); err != nil {
			t.Fatal(err)
		}
		removeClaudeMCP = func() error { return fmt.Errorf("timed out: %w", context.DeadlineExceeded) }
		out, _ := captureStdoutErr(t, func() error { return cmdUninstall([]string{"claude"}) })
		if !strings.Contains(out, "[!] unwired, but the MCP server may still be registered") ||
			!strings.Contains(out, "claude mcp remove witness") || strings.Contains(out, "[ok] unwired") {
			t.Fatalf("got:\n%s", out)
		}
	})
	t.Run("opencode leaves a config without witness byte-for-byte alone", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("OPENCODE_CONFIG_DIR", dir)
		t.Setenv("OPENCODE_CONFIG", "")
		cfg := filepath.Join(dir, "opencode.jsonc")
		body := "{\n  // my model\n  \"model\": \"x\",\n}\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdoutErr(t, func() error { return cmdUninstall([]string{"opencode"}) })
		if err != nil || !strings.Contains(out, "[-] nothing to remove") || strings.Contains(out, "if it was present") {
			t.Fatalf("(%v)\n%s", err, out)
		}
		if got, _ := os.ReadFile(cfg); string(got) != body {
			t.Errorf("unwire rewrote a config witness was not in, dropping its comments:\n%s", got)
		}
		plugin := openCodePluginPath(dir, openCodePluginName)
		if err := os.MkdirAll(filepath.Dir(plugin), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plugin, []byte("//"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, _ = captureStdoutErr(t, func() error { return cmdUninstall([]string{"opencode"}) })
		if !strings.Contains(out, "[ok] plugin        removed") || !strings.Contains(out, "[ok] unwired") || strings.Contains(out, legacyOpenCodePluginName) {
			t.Fatalf("got:\n%s", out)
		}
	})
}

func TestMCPAddFailureNamesTheCause(t *testing.T) {
	for err, want := range map[error]string{
		&exec.Error{Name: "claude", Err: exec.ErrNotFound}:    "`claude` is not on PATH",
		fmt.Errorf("timed out: %w", context.DeadlineExceeded): "did not finish within",
		errors.New("signal: killed"):                          "`claude mcp add` failed: signal: killed",
	} {
		if got := mcpAddFailure(err); !strings.Contains(got, want) {
			t.Errorf("mcpAddFailure(%v) = %q, want %q", err, got, want)
		}
	}
	if got := mcpAddCommand(shellInvocation("/My Dir/hooks/witness.sh")); got != "claude mcp add -s user witness '/My Dir/hooks/witness.sh' mcp" {
		t.Errorf("the by-hand command must paste as-is: %q", got)
	}
}

// install --path says whether it created or adopted the archive, and its next steps carry the
// WITNESS_HOME that selects it — printed bare, they acted on the default archive.
func TestProvisionSaysCreatedOrAdoptedAndSelectsTheArchive(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir()) // restored after the test: cmdProvision overwrites it
	t.Setenv(noDefaultLensEnv, "")
	dir := filepath.Join(t.TempDir(), "market")
	out, err := captureStdoutErr(t, func() error { return cmdProvision(dir) })
	if err != nil || !strings.HasPrefix(out, "[ok] archive created at ") ||
		!strings.Contains(out, "WITNESS_HOME="+shellArg(dir)+" witness lens register <name> <dir>") {
		t.Fatalf("(%v)\n%s", err, out)
	}
	out, err = captureStdoutErr(t, func() error { return cmdProvision(dir) })
	if err != nil || !strings.HasPrefix(out, "[ok] archive adopted at ") || !strings.Contains(out, "(0 sessions)") {
		t.Fatalf("(%v)\n%s", err, out)
	}
	if err := cmdProvision("/dev/null/x"); err == nil || !strings.HasPrefix(err.Error(), "cannot create an archive at /dev/null/x") {
		t.Errorf("a path that cannot hold an archive: %v", err)
	}
}

func TestStatusRowsAndTheNextStep(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	out := captureStdout(t, func() { _ = cmdDistillStatus(false) })
	for _, want := range []string{"witness status\n\n", "  worker        - idle\n", "  queue         0 pending · 0 backing off\n",
		"[!] nothing captured yet — connect an editor\n", "witness wire claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "worker:") || strings.Contains(out, "○") {
		t.Errorf("the plain form keeps one label style and ASCII glyphs:\n%s", out)
	}
	if got := behind("2026-11-02T06:34:00Z", "2026-11-02T05:29:00Z"); got != "1h05m behind" {
		t.Errorf("behind = %q", got)
	}
}

// cleanup's confirmation goes through the shared prompt, and reading the day count must not
// swallow the answer that follows it on a pipe.
func TestCleanupAsksThroughTheSharedPrompt(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st := openBase(t)
	if err := st.AppendRaw(store.RawRecord{TS: "2025-01-01T00:00:00Z", Session: "old", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var asked bool
	var err error
	out := captureStdout(t, func() { err = cmdCleanup(strings.NewReader("30\n"), answer(false, &asked)) })
	if err != nil || !asked || !strings.Contains(out, "deletes       1 raw message from 1 session") ||
		!strings.HasSuffix(out, "not deleted — nothing written\n") {
		t.Fatalf("decline (%v, asked=%v):\n%s", err, asked, out)
	}
	out = captureStdout(t, func() { err = cmdCleanup(strings.NewReader("30\n"), answer(true, nil)) })
	if err != nil || !strings.HasSuffix(out, "[ok] removed 1 raw message from 1 session\n") {
		t.Fatalf("accept (%v):\n%s", err, out)
	}
	out = captureStdout(t, func() { err = cmdCleanup(strings.NewReader("30\n"), answer(true, nil)) })
	if err != nil || !strings.HasSuffix(out, "[-] nothing to clean — no session idle for 30 days\n") {
		t.Fatalf("nothing left (%v):\n%s", err, out)
	}

	in := strings.NewReader("30\ny\n")
	if got, eof := readLine(in); got != "30" || eof {
		t.Fatalf("readLine = %q, eof %v", got, eof)
	}
	if rest, _ := readLine(in); rest != "y" {
		t.Fatalf("readLine consumed past its newline; the next reader got %q", rest)
	}
	if _, eof := readLine(in); !eof {
		t.Fatal("an exhausted input must report EOF")
	}

	// The production confirmation, on a pipe that ends before any answer (cron, /dev/null): a no —
	// and NOT askYesNo's "pass --yes to skip this question", because cleanup has no --yes
	// (`witness cleanup --yes` is an unknown flag). It used to print exactly that.
	st = openBase(t)
	if err := st.AppendRaw(store.RawRecord{TS: "2025-01-01T00:00:00Z", Session: "old2", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	r, w, perr := os.Pipe() // the real command, reading a real (non-terminal) stdin
	if perr != nil {
		t.Fatal(perr)
	}
	_, _ = w.WriteString("30\n")
	_ = w.Close()
	prevStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = prevStdin; _ = r.Close() })
	out = captureStdout(t, func() { err = newCleanupCmd().RunE(nil, nil) })
	if err != nil || strings.Contains(out, "--yes") || !strings.HasSuffix(out, "[y/N]: \nnot deleted — nothing written\n") {
		t.Fatalf("an unanswered prune (%v):\n%s", err, out)
	}
	if newCleanupCmd().Flags().Lookup("yes") != nil {
		t.Fatal("cleanup grew a --yes: confirmFrom exists only because it has none")
	}
}

func TestExportSaysWhatTheSnapshotHolds(t *testing.T) {
	seedArchiveForExport(t)
	dst := filepath.Join(t.TempDir(), "full")
	out, err := captureStdoutErr(t, func() error { return cmdExport(dst, false, true, false) })
	if err != nil || !strings.HasPrefix(out, "[ok] exported the complete archive to ") ||
		!strings.Contains(out, "    witness.db    ") || !strings.Contains(out, " sessions · ") ||
		!strings.Contains(out, "witness merge "+shellArg(dst)) || strings.Contains(out, " 1 files") {
		t.Fatalf("(%v)\n%s", err, out)
	}
	db := filepath.Join(t.TempDir(), "snap.db")
	out, err = captureStdoutErr(t, func() error { return cmdExport(db, false, false, false) })
	if err != nil || !strings.Contains(out, "witness export --all "+shellArg(strings.TrimSuffix(db, ".db"))) {
		t.Fatalf("(%v)\n%s", err, out)
	}
}

// causeTap is what puts a failure's cause on the screen: the FIRST error, on one line, without
// the child's color escapes, and never the user's own Ctrl-C.
func TestCauseTapKeepsTheFirstErrorOnOneLine(t *testing.T) {
	tap := &causeTap{}
	calls := 0
	run := tap.wrap(func(context.Context, string, string, string) (string, error) {
		calls++
		switch calls {
		case 1:
			return "", context.Canceled
		case 2:
			return "", errors.New("claude -p failed: exit status 1 (stderr: \x1b[31mAWS auth\n  refresh timed out\x1b[0m)")
		}
		return "", errors.New("a later error")
	})
	for range 3 {
		_, _ = run(context.Background(), "", "", "")
	}
	if got, want := tap.cause(), "claude -p failed: exit status 1 (stderr: AWS auth refresh timed out)"; got != want {
		t.Fatalf("cause = %q, want %q", got, want)
	}
	long := &causeTap{}
	long.note(errors.New(strings.Repeat("x", 300)))
	if n := utf8.RuneCountInString(long.cause()); n != 100 {
		t.Errorf("a long error is cut to 100 characters, got %d", n)
	}
	var none *causeTap
	none.note(errors.New("x")) // a nil tap is how background runs opt out
	if none.cause() != "" {
		t.Error("a nil tap has no cause")
	}
}
