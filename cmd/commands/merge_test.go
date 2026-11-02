package commands

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// The command half of `witness merge`: the files around the database, the backup, the refusals.
// The data rules themselves are pinned in internal/store/merge_test.go.

// mergeFixture builds an incoming archive and exports it with --all, then opens a separate base
// archive as the current one. seed runs against the incoming store before the export.
func mergeFixture(t *testing.T, seed func(st *store.Store)) (exportDir string) {
	t.Helper()
	t.Setenv(noDefaultLensEnv, "1")
	t.Setenv("WITNESS_HOME", t.TempDir())
	inc, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := inc.AppendRaw(store.RawRecord{TS: "2026-03-01T00:00:00Z", Session: "theirs", Seq: 0, Role: "user", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(inc)
	}
	exportDir = filepath.Join(t.TempDir(), "export")
	if _, err := inc.ExportAll(exportDir, false); err != nil {
		t.Fatal(err)
	}
	inc.Close()

	t.Setenv("WITNESS_HOME", t.TempDir())
	base, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	base.Close()
	return exportDir
}

func openBase(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func premergeBackups(t *testing.T, root string) []string {
	t.Helper()
	m, _ := filepath.Glob(root + ".premerge-*")
	return m
}

// answer is a scripted reply to the merge prompt; *asked records whether the prompt was shown.
func answer(reply bool, asked *bool) func(string) bool {
	return func(string) bool {
		if asked != nil {
			*asked = true
		}
		return reply
	}
}

// A lens that exists only in the export is registered but NOT enabled: enabling it would mine
// every session here, and a lens is a prompt someone else wrote. A portrait prompt is never
// copied — it would replace how this archive's portrait is written. The plan is shown before the
// question; declining writes nothing at all, files included, and FAILS — so `merge x && rm -rf x`
// cannot delete an archive that was never merged. Saying yes backs up first, then shows the undo.
func TestMergeCommandFilesAndBackup(t *testing.T) {
	export := mergeFixture(t, func(st *store.Store) {
		if err := st.RegisterLens("math", writeLensDir(t, "math", "mine math", "fold math")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(st.Root, "summarize"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.Root, "summarize", "unified.md"), []byte("PORTRAIT: the market"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	root := openBase(t).Root

	var asked bool
	var err error
	out := captureStdout(t, func() { err = cmdMerge(export, false, answer(false, &asked)) })
	if !errors.Is(err, errMergeDeclined) {
		t.Errorf("declining must fail the command, got %v", err)
	}
	if !asked || !strings.Contains(out, "math: will be registered, NOT enabled") {
		t.Errorf("the plan must be shown and then asked about (asked=%v):\n%s", asked, out)
	}
	st := openBase(t)
	if slices.Contains(st.RegisteredLenses(), "math") || len(premergeBackups(t, root)) != 0 {
		t.Fatal("a declined merge registered a lens or wrote a backup")
	}
	if recs, _ := st.ReadRaw("theirs"); len(recs) != 0 {
		t.Fatal("a declined merge wrote data")
	}

	out = captureStdout(t, func() {
		if err := cmdMerge(export, false, answer(true, nil)); err != nil {
			t.Fatal(err)
		}
	})
	st = openBase(t)
	if !slices.Contains(st.RegisteredLenses(), "math") {
		t.Error("the incoming-only lens was not registered")
	}
	if slices.Contains(st.LoadConfig().EnabledLenses, "math") {
		t.Error("the incoming lens was ENABLED: that mines every session here with someone else's prompt")
	}
	if _, err := os.Stat(filepath.Join(root, "summarize", "unified.md")); err == nil {
		t.Error("the incoming portrait prompt was copied; it must only be reported")
	}
	if !strings.Contains(out, "cp ") || !strings.Contains(out, "unified.md") {
		t.Errorf("the portrait prompt must be reported with the command to take it:\n%s", out)
	}
	backups := premergeBackups(t, root)
	if len(backups) != 1 {
		t.Fatalf("want exactly one pre-merge backup, got %v", backups)
	}
	if _, err := os.Stat(filepath.Join(backups[0], "witness.db")); err != nil {
		t.Errorf("the backup is not a complete archive: %v", err)
	}
	if recs, _ := st.ReadRaw("theirs"); len(recs) != 1 {
		t.Errorf("the incoming session did not arrive")
	}
	if !strings.Contains(out, backups[0]) || !strings.Contains(out, "to undo") {
		t.Errorf("a merge must end by naming its backup and how to undo:\n%s", out)
	}

	// A second run has no data to merge: it must not ask, and must not leave another backup behind.
	// The export still holds a portrait prompt that differs from ours, so it must say THAT — not
	// "everything is already here": prompt text is in no database, and that claim invites
	// discarding the export. --yes is passed with a declining prompt: had it asked, nothing merges.
	asked = false
	out = captureStdout(t, func() {
		if err := cmdMerge(export, true, answer(false, &asked)); err != nil {
			t.Fatal(err)
		}
	})
	if asked || !strings.Contains(out, "no data to merge") || !strings.Contains(out, "unified.md") ||
		strings.Contains(out, "already here") || len(premergeBackups(t, root)) != 1 {
		t.Errorf("a no-op re-run must not ask (asked=%v), must report the differing prompt and write no backup; got %d backups:\n%s",
			asked, len(premergeBackups(t, root)), out)
	}
}

// --yes merges without asking; the prompt is never shown.
func TestMergeCommandYesSkipsThePrompt(t *testing.T) {
	export := mergeFixture(t, nil)
	var asked bool
	_ = captureStdout(t, func() {
		if err := cmdMerge(export, true, answer(false, &asked)); err != nil {
			t.Fatal(err)
		}
	})
	st := openBase(t)
	if recs, _ := st.ReadRaw("theirs"); asked || len(recs) != 1 {
		t.Errorf("--yes must merge without asking (asked=%v, rows=%d)", asked, len(recs))
	}
}

// The production prompt: only y/yes merges. Anything else — including EOF, which is what a script
// with nobody to answer reads — is a no. The --yes hint is for EOF alone: the first version keyed
// it on "not a terminal", so `</dev/null` (a character device) got no hint and a piped "n" did.
func TestMergeAskYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, " y \n": true, "n\n": false, "\n": false, "": false, "yep\n": false} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(in)
		w.Close()
		old := os.Stdin
		os.Stdin = r
		var got bool
		out := captureStdout(t, func() { got = askYesNo("? ") })
		os.Stdin = old
		r.Close()
		if got != want {
			t.Errorf("askYesNo(%q) = %v, want %v", in, got, want)
		}
		if hinted := strings.Contains(out, "--yes"); hinted != (in == "") {
			t.Errorf("askYesNo(%q): --yes hint shown=%v; want it exactly when nobody answered:\n%s", in, hinted, out)
		}
	}
}

// While a worker holds the archive, merge refuses with an ERROR — not exit 0 as compact does — so
// `witness merge x --yes && rm -rf x` cannot delete the only copy after merging nothing.
func TestMergeCommandRefusesWhileTheWorkerRuns(t *testing.T) {
	export := mergeFixture(t, nil)
	st := openBase(t)
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take the worker lock for the test")
	}
	defer unlock()
	if err := cmdMerge(export, true, answer(false, nil)); err == nil || !strings.Contains(err.Error(), "nothing was merged") {
		t.Errorf("want a non-nil error saying nothing was merged, got %v", err)
	}
	if recs, _ := st.ReadRaw("theirs"); len(recs) != 0 {
		t.Error("data arrived despite the refusal")
	}
	if len(premergeBackups(t, st.Root)) != 0 {
		t.Error("a refused merge left a backup behind")
	}
}

// A single .db is accepted (plain `export` and every pre-v0.8 backup is one). A directory without
// witness.db is refused with the database it DOES hold — never with an export command aimed at
// that directory: an earlier draft's message exported THIS archive over the user's backup, and
// with --force deleted it.
func TestMergeSourceResolution(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "witness-2026-07.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveMergeSource(dir)
	if err == nil || !strings.Contains(err.Error(), "witness merge "+filepath.Join(dir, "witness-2026-07.db")) {
		t.Errorf("want a suggestion naming the .db it holds, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "export") {
		t.Errorf("the message must never suggest an export into the backup's own directory: %v", err)
	}
	db, d, err := resolveMergeSource(filepath.Join(dir, "witness-2026-07.db"))
	if err != nil || d != "" || !strings.HasSuffix(db, "witness-2026-07.db") {
		t.Errorf("a single .db must be accepted as-is, got db=%q dir=%q err=%v", db, d, err)
	}
}
