package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/view"
)

// seedViewArchive lays down a small archive with every piece the page reads — a conversation
// turn, a note, a trait citing it, and both portraits — and returns its data dir, closed.
func seedViewArchive(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const ts = "2026-10-01T10:00:00Z"
	if err := st.AppendRaw(store.RawRecord{TS: ts, Session: "s1", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendObservations([]store.Observation{{
		ID: "obs_1", TS: ts, Session: "s1", Lens: store.LensDefault, Dimension: "thinking",
		Observation: "asks why before how", Evidence: "hi", Poignancy: 5, Source: "mined",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFacets([]store.Facet{viewFacet(store.LensDefault, "asks why first")}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteProfile(store.LensDefault, "# default\n\nYou ask why first.\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteProfile(store.ProfileUnified, "# Who you are\n\nYou ask why first.\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return home
}

func viewFacet(lensName, value string) store.Facet {
	const ts = "2026-10-01T10:00:00Z"
	return store.Facet{
		Lens: lensName, Dimension: "thinking", Key: "why_first", LastSeen: ts,
		Versions: []store.FacetVersion{{
			Value: value, ValidFrom: ts, RecordedAt: ts, Confidence: 0.7, BecauseOf: []string{"obs_1"},
		}},
	}
}

// runView runs `witness view <args>` through its cobra command, so the flags are the real ones.
func runView(t *testing.T, args ...string) string {
	t.Helper()
	c := newViewCmd()
	c.SetArgs(append([]string{}, args...)) // never nil: cobra would read the test binary's os.Args
	out, err := captureStdoutErr(t, c.Execute)
	if err != nil {
		t.Fatalf("witness view %v: %v", args, err)
	}
	return out
}

// viewModel reads the page's data block back into the model it was rendered from.
func viewModel(t *testing.T, path string) view.Model {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := bytes.Cut(b, []byte(`<script type="application/json" id="data">`))
	data, _, ok2 := bytes.Cut(rest, []byte(`</script>`))
	if !ok || !ok2 {
		t.Fatalf("no data block in %s", path)
	}
	var m view.Model
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("data block: %v", err)
	}
	return m
}

// The default path is <data-root>/view.html, private, and named on screen. A snapshot left
// world-readable by something else (a copy, an older build) is REPLACED rather than rewritten
// in place — it carries the archive's own notes, and an in-place write keeps the old mode.
func TestViewWritesFile(t *testing.T) {
	home := seedViewArchive(t)
	path := filepath.Join(home, "view.html")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // past the umask, so the old mode is real
		t.Fatal(err)
	}

	out := runView(t, "--no-open")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("<!doctype html>")) {
		t.Errorf("view.html does not start with <!doctype html>: %.40q", b)
	}
	if runtime.GOOS != "windows" { // Windows has no POSIX mode bits
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("view.html mode = %v (err %v), want 0600", info.Mode().Perm(), err)
		}
	}
	if !strings.Contains(out, tildePath(path)) {
		t.Errorf("the output does not name the file %s:\n%s", path, out)
	}
	// --no-open says how to open it instead, and never claims it opened anything.
	if !strings.Contains(out, fileURL(path)) || strings.Contains(out, "in your browser") {
		t.Errorf("--no-open should print the file URL and no 'in your browser' line:\n%s", out)
	}
}

func TestViewOutFlag(t *testing.T) {
	home := seedViewArchive(t)
	dst := filepath.Join(t.TempDir(), "x.html")

	out := runView(t, "--no-open", "--out", dst)

	if b, err := os.ReadFile(dst); err != nil || !bytes.HasPrefix(b, []byte("<!doctype html>")) {
		t.Fatalf("--out file: err=%v prefix=%.40q", err, b)
	}
	if _, err := os.Stat(filepath.Join(home, "view.html")); !os.IsNotExist(err) {
		t.Errorf("--out must not also write the default path (stat err %v)", err)
	}
	if !strings.Contains(out, tildePath(dst)) {
		t.Errorf("the output does not name %s:\n%s", dst, out)
	}
}

// No test may launch a browser: without --no-open the opener is still never reached under go
// test, whatever a test installed in the var.
func TestViewNeverOpensUnderTest(t *testing.T) {
	home := seedViewArchive(t)
	calls := 0
	prev := openInBrowser
	openInBrowser = func(string) error { calls++; return nil }
	t.Cleanup(func() { openInBrowser = prev })

	runView(t)

	if calls != 0 {
		t.Errorf("openInBrowser was called %d times under go test", calls)
	}
	if _, err := os.Stat(filepath.Join(home, "view.html")); err != nil {
		t.Errorf("the file must still be written: %v", err)
	}
}

// `witness view` writes only its HTML. Every table is byte-identical (VACUUM INTO before and
// after), no portrait is touched, and the data dir gains exactly one entry — so taking the
// WorkerLock (.worker.lock) or opening witness.log would show here too.
func TestViewIsReadOnly(t *testing.T) {
	home := seedViewArchive(t)
	snap := func(name string) ([]byte, []string, map[string]time.Time) {
		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), name)
		if err := st.Export(dst, false); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		entries, _ := os.ReadDir(home)
		for _, e := range entries {
			names = append(names, e.Name())
		}
		mtimes := map[string]time.Time{}
		profiles, _ := filepath.Glob(filepath.Join(home, "profile", "*.md"))
		for _, p := range profiles {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			mtimes[filepath.Base(p)] = info.ModTime()
		}
		return db, names, mtimes
	}
	beforeDB, beforeNames, beforeMtimes := snap("before.db")
	if len(beforeMtimes) != 2 {
		t.Fatalf("precondition: want 2 portraits, got %v", beforeMtimes)
	}

	runView(t, "--no-open")

	afterDB, afterNames, afterMtimes := snap("after.db")
	if !bytes.Equal(beforeDB, afterDB) {
		t.Error("the database changed")
	}
	if want := slices.Sorted(slices.Values(append(beforeNames, "view.html"))); !slices.Equal(afterNames, want) {
		t.Errorf("data dir entries: got %v, want %v", afterNames, want)
	}
	for name, before := range beforeMtimes {
		if !afterMtimes[name].Equal(before) {
			t.Errorf("profile/%s was rewritten", name)
		}
	}
}

// A portrait `witness profile` would not rewrite must not read "out of date". Stale compares
// signatures that embed the model, and the model depends on the RESOLVED runner, so the view
// has to build its Summarizer exactly as the readers that write the profile do. The setup makes
// the raw config and the resolved runner disagree (WITNESS_RUNNER on an unbound archive, a lens
// with its own runner); a Summarizer built from the raw config computes a different model and
// would mark every portrait stale. The profile is written here with the worker's resolution
// (worker.go), independently of the code under test.
func TestViewFreshProfileIsNotStale(t *testing.T) {
	seedViewArchive(t)
	t.Setenv("WITNESS_RUNNER", "opencode")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := t.TempDir()
	for name, body := range map[string]string{
		"extract.md": "MINE", "review.md": "REVIEW", "lens.json": `{"name":"side","runner":"opencode"}`,
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RegisterLens("side", src); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("side"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfigString("distill_model", "review-model"); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFacets([]store.Facet{viewFacet(store.LensDefault, "asks why first"), viewFacet("side", "keeps notes")}); err != nil {
		t.Fatal(err)
	}
	cfg := st.LoadConfig()
	if resolved := st.ResolveRunner(cfg); resolved == cfg.Runner {
		t.Fatalf("precondition: the resolved runner (%q) must differ from the raw config's", resolved)
	}

	cfg.Runner = st.ResolveRunner(cfg) // the worker's resolution
	lenses, err := activeLenses(st)
	if err != nil {
		t.Fatal(err)
	}
	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts(st.Root)
	if err != nil {
		t.Fatal(err)
	}
	sm := &distill.Summarizer{Store: st, Config: cfg, Lenses: lenses, LensPrompt: lensPrompt, UnifiedPrompt: unifiedPrompt,
		Run: func(_ context.Context, _, _, input string) (string, error) { return "You keep notes.", nil }}
	if err := sm.Summarize(context.Background()); err != nil {
		t.Fatal(err)
	}

	stale := func() map[string]bool {
		runView(t, "--no-open")
		got := map[string]bool{}
		for _, p := range viewModel(t, filepath.Join(st.Root, "view.html")).Portraits {
			got[p.ID] = p.Stale
		}
		return got
	}
	if got := stale(); len(got) != 3 || got["side"] || got[store.LensDefault] || got[store.ProfileUnified] {
		t.Errorf("a freshly written profile reads as out of date (want 3 portraits, none stale): %v", got)
	}

	// And the line is wired at all: a changed trait makes its notebook (and the overall
	// portrait written from it) stale.
	if err := st.WriteFacets([]store.Facet{viewFacet(store.LensDefault, "asks why first"), viewFacet("side", "keeps fewer notes")}); err != nil {
		t.Fatal(err)
	}
	if got := stale(); !got["side"] || !got[store.ProfileUnified] || got[store.LensDefault] {
		t.Errorf("after a trait changed: got %v, want side and unified stale, default current", got)
	}
}

// Over 50 MiB the file is still written, with a warning. The threshold is a var only so this
// test need not write 50 MiB.
func TestViewWarnsWhenTheFileIsLarge(t *testing.T) {
	if viewWarnSize != 50<<20 {
		t.Fatalf("viewWarnSize = %d, want 50 MiB", viewWarnSize)
	}
	home := seedViewArchive(t)
	if out := runView(t, "--no-open"); strings.Contains(out, "large") {
		t.Errorf("a small file must not warn:\n%s", out)
	}
	prev := viewWarnSize
	viewWarnSize = 1
	t.Cleanup(func() { viewWarnSize = prev })

	out := runView(t, "--no-open")

	if !strings.Contains(out, "large") {
		t.Errorf("no size warning:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "view.html")); err != nil {
		t.Errorf("a large file must still be written: %v", err)
	}
}

// Any opener failure means "here is the path": a link the terminal can open, spaces escaped.
func TestViewOpenFailureNamesThePath(t *testing.T) {
	out, _ := captureStdoutErr(t, func() error {
		reportOpen("/tmp/my archive/view.html", errors.New("no display"))
		return nil
	})
	if !strings.Contains(out, "file:///tmp/my%20archive/view.html") || !strings.Contains(out, "no display") {
		t.Errorf("an opener error should give the cause and the file URL:\n%s", out)
	}
	out, _ = captureStdoutErr(t, func() error { reportOpen("/tmp/view.html", nil); return nil })
	if strings.Contains(out, "file://") || !strings.Contains(out, "in your browser") {
		t.Errorf("a successful open should say so, without a hint:\n%s", out)
	}
	// A Windows drive path needs the third slash, or "C:" reads as the host.
	if got := fileURL("C:/Users/me/view.html"); got != "file:///C:/Users/me/view.html" {
		t.Errorf("fileURL(C:/…) = %q, want file:///C:/Users/me/view.html", got)
	}
}
