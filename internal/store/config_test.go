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
	// The runner line ships COMMENTED so a fresh (never-installed) config stays
	// unbound → the npm plugin's WITNESS_RUNNER fallback applies (issue #71). No
	// separate marker comment exists anymore; the commented runner line IS the
	// unbound signal, and the absence of the runner_bound flag is what resolution reads.
	if !strings.Contains(body, `# runner = "claude"`) {
		t.Fatalf("auto-created config should ship the runner line commented (unbound):\n%s", body)
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
	// SetRunner binds the runner (issue #71: the flag is the sole authority).
	if s.MetaString(runnerBoundKey) != "1" {
		t.Error("SetRunner did not bind the runner")
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
	// The install sequence binds the runner (issue #71: flag is the sole authority).
	if st.MetaString(runnerBoundKey) != "1" {
		t.Error("install sequence (SetRunner) did not bind the runner")
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

// A legacy install wrote an active `runner = "claude"` line but predates the
// runner_bound flag. adoptRunnerBound (at Open) must fold that active line into the
// flag, so the config choice still beats a stray WITNESS_RUNNER. Exercises the real
// Open path — adoption is what makes a config-choice authoritative now (issue #71).
func TestResolveRunnerLegacyActiveLineIsAdoptedAsBound(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("runner = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open() // adoptRunnerBound folds the active runner line into the flag
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.MetaString(runnerBoundKey) != "1" {
		t.Fatal("Open should adopt an active runner line as bound")
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s.ResolveRunner(s.LoadConfig()); got != "claude" {
		t.Fatalf("an adopted active-line config should beat the env fallback: got %q", got)
	}
}

// A user who manually uncommented the template's runner line (an active line) is
// adopted as bound at Open, so it beats the npm WITNESS_RUNNER fallback.
func TestResolveRunnerHonorsManualRunnerInNewTemplate(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", home)
	if _, err := Open(); err != nil { // lay down the template (runner commented)
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("runner = \"claude\"\n")...) // user uncomments/adds active line
	if err := os.WriteFile(filepath.Join(home, "config.toml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open() // a later Open adopts the now-active line
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s.ResolveRunner(s.LoadConfig()); got != "claude" {
		t.Fatalf("manual runner should beat the npm fallback: got %q", got)
	}
}

// The npm OpenCode-plugin case (issue #71): a fresh template ships the runner line
// COMMENTED and never runs install. adoptRunnerBound must NOT adopt a commented line,
// so the flag stays unset across Open and the WITNESS_RUNNER fallback keeps working.
// This is the safety property the whole fix protects.
func TestAdoptRunnerBoundDoesNotAdoptCommentedTemplate(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", home)
	s, err := Open() // lays down the template (runner commented) + runs adoption
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.MetaString(runnerBoundKey) == "1" {
		t.Fatal("a commented template runner line must NOT be adopted as bound")
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s.ResolveRunner(s.LoadConfig()); got != "opencode" {
		t.Fatalf("unbound template must resolve via the env fallback, got %q", got)
	}
}

// Once bound (via install or adoption), the flag is AUTHORITATIVE: deleting the
// active runner line from config does not un-bind on a later Open. Proves ResolveRunner
// reads the flag, not config text — the single-source-of-truth guarantee (issue #71).
func TestRunnerBoundFlagIsAuthoritativeOverConfigText(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	t.Setenv("WITNESS_HOME", home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("runner = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s1, err := Open() // adopts the active line → flag = 1
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	// Delete the runner line entirely; the flag must remain authoritative.
	if err := os.WriteFile(cfgPath, []byte("triage_model = \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.MetaString(runnerBoundKey) != "1" {
		t.Fatal("bound flag must survive removal of the config runner line")
	}
	t.Setenv("WITNESS_RUNNER", "opencode")
	if got := s2.ResolveRunner(s2.LoadConfig()); got != "claude" {
		t.Fatalf("a bound store must ignore the env fallback even with no runner line, got %q", got)
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
