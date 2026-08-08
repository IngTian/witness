package lens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bundleWithSummarizePrompts stages a fake bundle (prompts/summarize/{lens,unified}.md) and
// points the loader at it via WITNESS_PROMPTS, returning the archive root to pass to
// LoadSummarizePrompts.
func bundleWithSummarizePrompts(t *testing.T, lensBody, unifiedBody string) string {
	t.Helper()
	base := t.TempDir()
	bundleDir := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "summarize"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bundleDir, "summarize", "lens.md"), lensBody)
	write(t, filepath.Join(bundleDir, "summarize", "unified.md"), unifiedBody)
	t.Setenv("WITNESS_PROMPTS", bundleDir)
	return filepath.Join(base, "archive")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With no archive override, the bundled prompts are used verbatim.
//
// This is the "behaves exactly as before" half of #100: an archive that never opts in must be
// byte-identical to the pre-override behavior, because the summary signature hashes the prompt
// text — any drift here would silently force every existing archive to regenerate its profile.
func TestSummarizePromptsFallBackToTheBundle(t *testing.T) {
	root := bundleWithSummarizePrompts(t, "BUNDLED LENS", "BUNDLED UNIFIED")

	lp, up, err := LoadSummarizePrompts(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if lp != "BUNDLED LENS" {
		t.Errorf("lens prompt = %q, want the bundled one", lp)
	}
	if up != "BUNDLED UNIFIED" {
		t.Errorf("unified prompt = %q, want the bundled one", up)
	}
}

// An archive-local prompt WINS over the bundled one.
//
// Asserts the exact returned bytes, not merely that something changed: this is the whole
// feature, and a precedence inversion (bundle winning over the override) would still return a
// non-empty prompt and pass a weaker assertion.
func TestArchiveOverrideWinsOverTheBundle(t *testing.T) {
	root := bundleWithSummarizePrompts(t, "BUNDLED LENS", "BUNDLED UNIFIED")
	write(t, filepath.Join(root, SummarizeOverrideDir, "unified.md"), "MY REGIME PORTRAIT")

	lp, up, err := LoadSummarizePrompts(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if up != "MY REGIME PORTRAIT" {
		t.Errorf("unified prompt = %q, want the archive override; the override is the entire "+
			"point of #100 — a bundled prompt winning here means the user's edit is ignored", up)
	}
	// Overriding one prompt must not disturb the other.
	if lp != "BUNDLED LENS" {
		t.Errorf("lens prompt = %q; overriding unified.md must not affect lens.md", lp)
	}
}

// Each prompt overrides INDEPENDENTLY: lens.md alone works too.
func TestEitherPromptCanBeOverriddenAlone(t *testing.T) {
	root := bundleWithSummarizePrompts(t, "BUNDLED LENS", "BUNDLED UNIFIED")
	write(t, filepath.Join(root, SummarizeOverrideDir, "lens.md"), "MY PER-LENS PROMPT")

	lp, up, err := LoadSummarizePrompts(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if lp != "MY PER-LENS PROMPT" {
		t.Errorf("lens prompt = %q, want the archive override", lp)
	}
	if up != "BUNDLED UNIFIED" {
		t.Errorf("unified prompt = %q, want the bundled one", up)
	}
}

// An EMPTY (or whitespace-only) override is ignored, not honored.
//
// An empty prompt would send the corpus to the model with no instruction and write the
// resulting junk over the user's profile. A truncated or accidentally-cleared file must
// degrade to the shipped prompt instead of silently poisoning the output.
func TestEmptyOverrideIsIgnoredRatherThanHonored(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\n\t\n"} {
		root := bundleWithSummarizePrompts(t, "BUNDLED LENS", "BUNDLED UNIFIED")
		write(t, filepath.Join(root, SummarizeOverrideDir, "unified.md"), body)

		_, up, err := LoadSummarizePrompts(root)
		if err != nil {
			t.Fatalf("load with override %q: %v", body, err)
		}
		if up != "BUNDLED UNIFIED" {
			t.Errorf("override %q was honored (got %q); an empty prompt must fall back to the "+
				"bundle rather than instruct the model with nothing", body, up)
		}
	}
}

// An empty root disables override lookup entirely (bundle only).
//
// Keeps the loader usable from a context with no store open, and pins that "" is not treated
// as a relative path — which would read ./summarize/unified.md out of the process CWD, i.e.
// whatever repo the user happened to be sitting in.
func TestEmptyRootSkipsOverrideLookup(t *testing.T) {
	root := bundleWithSummarizePrompts(t, "BUNDLED LENS", "BUNDLED UNIFIED")
	// Stage a CWD-relative override that must NOT be picked up. Written BEFORE the chdir, so
	// the directory exists (t.Chdir fails on a missing path).
	write(t, filepath.Join(root, SummarizeOverrideDir, "unified.md"), "CWD OVERRIDE")
	t.Chdir(root)

	_, up, err := LoadSummarizePrompts("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if up != "BUNDLED UNIFIED" {
		t.Errorf("unified prompt = %q; an empty root must not resolve overrides relative to the "+
			"process CWD", up)
	}
}

// A real read error (not "absent") is reported, not swallowed as a missing override.
//
// Hiding an EACCES would mask a permissions mistake as "no override configured", so the user's
// prompt would be silently ignored with a green result.
func TestUnreadableOverrideIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	root := bundleWithSummarizePrompts(t, "BUNDLED LENS", "BUNDLED UNIFIED")
	p := filepath.Join(root, SummarizeOverrideDir, "unified.md")
	write(t, p, "MY PROMPT")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	if _, _, err := LoadSummarizePrompts(root); err == nil {
		t.Error("an unreadable override must return an error; swallowing it reports 'no override' " +
			"and silently ignores the user's prompt")
	} else if !strings.Contains(err.Error(), "override") {
		t.Errorf("error %q should name the override so the cause is actionable", err)
	}
}
