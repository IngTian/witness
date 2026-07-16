package store

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// RecordDrift atomically accumulates the prose_drift counter and overwrites the
// last-* stamps (#57). Absent → 0; each RecordDrift adds n; the last stamps reflect
// the most recent call. Guards the meta-based surfacing that doctor + backfill read.
func TestRecordDriftAccumulates(t *testing.T) {
	s := tempStore(t)
	if got := s.DriftTotal(); got != 0 {
		t.Fatalf("absent drift counter must read 0, got %d", got)
	}
	if ts, lens := s.DriftLast(); ts != "" || lens != "" {
		t.Fatalf("absent last-stamp must be empty, got ts=%q lens=%q", ts, lens)
	}
	if err := s.RecordDrift(2, "s1", "codereview"); err != nil {
		t.Fatalf("RecordDrift: %v", err)
	}
	if got := s.DriftTotal(); got != 2 {
		t.Fatalf("after +2: want 2, got %d", got)
	}
	if err := s.RecordDrift(3, "s2", "math"); err != nil {
		t.Fatalf("RecordDrift: %v", err)
	}
	if got := s.DriftTotal(); got != 5 {
		t.Fatalf("counter must accumulate: want 5, got %d", got)
	}
	ts, lens := s.DriftLast()
	if ts == "" || lens != "math" {
		t.Fatalf("last-stamp must reflect the most recent drift: ts=%q lens=%q", ts, lens)
	}
	// n<=0 is a no-op (never negatively adjusts or clobbers stamps).
	if err := s.RecordDrift(0, "s3", "ignored"); err != nil {
		t.Fatalf("RecordDrift(0): %v", err)
	}
	if got := s.DriftTotal(); got != 5 {
		t.Fatalf("RecordDrift(0) must be a no-op, got %d", got)
	}
	if _, lens := s.DriftLast(); lens != "math" {
		t.Fatalf("RecordDrift(0) must not overwrite the last-stamp, got %q", lens)
	}
}

// TestOpenCreatesFullConfigTemplate: a user who never had a config (e.g. ran an
// older CLI that didn't write one) gets a fully-commented template on the first
// command they run, because Open() ensures it. Every tunable must be visible so
// the user knows what to edit — this is the whole point of auto-init.
func TestOpenCreatesFullConfigTemplate(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	data, err := os.ReadFile(st.ConfigPath())
	if err != nil {
		t.Fatalf("Open did not create config.toml: %v", err)
	}
	body := string(data)
	// The fresh template must leave the runner UNBOUND: the runner line stays commented
	// (so WITNESS_RUNNER can still override for the npm OpenCode user) and the DB flag is
	// unset. Post-#71 there is no marker comment; bound-ness lives solely in the flag.
	if st.MetaString("runner_bound") == "1" {
		t.Fatalf("fresh template must leave runner unbound (runner_bound must be unset)")
	}
	if !strings.Contains(body, `# runner = "claude"`) {
		t.Fatalf("template should keep the runner line COMMENTED (unbound):\n%s", body)
	}
	for _, want := range []string{
		"runner =",
		"triage_model",
		"distill_model",
		"review_every",
		"review_poignancy",
		"auto_distill",
		"mine_concurrency",
		"lens =",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("auto-created config missing %q:\n%s", want, body)
		}
	}
	c := st.LoadConfig()
	if c.Runner != "claude" || c.ReviewEvery != 5 || c.ReviewPoignancy != 30 || !c.AutoDistill || c.MineConcurrency != DefaultMineConcurrency {
		t.Errorf("template defaults not loadable: %+v", c)
	}
}

