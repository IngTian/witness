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

// "unified" is the cross-lens profile summary's filename stem; a lens must not be
// allowed to take it, or its per-lens summary would clobber the unified portrait.
func TestRegisterLensRejectsReservedName(t *testing.T) {
	s := tempStore(t)
	src := filepath.Join(t.TempDir(), "src.md")
	if err := os.WriteFile(src, []byte("# name: unified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterLens("unified", src); err == nil {
		t.Fatal("registering a lens named 'unified' must be rejected")
	}
	if slices.Contains(s.RegisteredLenses(), "unified") {
		t.Fatal("reserved lens must not be written to the registry")
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
