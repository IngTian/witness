package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The read area's behaviour fixes: each test names the defect it pins. Tests run without a TTY,
// so every captured string is the plain (pipe / NO_COLOR) form.

// readFixture opens a fresh archive with the bundled prompts reachable and returns it.
func readFixture(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "w"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_RUNNER", "")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// registerRenamedLens registers a lens under `name` whose lens.json mines it as `mined` — the
// shape the shipped corpus-example lens had until F153 (registered corpus-example, mines as "regime").
func registerRenamedLens(t *testing.T, st *store.Store, name, mined string) {
	t.Helper()
	dir := t.TempDir()
	for file, body := range map[string]string{
		"extract.md": "mine\n", "review.md": "review\n",
		"lens.json": `{"name":"` + mined + `","dimensions":["regimes"]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RegisterLens(name, dir); err != nil {
		t.Fatal(err)
	}
}

// runCLI executes the real command tree with args, returning stdout and the error Run would report.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		root := newRootCmd()
		root.SetArgs(args)
		err = root.Execute()
	})
	return out, err
}

// `witness lens frob` printed the lens help page and exited 0 — cobra checks unknown subcommands
// on the root only — so a typo read as success to a person and to a script.
func TestGroupCommandsRejectATypoButStillPrintHelpBare(t *testing.T) {
	readFixture(t)
	for _, group := range []string{"lens", "config", "observations", "worker"} {
		if _, err := runCLI(t, group, "frob"); err == nil || !strings.Contains(err.Error(), `unknown command "frob"`) {
			t.Errorf("witness %s frob must fail as an unknown command, got %v", group, err)
		}
		out, err := runCLI(t, group)
		if err != nil || !strings.Contains(out, "Usage:") {
			t.Errorf("bare witness %s must still print its help and succeed: err=%v\n%s", group, err, out)
		}
	}
	if _, err := runCLI(t, "lens", "enabel"); err == nil || !strings.Contains(err.Error(), "did you mean enable?") {
		t.Errorf("a near-miss should suggest the real subcommand like the root does, got %v", err)
	}
	if _, err := runCLI(t, "worker", "stpo"); err == nil || !strings.Contains(err.Error(), "did you mean stop?") {
		t.Errorf("worker is a group too: a near-miss should suggest stop, got %v", err)
	}
}

// `lens show` printed the lens.json name, so for a lens registered as corpus-example (mining as
// "regime") its own footer said `witness lens show regime --prompts` — which fails, "not registered".
func TestLensShowSuggestsTheRegisteredNameAndTheCommandWorks(t *testing.T) {
	st := readFixture(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	out, err := runCLI(t, "lens", "show", "corpus-example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "witness lens show corpus-example --prompts") || strings.Contains(out, "show regime") {
		t.Fatalf("the suggested command must use the registered name:\n%s", out)
	}
	if !strings.Contains(out, `mines as "regime"`) {
		t.Errorf("the mined name should still be visible when it differs:\n%s", out)
	}
	if _, err := runCLI(t, "lens", "show", "corpus-example", "--prompts"); err != nil {
		t.Errorf("the command the card suggests must work: %v", err)
	}
}

// config list and doctor read the raw review_model field and said "(runner default)", while the
// engine (distill.ModelFor) reviews on mine_model whenever review_model is empty.
func TestEffectiveConfigShowsTheModelReviewActuallyRunsOn(t *testing.T) {
	st := readFixture(t)
	if err := configApplySet(st, "mine_model", "sonnet-x", ""); err != nil {
		t.Fatal(err)
	}
	got := map[string]setting{}
	for _, s := range effectiveConfig(st, nil) {
		got[s.key] = s
	}
	if r := got["review_model"]; r.value != "sonnet-x" || r.from != "falls back to mine_model" {
		t.Errorf("review_model must show the fallback it really runs on, got %+v", r)
	}
	if v, _ := configReadValue(st, "review_model", ""); v != "sonnet-x" {
		t.Errorf("config get review_model = %q, want the effective sonnet-x", v)
	}
	out, err := runCLI(t, "config", "list")
	if err != nil || !strings.Contains(out, "review_model  sonnet-x (falls back to mine_model)") {
		t.Errorf("config list must render the effective value: err=%v\n%s", err, out)
	}

	// A lens on ANOTHER runtime does not inherit the default's model names (ModelFor drops them).
	cross := &lens.Lens{Name: "x", Runner: "opencode"}
	for _, s := range effectiveConfig(st, cross)[1:] {
		if s.value != "" || !strings.Contains(s.from, "opencode") {
			t.Errorf("a cross-runtime lens rides its own runner's default, got %+v", s)
		}
	}
	// And a lens override wins, said as such.
	own := &lens.Lens{Name: "y", ExtractModel: "haiku-y"}
	if s := effectiveConfig(st, own)[1]; s.value != "haiku-y" || s.from != "lens override" {
		t.Errorf("a per-lens mine_model is the effective one, got %+v", s)
	}
}

// `config unset review_model` printed "set default · review_model = (runner default)": it read as a
// value being chosen, and named the wrong fallback.
func TestConfigUnsetSaysWhatResolutionFallsBackTo(t *testing.T) {
	readFixture(t)
	out, err := runCLI(t, "config", "unset", "review_model")
	if err != nil || !strings.Contains(out, "[ok] cleared review_model  (default scope → falls back to mine_model)") {
		t.Errorf("unset must say what it falls back to: err=%v\n%s", err, out)
	}
	out, _ = runCLI(t, "config", "set", "mine_model", "m1")
	if !strings.Contains(out, "[ok] set mine_model = m1  (default scope)") {
		t.Errorf("set result line:\n%s", out)
	}
}

// `lens disable nope` printed "disabled lens "nope"" and exited 0 for a name that was never
// registered or enabled. An orphan (enabled, not registered) must stay disable-able: that is the
// cleanup path the list's hint points at.
func TestLensDisableRejectsATypoButCleansUpAnOrphan(t *testing.T) {
	st := readFixture(t)
	if _, err := runCLI(t, "lens", "disable", "nope"); err == nil {
		t.Error("disabling a name that is neither registered nor enabled must fail")
	}
	registerRenamedLens(t, st, "ghost", "ghost")
	if err := st.EnableLens("ghost"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeregisterLens("ghost"); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "lens", "list")
	if err != nil {
		t.Fatal(err)
	}
	// default is registered + enabled; ghost is enabled in config.toml only.
	if !strings.Contains(out, "1 lens · 1 enabled · 1 orphan") || !strings.Contains(out, "ghost") ||
		!strings.Contains(out, "witness lens disable ghost") {
		t.Fatalf("lens list must count what runs and show the orphan with its fix:\n%s", out)
	}
	if _, err := runCLI(t, "lens", "disable", "ghost"); err != nil {
		t.Errorf("disabling an orphan is the cleanup path and must work: %v", err)
	}
	if out, err := runCLI(t, "lens", "disable", "ghost"); err == nil {
		t.Errorf("once cleaned up, ghost is neither registered nor enabled: want an error, got\n%s", out)
	}
}

// The other no-op and typo paths of the lens mutations: deregistering a name that was never
// registered printed "deregistered" with exit 0; re-enabling or re-disabling says nothing changed.
func TestLensMutationsSayWhenNothingChanged(t *testing.T) {
	st := readFixture(t)
	if _, err := runCLI(t, "lens", "deregister", "nope"); err == nil {
		t.Error("deregistering a name that is not registered must fail")
	}
	registerRenamedLens(t, st, "math", "math")
	for _, step := range []struct{ args, want string }{
		{"disable math", "[-] lens math is already disabled  (nothing changed)"},
		{"enable math", "[ok] enabled lens math"},
		{"enable math", "[-] lens math is already enabled  (nothing changed)"},
	} {
		out, err := runCLI(t, append([]string{"lens"}, strings.Fields(step.args)...)...)
		if err != nil || !strings.Contains(out, step.want) {
			t.Errorf("lens %s: want %q, got err=%v\n%s", step.args, step.want, err, out)
		}
	}
}

// kvRow's label column is 14 wide, so a 14-character name (the shipped corpus-example) put its
// value two columns right of every other row's — and in a pipe the orphan's "[!]" did the same.
func TestLensListColumnsLineUp(t *testing.T) {
	st := readFixture(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	registerRenamedLens(t, st, "ghost", "ghost")
	if err := st.EnableLens("ghost"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeregisterLens("ghost"); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "lens", "list")
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		for _, name := range []string{"corpus-example", "default", "ghost"} {
			if i := strings.Index(line, " "+name+" "); i >= 0 {
				rest := line[i+len(name)+1:]
				col[name] = len(line) - len(strings.TrimLeft(rest, " ")) // where the value starts
			}
		}
	}
	if len(col) != 3 || col["corpus-example"] != col["default"] || col["ghost"] != col["default"] {
		t.Errorf("every row's value must start in the same column, got %v:\n%s", col, out)
	}
}

// The empty profile said "it's generated after the next background review" in every case — on an
// archive that captures nothing, or for a lens that is not registered, that review never comes.
func TestProfileEmptyNamesTheMissingLayer(t *testing.T) {
	st := readFixture(t)
	for _, c := range []struct{ lens, want string }{
		{"nope", "witness lens list"},
		{store.ProfileUnified, "witness wire <"},
		{"default", "witness wire <"},
	} {
		out := captureStdout(t, func() { renderProfileEmpty(st, c.lens) })
		if !strings.Contains(out, c.want) || strings.Contains(out, "next background review") {
			t.Errorf("profile %s, empty: want %q\n%s", c.lens, c.want, out)
		}
	}
	if err := st.AppendRaw(store.RawRecord{Session: "s1", TS: "2026-11-01T00:00:00Z", Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DisableLens("default"); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() { renderProfileEmpty(st, "default") }); !strings.Contains(out, "witness lens enable default") {
		t.Errorf("a disabled lens's profile points at enabling it:\n%s", out)
	}
	if out := captureStdout(t, func() { renderProfileEmpty(st, store.ProfileUnified) }); !strings.Contains(out, "witness facets") {
		t.Errorf("capturing but no facets yet points at facets, which says why:\n%s", out)
	}
}

// doctor read "✓ witness doctor" on an archive that had captured nothing, the one state where a
// new user most needs it to say why every read command comes back empty.
func TestDoctorSaysWhenNothingIsCaptured(t *testing.T) {
	readFixture(t)
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no model: the embedder check fails fast
	out, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if !strings.Contains(out, "nothing captured yet") || !strings.Contains(out, "witness wire <") {
		t.Errorf("an empty archive must say nothing is captured and how to wire an editor:\n%s", out)
	}
}

// `observations search q --lens nope` suggested `witness lens backfill nope`, which then failed
// with "not registered".
func TestObservationSearchOnAnUnregisteredLensSuggestsACommandThatWorks(t *testing.T) {
	out := captureStdout(t, func() {
		renderObservationHits("nope", "q", nil, false, false, func() store.EmptyReason { return store.NoObservations })
	})
	if strings.Contains(out, "backfill") || !strings.Contains(out, "witness lens list") {
		t.Errorf("an unregistered lens must point at lens list, not a backfill that fails:\n%s", out)
	}
	// Registered but disabled: backfill requires enabled, so enabling comes first.
	out = captureStdout(t, func() {
		renderObservationHits("math", "q", nil, true, false, func() store.EmptyReason { return store.NoObservations })
	})
	if i, j := strings.Index(out, "witness lens enable math"), strings.Index(out, "witness lens backfill math"); i < 0 || j < i {
		t.Errorf("a disabled lens must be enabled before it is backfilled:\n%s", out)
	}
}

// Facets and profiles are tagged with the MINED name, so `witness facets corpus-example` found
// nothing for a lens that had facets under "regime".
func TestFacetsReadsALensByItsRegisteredName(t *testing.T) {
	st := readFixture(t)
	registerRenamedLens(t, st, "corpus-example", "regime")
	if err := st.WriteFacets([]store.Facet{{Lens: "regime", Dimension: "regimes", Key: "flips_on_rates",
		Versions: []store.FacetVersion{{Value: "turns when rates move", Confidence: 0.7}}}}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := cmdFacets([]string{"corpus-example"}, false, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "flips_on_rates") || !strings.Contains(out, "witness facets · corpus-example  1 active") {
		t.Errorf("the registered name must reach the lens's facets:\n%s", out)
	}
}

// `witness facets --lens math` died on "unknown flag" while `observations search q --lens math`
// worked (#199 F051). On facets and profile --lens is a hidden alias of the [lens] argument.
func TestFacetsAndProfileTakeAHiddenLensAlias(t *testing.T) {
	readFixture(t)
	t.Setenv("PATH", t.TempDir()) // profile's refresh must find no runner to spawn
	for _, cmd := range []string{"facets", "profile"} {
		out, err := runCLI(t, cmd, "--lens", "math", "--json")
		if err != nil || !strings.Contains(out, `"lens": "math"`) {
			t.Errorf("%s --lens math: err=%v, want the math lens\n%s", cmd, err, out)
		}
		if _, err := runCLI(t, cmd, "math", "--lens", "math", "--json"); err != nil {
			t.Errorf("%s math --lens math: one lens named twice is fine, got %v", cmd, err)
		}
		_, err = runCLI(t, cmd, "default", "--lens", "math")
		if err == nil || !strings.Contains(err.Error(), `"default"`) || !strings.Contains(err.Error(), `"math"`) {
			t.Errorf("%s default --lens math: want an error naming both lenses, got %v", cmd, err)
		}
		help, _ := runCLI(t, cmd, "--help")
		if !strings.Contains(help, cmd+" [lens]") || strings.Contains(help, "--lens") {
			t.Errorf("%s --help must show one spelling, [lens]:\n%s", cmd, help)
		}
	}
}

// `lens try` with every session failing printed "0 observation(s) across 2 session(s)" and exited 0.
func TestLensTryFailsOnlyWhenEverySessionFailed(t *testing.T) {
	boom := errors.New("claude runner disabled")
	if err := allFailed([]tryResult{{err: boom}, {err: boom}}); err == nil || !errors.Is(err, boom) {
		t.Errorf("all failed must be an error carrying the cause, got %v", err)
	}
	if err := allFailed([]tryResult{{err: boom}, {}}); err != nil {
		t.Errorf("one success among failures is a partial preview, not a failure: %v", err)
	}
	if err := allFailed(nil); err != nil {
		t.Errorf("no sessions is not a mine failure: %v", err)
	}
}

// `lens try corpus-example` titled the preview "regime" — its lens.json name — while every other
// command, and the user, calls it corpus-example. A fake runner: no CLI is spawned.
func TestLensTryTitlesARegisteredLensByItsRegisteredName(t *testing.T) {
	s := seedTryStore(t, store.RunnerOpenCode)
	opened := false
	previous := runnerForName
	runnerForName = func(string, store.Config) (platform.Runner, error) {
		return limitedPreviewRunner{opened: &opened}, nil
	}
	t.Cleanup(func() { runnerForName = previous })
	registerRenamedLens(t, s, "corpus-example", "regime")
	var err error
	out := captureStdout(t, func() { err = cmdLensTry("corpus-example", lensTryOpts{nSessions: 1}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "witness lens try · corpus-example") || strings.Contains(out, "· regime") {
		t.Errorf("the title must use the registered name:\n%s", out)
	}
}

// `lens register x /nonexistent` said only "stat /nonexistent: no such file or directory".
func TestLensRegisterNamesAMissingDirectory(t *testing.T) {
	readFixture(t)
	_, err := runCLI(t, "lens", "register", "x", "/nonexistent/dir")
	if err == nil || !strings.Contains(err.Error(), "lens directory /nonexistent/dir does not exist") ||
		!strings.Contains(err.Error(), "extract.md") {
		t.Errorf("want a message naming the directory and what belongs in it, got %v", err)
	}
}

// The first-run seed notice reaches a person at a terminal, once, in the house style. The hook
// path (no terminal) stays silent — TestHooksPrintNothingOnAFreshArchive pins that half.
func TestSeedNoticeIsShownToAPersonOnce(t *testing.T) {
	old := seedNoticeWanted
	seedNoticeWanted = func() bool { return true }
	defer func() { seedNoticeWanted = old }()
	home := filepath.Join(t.TempDir(), "fresh")
	read := func() string {
		return captureStream(t, &os.Stderr, func() { openSeedTestStore(t, home).Close() })
	}
	first := read()
	if !strings.Contains(first, `added the built-in "default" lens`) || !strings.Contains(first, "witness lens disable default") ||
		strings.Contains(first, "#44") {
		t.Errorf("first open must say what was added and how to turn it off, without internal refs:\n%q", first)
	}
	if again := read(); again != "" {
		t.Errorf("the seed is one-shot, so is its notice: %q", again)
	}
}

// doctor said "check witness.log" for backing-off sessions; the cause belongs on the screen.
func TestLastLoggedErrorFindsTheNewestCause(t *testing.T) {
	p := filepath.Join(t.TempDir(), "witness.log")
	_ = os.WriteFile(p, []byte(`{"level":"ERROR","msg":"old","err":"first"}
{"level":"WARN","msg":"backing off","lens":"math"}
{"level":"ERROR","msg":"review","err":"claude runner disabled by WITNESS_DISABLE_EXTERNAL_RUNNERS"}
{"level":"INFO","msg":"scheduled"}
`), 0o600)
	if got := lastLoggedError(p); got != "claude runner disabled by WITNESS_DISABLE_EXTERNAL_RUNNERS" {
		t.Errorf("lastLoggedError = %q", got)
	}
	if got := lastLoggedError(filepath.Join(t.TempDir(), "none")); got != "" {
		t.Errorf("no log, no cause: %q", got)
	}
	// A runner error carries the CLI's multi-line stderr: printed as-is it broke doctor's indent
	// in a pipe (column 0) and could fill the screen. One line, bounded; witness.log keeps the rest.
	_ = os.WriteFile(p, []byte(`{"level":"ERROR","err":"claude -p: exit 1:\nline two\n`+strings.Repeat("x ", 400)+`"}`+"\n"), 0o600)
	got := lastLoggedError(p)
	if strings.Contains(got, "\n") || !strings.HasPrefix(got, "claude -p: exit 1: line two x") || len([]rune(got)) > 310 {
		t.Errorf("want one bounded line, got %d runes: %q", len([]rune(got)), got)
	}
}

// Plain forms of the read helpers: a pipe or NO_COLOR gets ASCII and the text verbatim.
func TestReadHelpersPlainForms(t *testing.T) {
	if got := markdownLine("## **Bold** heading"); got != "## **Bold** heading" {
		t.Errorf("markdown must stay verbatim in a pipe: %q", got)
	}
	if got := rule(5); got != "-----" {
		t.Errorf("rule plain = %q", got)
	}
	if got := glyph("↳", "->"); got != "->" {
		t.Errorf("glyph plain = %q", got)
	}
	long := strings.Repeat("dimension, ", 40)
	if got := wrapHanging(long, 16); got != long {
		t.Error("piped output is never wrapped")
	}
	if got := result(okGlyph(), "deleted x", "note"); got != "[ok] deleted x  (note)" {
		t.Errorf("result plain = %q", got)
	}
	for n, want := range map[int64]string{950: "950", 3461: "3,461", 12_345: "12.3k", 2_828_543: "2.8M"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