// TestOpenPreservesExistingConfig is the forward-compatibility guarantee: a user
// who already has a hand-edited config.toml (from an older CLI, or their own
// edits) must not have it overwritten, reformatted, or appended to by any command
// that calls Open(). Byte-for-byte preservation — the strongest guarantee we can
// give so upgrades never surprise the user.
func TestOpenPreservesExistingConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Includes the retired auto_distill_interval_minutes / auto_distill_session_budget
	// lines (pre-#22): Open must still preserve them byte-for-byte, and LoadConfig
	// must ignore them gracefully (unknown keys) rather than error.
	original := []byte("# my custom config from an older CLI\n" +
		"runner = \"opencode\"\n" +
		"triage_model = \"my-fine-model\"\n" +
		"review_every = 99\n" +
		"auto_distill = false\n" +
		"auto_distill_interval_minutes = 120\n" +
		"auto_distill_session_budget = 2\n" +
		"lens = math\n")
	if err := os.WriteFile(filepath.Join(root, "config.toml"), original, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	got, err := os.ReadFile(st.ConfigPath())
	if err != nil {
		t.Fatalf("config vanished after Open: %v", err)
	}
	if !bytes.Equal(original, got) {
		t.Errorf("Open() modified an existing config (forward-compatibility broken):\n got %q\nwant %q", got, original)
	}
	c := st.LoadConfig()
	if c.Runner != "opencode" || c.TriageModel != "my-fine-model" || c.ReviewEvery != 99 || c.AutoDistill || !slices.Contains(c.EnabledLenses, "math") {
		t.Errorf("existing values not loaded intact: %+v", c)
	}
}

// TestOpenIsIdempotentOnTemplate: running multiple commands (each calling Open)
// must not duplicate, reorder, or rewrite the template once it exists. A user
// running `witness doctor` then `witness profile` should see no churn.
func TestOpenIsIdempotentOnTemplate(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, _ := Open()
	first, _ := os.ReadFile(st.ConfigPath())
	st.Close()

	st2, _ := Open()
	second, _ := os.ReadFile(st2.ConfigPath())
	st2.Close()

	if !bytes.Equal(first, second) {
		t.Errorf("repeated Open() changed config.toml:\n first %q\nsecond %q", first, second)
	}
}

