package store

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnabledLensesGlobalList(t *testing.T) {
	s := tempStore(t)
	if got := s.LoadConfig().EnabledLenses; len(got) != 0 {
		t.Fatalf("fresh config: want none, got %v", got)
	}

	s.EnableLens("math")
	s.EnableLens("rust")
	s.EnableLens("math") // idempotent — no duplicate

	got := s.LoadConfig().EnabledLenses
	if len(got) != 2 || !slices.Contains(got, "math") || !slices.Contains(got, "rust") {
		t.Fatalf("after enabling math,rust (twice math): want [math rust], got %v", got)
	}

	s.DisableLens("math")
	got = s.LoadConfig().EnabledLenses
	if len(got) != 1 || slices.Contains(got, "math") || !slices.Contains(got, "rust") {
		t.Fatalf("after disabling math: want [rust], got %v", got)
	}
}

// writeLensSrcDir lays down a source lens directory (new format, issue #75): lens.json
// (only when name/models given) + extract.md + optional review.md. Returns the dir.
func writeLensSrcDir(t *testing.T, name, extract, review string) string {
	t.Helper()
	dir := t.TempDir()
	if name != "" {
		if err := os.WriteFile(filepath.Join(dir, "lens.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "extract.md"), []byte(extract), 0o644); err != nil {
		t.Fatal(err)
	}
	if review != "" {
		if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte(review), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLensRegistry(t *testing.T) {
	s := tempStore(t)
	src := writeLensSrcDir(t, "math", "mine", "rev")

	if err := s.RegisterLens("math", src); err != nil {
		t.Fatalf("RegisterLens: %v", err)
	}
	if got := s.RegisteredLenses(); !slices.Contains(got, "math") {
		t.Fatalf("want math registered, got %v", got)
	}
	// The new format copies the three files; extract.md is the presence-probe file.
	for _, fn := range []string{"lens.json", "extract.md", "review.md"} {
		if _, err := os.Stat(filepath.Join(s.LensesDir(), "math", fn)); err != nil {
			t.Fatalf("definition file %s not copied into registry: %v", fn, err)
		}
	}

	s.DeregisterLens("math")
	if got := s.RegisteredLenses(); slices.Contains(got, "math") {
		t.Fatalf("math still registered after deregister: %v", got)
	}
}

// register rejects a source that is a single FILE (the old format) rather than a
// directory, with a message pointing at the new layout.
func TestRegisterLensRejectsSingleFile(t *testing.T) {
	s := tempStore(t)
	src := filepath.Join(t.TempDir(), "old-lens.md")
	if err := os.WriteFile(src, []byte("# name: x\n## EXTRACT\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterLens("x", src); err == nil {
		t.Fatalf("registering a single file (old format) must be rejected — a lens is now a directory")
	}
}

// register rejects a directory missing extract.md (the required mining prompt).
func TestRegisterLensRequiresExtract(t *testing.T) {
	s := tempStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("rev only"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterLens("noextract", dir); err == nil {
		t.Fatalf("registering a dir with no extract.md must be rejected")
	}
}

// A re-register rebuilds the destination, so a file dropped from the source (e.g.
// review.md) does not linger in the registry.
func TestRegisterLensRebuildsDropsStaleFiles(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("math", writeLensSrcDir(t, "math", "mine", "rev")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.LensesDir(), "math", "review.md")); err != nil {
		t.Fatalf("precondition: review.md should exist after first register: %v", err)
	}
	// Re-register from a source that has NO review.md.
	if err := s.RegisterLens("math", writeLensSrcDir(t, "math", "mine2", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.LensesDir(), "math", "review.md")); !os.IsNotExist(err) {
		t.Fatalf("stale review.md should be gone after re-register from a source without it (err=%v)", err)
	}
}

// SetLensModel round-trips per-lens models through lens.json without touching prompts,
// and an empty value clears the field (the lens rides the global again).
func TestSetLensModelRoundTrip(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("math", writeLensSrcDir(t, "math", "mine", "rev")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLensModel("math", "extract", "openai/gpt-5.5-mini"); err != nil {
		t.Fatalf("SetLensModel extract: %v", err)
	}
	if err := s.SetLensModel("math", "review", "anthropic/claude-opus"); err != nil {
		t.Fatalf("SetLensModel review: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.LensesDir(), "math", "lens.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "openai/gpt-5.5-mini") || !strings.Contains(body, "anthropic/claude-opus") {
		t.Fatalf("lens.json missing set models:\n%s", body)
	}
	// The lens.json name written at register time must survive the model writes.
	if !strings.Contains(body, `"name"`) {
		t.Fatalf("SetLensModel clobbered other lens.json fields:\n%s", body)
	}
	// Clearing extract removes it; review stays.
	if err := s.SetLensModel("math", "extract", ""); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(s.LensesDir(), "math", "lens.json"))
	if strings.Contains(string(data), "openai/gpt-5.5-mini") {
		t.Fatalf("cleared extract_model should be gone:\n%s", string(data))
	}
	if !strings.Contains(string(data), "anthropic/claude-opus") {
		t.Fatalf("clearing extract must not touch review_model:\n%s", string(data))
	}
	// Setting on an unregistered lens errors.
	if err := s.SetLensModel("nope", "extract", "m"); err == nil {
		t.Fatalf("SetLensModel on an unregistered lens must error")
	}
}

// Both reserved names must be refused at register AND enable: "unified" (the
// cross-lens summary's filename stem — its per-lens summary would clobber the
// unified portrait) and "default" (the always-on built-in's identity — a second
// lens under this name would share the built-in's (session,'default') watermark +
// observation key and corrupt the backbone lens). This is the identity-layer guard
// that keeps default's NAME protected even though the engine treats every lens's
// BEHAVIOR identically.
func TestReservedLensNamesRejected(t *testing.T) {
	// Include case variants: on case-insensitive filesystems (macOS/Windows) a
	// "Default" lens's profile file (Default.md) collides with the built-in's
	// (default.md), so the guard must fold case and reject these too — else the
	// registered lens's summary silently clobbers the built-in's.
	for _, name := range []string{"unified", "default", "Default", "UNIFIED", "Unified"} {
		t.Run(name, func(t *testing.T) {
			s := tempStore(t)
			src := writeLensSrcDir(t, name, "x", "y")
			if err := s.RegisterLens(name, src); err == nil {
				t.Fatalf("registering a lens named %q must be rejected", name)
			}
			if slices.Contains(s.RegisteredLenses(), name) {
				t.Fatalf("reserved lens %q must not be written to the registry", name)
			}
			if err := s.EnableLens(name); err == nil {
				t.Fatalf("enabling a reserved lens %q must be rejected", name)
			}
			if slices.Contains(s.LoadConfig().EnabledLenses, name) {
				t.Fatalf("reserved lens %q must not enter the enabled set", name)
			}
		})
	}
	if !ReservedLensName("default") || !ReservedLensName("unified") {
		t.Fatal("ReservedLensName must report both reserved names")
	}
	// Case-folded: mixed/upper-case variants of the reserved names are also reserved.
	for _, v := range []string{"Default", "DEFAULT", "Unified", "UNIFIED", "uNiFiEd"} {
		if !ReservedLensName(v) {
			t.Fatalf("ReservedLensName(%q) must be true (case-insensitive)", v)
		}
	}
	if ReservedLensName("math") {
		t.Fatal("ReservedLensName must not reserve an ordinary lens name")
	}
}

func TestEnableLensPreservesOtherConfig(t *testing.T) {
	s := tempStore(t)
	if err := os.WriteFile(s.ConfigPath(), []byte("# hi\nreview_every = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.EnableLens("math")
	c := s.LoadConfig()
	if c.ReviewEvery != 3 {
		t.Errorf("review_every clobbered: got %d", c.ReviewEvery)
	}
	if !slices.Contains(c.EnabledLenses, "math") {
		t.Errorf("math not enabled: %v", c.EnabledLenses)
	}
	data, _ := os.ReadFile(s.ConfigPath())
	if !filepath.IsAbs(s.ConfigPath()) || len(data) == 0 {
		t.Errorf("config not written")
	}
}
