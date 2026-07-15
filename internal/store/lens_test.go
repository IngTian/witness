package store

import (
	"os"
	"path/filepath"
	"slices"
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

func TestLensRegistry(t *testing.T) {
	s := tempStore(t)
	src := filepath.Join(t.TempDir(), "src.md")
	if err := os.WriteFile(src, []byte("# name: math\n## EXTRACT\nmine\n## REVIEW\nrev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.RegisterLens("math", src); err != nil {
		t.Fatalf("RegisterLens: %v", err)
	}
	if got := s.RegisteredLenses(); !slices.Contains(got, "math") {
		t.Fatalf("want math registered, got %v", got)
	}
	if _, err := os.Stat(filepath.Join(s.LensesDir(), "math", "lens.md")); err != nil {
		t.Fatalf("definition not copied into registry: %v", err)
	}

	s.DeregisterLens("math")
	if got := s.RegisteredLenses(); slices.Contains(got, "math") {
		t.Fatalf("math still registered after deregister: %v", got)
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
			src := filepath.Join(t.TempDir(), "src.md")
			if err := os.WriteFile(src, []byte("# name: "+name+"\n## EXTRACT\nx\n## REVIEW\ny\n"), 0o644); err != nil {
				t.Fatal(err)
			}
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
