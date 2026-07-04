package store

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
	for _, want := range []string{
		"runner =",
		"triage_model",
		"distill_model",
		"review_every",
		"review_poignancy",
		"lens =",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("auto-created config missing %q:\n%s", want, body)
		}
	}
	c := st.LoadConfig()
	if c.Runner != "claude" || c.ReviewEvery != 5 || c.ReviewPoignancy != 30 {
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
	original := []byte("# my custom config from an older CLI\n" +
		"runner = \"opencode\"\n" +
		"triage_model = \"my-fine-model\"\n" +
		"review_every = 99\n" +
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
	if c.Runner != "opencode" || c.TriageModel != "my-fine-model" || c.ReviewEvery != 99 || !slices.Contains(c.EnabledLenses, "math") {
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