// TestReviewEveryClampsNonPositive is the issue #49 I1 regression: review_every <= 0
// must clamp to the default, not be accepted verbatim. A verbatim 0 makes ReviewDue
// (SessionsSinceReview() >= ReviewEvery) always true, firing the reviewer on every
// session-end. Mirrors the mine_concurrency clamp.
func TestReviewEveryClampsNonPositive(t *testing.T) {
	for _, val := range []string{"0", "-1"} {
		t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
		st, err := Open()
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := os.WriteFile(st.ConfigPath(), []byte("review_every = "+val+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := st.LoadConfig()
		if c.ReviewEvery != DefaultConfig().ReviewEvery {
			t.Errorf("review_every=%s should clamp to default %d, got %d", val, DefaultConfig().ReviewEvery, c.ReviewEvery)
		}
		// And ReviewDue must NOT be trivially/always true on a fresh archive.
		if c.ReviewEvery <= 0 {
			t.Errorf("review_every=%s left a non-positive value %d", val, c.ReviewEvery)
		}
		st.Close()
	}

	// A positive value is still honored verbatim.
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if err := os.WriteFile(st.ConfigPath(), []byte("review_every = 12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := st.LoadConfig(); c.ReviewEvery != 12 {
		t.Errorf("positive review_every should be honored, got %d", c.ReviewEvery)
	}
}

// --- SetRunner: install wires runner to match the integration. ----------------
// tempStore's Open already ensures a template; SetRunner tests overwrite it with
// a controlled fixture to test the read-modify-write in isolation.

func TestSetRunnerReplacesExistingLine(t *testing.T) {
	s := tempStore(t)
	if err := os.WriteFile(s.ConfigPath(), []byte("runner = \"claude\"\nreview_every = 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunner("opencode"); err != nil {
		t.Fatalf("SetRunner: %v", err)
	}
	c := s.LoadConfig()
	if c.Runner != "opencode" {
		t.Errorf("runner not updated: got %q", c.Runner)
	}
	if c.ReviewEvery != 7 {
		t.Errorf("review_every clobbered: got %d", c.ReviewEvery)
	}
	body, _ := os.ReadFile(s.ConfigPath())
	if !strings.Contains(string(body), "review_every = 7") {
		t.Errorf("other fields lost:\n%s", body)
	}
	if s.MetaString("runner_bound") != "1" {
		t.Error("SetRunner must mark the runner bound (runner_bound=1)")
	}
}

func TestSetRunnerAppendsWhenAbsent(t *testing.T) {
	s := tempStore(t)
	if err := os.WriteFile(s.ConfigPath(), []byte("# just a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunner("claude"); err != nil {
		t.Fatalf("SetRunner: %v", err)
	}
	c := s.LoadConfig()
	if c.Runner != "claude" {
		t.Errorf("runner not set: got %q", c.Runner)
	}
}

// TestInstallSequenceMatchesBindRunner mirrors cmdInstall's flow: Open (which now
// ensures the template) followed by SetRunner. The other template fields survive.
func TestInstallSequenceMatchesBindRunner(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := Open() // ensures template
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunner("opencode"); err != nil {
		t.Fatal(err)
	}
	c := st.LoadConfig()
	if c.Runner != "opencode" {
		t.Errorf("runner = %q, want opencode", c.Runner)
	}
	if c.ReviewEvery != 5 || c.ReviewPoignancy != 30 {
		t.Errorf("template defaults lost after SetRunner: every=%d poignancy=%d", c.ReviewEvery, c.ReviewPoignancy)
	}
	body, _ := os.ReadFile(st.ConfigPath())
	for _, want := range []string{`runner = "opencode"`, "triage_model", "review_every = 5"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config missing %q:\n%s", want, body)
		}
	}
	if st.MetaString("runner_bound") != "1" {
		t.Error("install-sequence SetRunner must mark the runner bound (runner_bound=1)")
	}
}

// --- ResolveRunner: the WITNESS_RUNNER env fallback for the npm OpenCode user,
// which must never override an explicit `install` (SetRunner) choice. ----------

// The npm user never ran install (no runner_bound), config carries the default
// "claude", and the plugin passes WITNESS_RUNNER=opencode → OpenCode wins.
func TestResolveRunnerEnvFallbackWhenUnbound(t *testing.T) {
	s := tempStore(t)
	t.Setenv("WITNESS_RUNNER", "opencode")
	cfg := s.LoadConfig() // default runner = "claude"
	if got := s.ResolveRunner(cfg); got != "opencode" {
		t.Errorf("unbound + WITNESS_RUNNER=opencode: got %q, want opencode", got)
	}
}

// A user who explicitly ran `witness install claude` (SetRunner stamps
// runner_bound) must NOT be hijacked by a stray WITNESS_RUNNER — the persisted
// choice wins. This is the dual CC+OpenCode safety property.
func TestResolveRunnerBoundBeatsEnv(t *testing.T) {
	s := tempStore(t)
	if err := s.SetRunner("claude"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	cfg := s.LoadConfig()
	if got := s.ResolveRunner(cfg); got != "claude" {
		t.Errorf("bound=claude must beat WITNESS_RUNNER=opencode: got %q", got)
	}
}

// With neither a bound choice nor the env, ResolveRunner returns the config value.
func TestResolveRunnerDefaultsToConfig(t *testing.T) {
	s := tempStore(t)
	os.Unsetenv("WITNESS_RUNNER")
	cfg := s.LoadConfig()
	if got := s.ResolveRunner(cfg); got != "claude" {
		t.Errorf("no bind, no env: got %q, want claude (config default)", got)
	}
}

// openWithConfig lays down a config.toml under a fresh WITNESS_HOME, then Open()s —
// so the config exists BEFORE Open (as it does in production), and Open's one-time
// adoptRunnerBound runs against it. This is the setup a legacy on-disk config has.
func openWithConfig(t *testing.T, body string) *Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Migration (issue #71): a legacy config that bound its runner via an ACTIVE `runner =`
// line — back when resolution scanned config text — has no runner_bound flag. Open's
// adoptRunnerBound must stamp it once, so the now-text-free ResolveRunner keeps honoring
// that choice and does NOT drop the user to the WITNESS_RUNNER fallback.
func TestOpenAdoptsLegacyActiveRunnerLineAsBound(t *testing.T) {
	s := openWithConfig(t, "runner = \"claude\"\nreview_every = 7\n")
	if s.MetaString("runner_bound") != "1" {
		t.Fatalf("Open must adopt a legacy active runner line as bound (runner_bound=1)")
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s.ResolveRunner(s.LoadConfig()); got != "claude" {
		t.Fatalf("adopted legacy config should stay bound to its runner, not the env: got %q", got)
	}
}

// The inverse: the fresh template's runner line is COMMENTED, so Open must NOT adopt it —
// the npm OpenCode-plugin user stays unbound and env-resolvable.
func TestOpenDoesNotAdoptCommentedRunnerLine(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	s, err := Open() // writes the template (runner commented)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.MetaString("runner_bound") == "1" {
		t.Fatalf("a commented `# runner =` line must NOT be adopted as bound")
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s.ResolveRunner(s.LoadConfig()); got != "opencode" {
		t.Fatalf("unbound template user should resolve via WITNESS_RUNNER: got %q", got)
	}
}

// Adoption tolerates a hand-edited legacy config whose active runner line is indented
// or padded (valid TOML). isConfigKeyLine TrimSpaces before classifying, so it adopts.
func TestOpenAdoptsIndentedRunnerLine(t *testing.T) {
	s := openWithConfig(t, "  runner = \"opencode\"  \nreview_every = 5\n")
	if s.MetaString("runner_bound") != "1" {
		t.Fatalf("an indented/padded active runner line must still be adopted as bound")
	}
	t.Setenv("WITNESS_RUNNER", "claude")
	if got := s.ResolveRunner(s.LoadConfig()); got != "opencode" {
		t.Fatalf("adopted indented runner should stay bound (opencode), not env: got %q", got)
	}
}

// Adoption is idempotent AND does not re-derive across re-opens: once bound, a second
// Open is a clean no-op (exercises the short-circuit), and — critically — a config
// whose active runner line is later REMOVED stays bound (the flag is now authoritative,
// not re-derived from text). Guards the "one fact, one place" property across restarts.
func TestOpenAdoptionIdempotentAndStable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfg, []byte("runner = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s1, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if s1.MetaString("runner_bound") != "1" {
		t.Fatalf("first Open should adopt the active runner line")
	}
	_ = s1.Close()

	// Now REMOVE the active runner line entirely and re-open. A text-scanning resolver
	// would drop to the env fallback; the flag-authoritative design must stay bound.
	if err := os.WriteFile(cfg, []byte("review_every = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.MetaString("runner_bound") != "1" {
		t.Fatalf("bound-ness must persist across re-open even after the runner line is removed")
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s2.ResolveRunner(s2.LoadConfig()); got != "claude" {
		t.Fatalf("still-bound config must beat the env fallback: got %q", got)
	}
}

// Class property (issue #71): once resolution is flag-only, an enabled-lens config
// write (rewriteEnabledLens, the OTHER config mutator besides setConfigKey) cannot
// disturb runner resolution — the npm unbound user stays env-resolvable.
func TestEnableLensDoesNotDisturbRunnerResolution(t *testing.T) {
	s := tempStore(t) // fresh template: unbound
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s.ResolveRunner(s.LoadConfig()); got != "opencode" {
		t.Fatalf("precondition: unbound user resolves via env, got %q", got)
	}
	if err := s.EnableLens("math"); err != nil { // rewrites config.toml
		t.Fatalf("EnableLens: %v", err)
	}
	if s.MetaString("runner_bound") == "1" {
		t.Fatalf("enabling a lens must not bind the runner")
	}
	if got := s.ResolveRunner(s.LoadConfig()); got != "opencode" {
		t.Fatalf("enabling a lens disturbed runner resolution: got %q, want opencode", got)
	}
}

// SetRunner must stamp runner_bound so the very next ResolveRunner honors it.
func TestSetRunnerStampsBound(t *testing.T) {
	s := tempStore(t)
	if s.MetaString("runner_bound") == "1" {
		t.Fatal("runner_bound should be unset before install")
	}
	if err := s.SetRunner("opencode"); err != nil {
		t.Fatal(err)
	}
	if s.MetaString("runner_bound") != "1" {
		t.Error("SetRunner did not stamp runner_bound")
	}
}

// TestEnabledLensesSurviveSetRunner: a user who already enabled lenses must not
// lose them when install refreshes runner.
func TestEnabledLensesSurviveSetRunner(t *testing.T) {
	s := tempStore(t)
	if err := s.EnableLens("math"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunner("opencode"); err != nil {
		t.Fatal(err)
	}
	c := s.LoadConfig()
	if c.Runner != "opencode" {
		t.Errorf("runner = %q", c.Runner)
	}
	if !slices.Contains(c.EnabledLenses, "math") {
		t.Errorf("math lens lost after SetRunner: %v", c.EnabledLenses)
	}
}
