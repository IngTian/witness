package store

import (
	"fmt"
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

// emerge.md must be copied into the registry, like review.md and lens.json.
//
// It is the S3 long-arc verify prompt, and the LOADER reads it (internal/lens EmergeFile, via
// loadDir/readPromptPair). RegisterLens copied only extract.md, review.md and lens.json — and
// because register stores a SNAPSHOT, a lens that shipped an emerge.md simply did not have one in
// its registered copy. Nothing failed: the reviewer silently falls back to review.md, so a lens
// author who wrote a long-arc prompt got the generic behavior with no warning. prompts/default/
// ships an emerge.md, which is how the gap surfaced.
func TestRegisterLensCopiesTheEmergePrompt(t *testing.T) {
	s := tempStore(t)
	src := writeLensSrcDir(t, "math", "mine", "rev")
	const emerge = "EMERGE PROMPT: verify this long arc"
	if err := os.WriteFile(filepath.Join(src, "emerge.md"), []byte(emerge), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.RegisterLens("math", src); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(s.LensesDir(), "math", "emerge.md"))
	if err != nil {
		t.Fatalf("emerge.md was not copied into the registry (%v) — the lens author's long-arc "+
			"prompt is silently dropped and the reviewer falls back to review.md", err)
	}
	if string(got) != emerge {
		t.Errorf("emerge.md content changed: got %q, want %q", got, emerge)
	}

	// And a source WITHOUT one must still register (it is optional), not error.
	if err := s.RegisterLens("plain", writeLensSrcDir(t, "plain", "mine", "rev")); err != nil {
		t.Fatalf("a lens with no emerge.md must still register: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.LensesDir(), "plain", "emerge.md")); !os.IsNotExist(err) {
		t.Errorf("emerge.md appeared for a source that had none (err=%v)", err)
	}
}

// SELF-REGISTER must be lossless: re-registering a lens FROM its own registry directory
// (the "edit the registered copy in place, then re-register" workflow) must not lose
// review.md/lens.json to the destination wipe. Regression for the audit finding where
// RemoveAll ran before those optional files were read.
func TestRegisterLensSelfRegisterIsLossless(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("math", writeLensSrcDir(t, "math", "mine", "rev")); err != nil {
		t.Fatal(err)
	}
	regDir := filepath.Join(s.LensesDir(), "math")
	// Set a per-lens model so lens.json carries state worth preserving.
	if err := s.SetLensModel("math", "extract", "openai/gpt-5.5-mini"); err != nil {
		t.Fatal(err)
	}
	// Re-register FROM the registry dir itself (srcDir == dest).
	if err := s.RegisterLens("math", regDir); err != nil {
		t.Fatalf("self-register: %v", err)
	}
	for _, fn := range []string{"extract.md", "review.md", "lens.json"} {
		if _, err := os.Stat(filepath.Join(regDir, fn)); err != nil {
			t.Fatalf("self-register lost %s: %v", fn, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(regDir, "lens.json"))
	if !strings.Contains(string(data), "openai/gpt-5.5-mini") {
		t.Fatalf("self-register lost the per-lens model in lens.json:\n%s", string(data))
	}
}

// RegisterLens rejects a non-slug name so the stored dir name always equals the handle
// the CLI gates (set/enable/backfill/show) look up — else a "my lens" lens would live at
// "my_lens" on disk and be unaddressable under the name the tool accepted.
func TestRegisterLensRejectsNonSlugName(t *testing.T) {
	s := tempStore(t)
	src := writeLensSrcDir(t, "", "mine", "rev") // no lens.json name; register-name is the handle
	for _, bad := range []string{"my lens", "a/b", "weird!", "trailing "} {
		if err := s.RegisterLens(bad, src); err == nil {
			t.Fatalf("register with non-slug name %q must be rejected", bad)
		}
		if slices.Contains(s.RegisteredLenses(), sanitize(bad)) {
			t.Fatalf("a rejected non-slug name %q must not have been written to the registry", bad)
		}
	}
	// A clean slug is accepted.
	if err := s.RegisterLens("my_lens-2", src); err != nil {
		t.Fatalf("a valid slug name must register: %v", err)
	}
}

// A failed swap must never leave the user with NO lens: RegisterLens moves the old
// definition aside and restores it if the rename fails. We can't easily force a rename
// failure in-process, so this asserts the recoverable-by-design property indirectly — a
// SUCCESSFUL re-register leaves no .bak/.tmp turds and the lens intact.
func TestRegisterLensLeavesNoStagingTurds(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("cr", writeLensSrcDir(t, "cr", "mine", "rev")); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterLens("cr", writeLensSrcDir(t, "cr", "mine2", "rev2")); err != nil {
		t.Fatal(err) // re-register
	}
	entries, _ := os.ReadDir(s.LensesDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ".bak") {
			t.Fatalf("re-register left a staging turd: %s", e.Name())
		}
	}
	// And the staging dirs, even if present, never appear as registered lenses.
	if got := s.RegisteredLenses(); len(got) != 1 || got[0] != "cr" {
		t.Fatalf("want exactly [cr] registered, got %v", got)
	}
}

// A failed swap restores the previous definition, and when the restore ALSO fails (Windows: a
// rename can fail while a file is open) the error says where it was left — RegisteredLenses
// hides <name>.bak, so "restored" there would send the user looking for a lens that is gone.
func TestRegisterLensSwapFailureRestoresOrSaysWhereItIs(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("cr", writeLensSrcDir(t, "cr", "OLD", "rev")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.LensesDir(), "cr")
	orig := renameDir
	t.Cleanup(func() { renameDir = orig })

	renameDir = func(from, to string) error { // only the swap-into-place fails
		if from == dir+".tmp" {
			return fmt.Errorf("injected swap failure")
		}
		return orig(from, to)
	}
	err := s.RegisterLens("cr", writeLensSrcDir(t, "cr", "NEW", "rev"))
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("want a swap error saying the previous definition was restored, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "extract.md")); string(b) != "OLD" {
		t.Fatalf("the previous definition was not put back: extract.md = %q", b)
	}

	renameDir = func(from, to string) error { // the swap AND the restore fail
		if from == dir+".tmp" || from == dir+".bak" {
			return fmt.Errorf("injected failure")
		}
		return orig(from, to)
	}
	err = s.RegisterLens("cr", writeLensSrcDir(t, "cr", "NEW", "rev"))
	if err == nil || strings.Contains(err.Error(), "restored") || !strings.Contains(err.Error(), dir+".bak") {
		t.Fatalf("a failed restore must name where the lens was left, not claim it was restored: %v", err)
	}
}

// SetLensModel round-trips per-lens models through lens.json without touching prompts,
// and an empty value clears the field (the lens rides the default again).
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

// SetLensRunner round-trips the per-lens runner through lens.json (#75 slice 2) without
// touching prompts or other fields, and an empty value clears it (ride the default runner).
func TestSetLensRunnerRoundTrip(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("cr", writeLensSrcDir(t, "cr", "mine", "rev")); err != nil {
		t.Fatal(err)
	}
	// Also set a model, so we can prove runner + model coexist and clears are independent.
	if err := s.SetLensModel("cr", "extract", "openai/free"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLensRunner("cr", "opencode"); err != nil {
		t.Fatalf("SetLensRunner: %v", err)
	}
	path := filepath.Join(s.LensesDir(), "cr", "lens.json")
	data, _ := os.ReadFile(path)
	body := string(data)
	if !strings.Contains(body, `"runner"`) || !strings.Contains(body, "opencode") {
		t.Fatalf("lens.json missing the runner:\n%s", body)
	}
	if !strings.Contains(body, "openai/free") || !strings.Contains(body, `"name"`) {
		t.Fatalf("SetLensRunner clobbered other fields:\n%s", body)
	}
	// Clearing the runner removes only it; the model stays.
	if err := s.SetLensRunner("cr", ""); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), `"runner"`) {
		t.Fatalf("cleared runner should be gone:\n%s", string(data))
	}
	if !strings.Contains(string(data), "openai/free") {
		t.Fatalf("clearing runner must not touch the model:\n%s", string(data))
	}
	// Unregistered lens errors.
	if err := s.SetLensRunner("nope", "opencode"); err == nil {
		t.Fatalf("SetLensRunner on an unregistered lens must error")
	}
}

// Since #44 slice 1a exactly ONE name is reserved and must be refused at register AND
// enable: "unified" (the cross-lens summary's filename stem — a per-lens summary under
// it would clobber the unified portrait). "default" is now an ordinary lens and must be
// ACCEPTED (register + enable). This locks the post-1a identity-layer rule.
func TestReservedLensNamesRejected(t *testing.T) {
	// Only "unified" is reserved (it is the cross-lens profile filename stem, not a
	// lens). Case variants included: on case-insensitive filesystems (macOS/Windows)
	// "Unified.md" collides with "unified.md", so the guard folds case. Since #44 slice
	// 1a "default" is NO LONGER reserved — it is an ordinary registered lens.
	for _, name := range []string{"unified", "UNIFIED", "Unified", "uNiFiEd"} {
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
	// "default" is now an ORDINARY lens: registerable + enableable, and NOT reported
	// reserved (any case variant).
	for _, name := range []string{"default", "Default", "DEFAULT"} {
		t.Run("allowed/"+name, func(t *testing.T) {
			s := tempStore(t)
			src := writeLensSrcDir(t, "default", "x", "y") // registry key is the slug "default"
			if err := s.RegisterLens("default", src); err != nil {
				t.Fatalf("registering the ordinary default lens must succeed: %v", err)
			}
			if !slices.Contains(s.RegisteredLenses(), "default") {
				t.Fatal("default lens must be written to the registry")
			}
			if err := s.EnableLens("default"); err != nil {
				t.Fatalf("enabling the default lens must succeed: %v", err)
			}
			if ReservedLensName(name) {
				t.Fatalf("ReservedLensName(%q) must be false since #44 slice 1a", name)
			}
		})
	}
	if ReservedLensName("default") || !ReservedLensName("unified") {
		t.Fatal("only 'unified' is reserved now; 'default' is an ordinary lens")
	}
	// Case-folded: only unified's case variants are reserved.
	for _, v := range []string{"Unified", "UNIFIED", "uNiFiEd"} {
		if !ReservedLensName(v) {
			t.Fatalf("ReservedLensName(%q) must be true (case-insensitive)", v)
		}
	}
	if ReservedLensName("math") {
		t.Fatal("ReservedLensName must not reserve an ordinary lens name")
	}
}

// TestRegisterRejectsCaseCollision is the regression guard for the adversarial finding
// that, once "default" became registerable, `register Default` on a case-insensitive
// filesystem (macOS APFS / Windows NTFS) would reuse and CLOBBER `default`'s registry
// dir. RegisterLens now refuses a name that case-insensitively collides with an already-
// registered lens under a different casing (protecting ANY lens, not just default),
// while still allowing an exact-case re-register (an intentional overwrite/update).
func TestRegisterCaseCollision(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("default", writeLensSrcDir(t, "default", "x", "y")); err != nil {
		t.Fatalf("register default: %v", err)
	}
	// A different-case variant must be REFUSED (would clobber default's dir on APFS/NTFS).
	for _, variant := range []string{"Default", "DEFAULT", "deFAULT"} {
		if err := s.RegisterLens(variant, writeLensSrcDir(t, variant, "a", "b")); err == nil {
			t.Fatalf("register %q must be refused (case-collides with the registered 'default')", variant)
		}
	}
	// Same lens still registered exactly once, unclobbered.
	n := 0
	for _, r := range s.RegisteredLenses() {
		if strings.EqualFold(r, "default") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("default must remain registered exactly once after refused collisions, got %d", n)
	}
	// Exact-case re-register is still allowed (intentional overwrite).
	if err := s.RegisterLens("default", writeLensSrcDir(t, "default", "x2", "y2")); err != nil {
		t.Fatalf("exact-case re-register must be allowed: %v", err)
	}
	// An unrelated name is unaffected.
	if err := s.RegisterLens("market", writeLensSrcDir(t, "market", "m", "r")); err != nil {
		t.Fatalf("register unrelated lens: %v", err)
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

// TestRegisterRejectsResolvedNameCollision is the guard for issue #101: RegisterLens
// validates the lens's RESOLVED name (lens.json `name` field when present, else registry
// dir name) against every other lens's resolved name, so two different registry dirs
// can't resolve to the same effective lens identity and share watermark/observations/profile.
func TestRegisterRejectsResolvedNameCollision(t *testing.T) {
	s := tempStore(t)

	// Register a lens "default" (no lens.json, so it resolves to "default").
	if err := s.RegisterLens("default", writeLensSrcDir(t, "", "extract1", "review1")); err != nil {
		t.Fatalf("register default: %v", err)
	}

	// Register a probe dir "probe" with lens.json name="default" → MUST be rejected
	// (resolves to "default", collides with the existing lens's resolved name).
	probeDir := writeLensSrcDir(t, "default", "extract2", "review2")
	if err := s.RegisterLens("probe", probeDir); err == nil {
		t.Fatalf("register 'probe' with lens.json name='default' must be rejected (collides with existing default lens)")
	} else if !strings.Contains(err.Error(), "collide") && !strings.Contains(err.Error(), "default") {
		t.Fatalf("error missing collision details: %v", err)
	}
	// The probe must NOT have been written to the registry.
	if slices.Contains(s.RegisteredLenses(), "probe") {
		t.Fatal("probe lens must not be registered after collision rejection")
	}

	// Register a lens "math" (no lens.json) — resolves to "math", no collision → succeeds.
	if err := s.RegisterLens("math", writeLensSrcDir(t, "", "extract-math", "review-math")); err != nil {
		t.Fatalf("register math: %v", err)
	}

	// Register a probe dir "another" with lens.json name="math" → MUST be rejected
	// (resolves to "math", collides with math lens).
	if err := s.RegisterLens("another", writeLensSrcDir(t, "math", "x", "y")); err == nil {
		t.Fatalf("register 'another' with lens.json name='math' must be rejected")
	}

	// Register a lens.json name="unified" (reserved) → MUST be rejected at register
	// (even though the dir name "probe2" is innocent, the resolved name is reserved).
	if err := s.RegisterLens("probe2", writeLensSrcDir(t, "unified", "x", "y")); err == nil {
		t.Fatalf("register with lens.json name='unified' (reserved) must be rejected")
	} else if !strings.Contains(err.Error(), "reserved") && !strings.Contains(err.Error(), "unified") {
		t.Fatalf("error missing reserved-name details: %v", err)
	}

	// A dir whose lens.json name matches its OWN dir name (no collision) → succeeds.
	if err := s.RegisterLens("market", writeLensSrcDir(t, "market", "extract-m", "review-m")); err != nil {
		t.Fatalf("register 'market' with matching lens.json name='market' must succeed: %v", err)
	}

	// Exact-case re-register (resolved name == existing) is still allowed (intentional overwrite).
	if err := s.RegisterLens("default", writeLensSrcDir(t, "default", "new-extract", "new-review")); err != nil {
		t.Fatalf("exact-case re-register of 'default' must be allowed: %v", err)
	}
}

// The #101 guard used to run only when the CANDIDATE's two names differed, so it caught
// probe-after-default but not this order: `regime` (no lens.json name) registered AFTER a
// corpus-example that mines as "regime" passed, and both lenses wrote one "regime" archive.
func TestRegisterRejectsResolvedNameCollisionInEitherOrder(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("corpus-example", writeLensSrcDir(t, "regime", "x", "y")); err != nil {
		t.Fatalf("register corpus-example: %v", err)
	}
	if err := s.RegisterLens("regime", writeLensSrcDir(t, "", "x", "y")); err == nil || !strings.Contains(err.Error(), "share") {
		t.Fatalf("regime after corpus-example (mines as regime) must be refused as shared data, got %v", err)
	}
	// A handle equal to another lens's data name is refused whatever the candidate mines as:
	// in the other order it is the "shadows the registry handle" refusal.
	if err := s.RegisterLens("regime", writeLensSrcDir(t, "other", "x", "y")); err == nil {
		t.Fatal("a lens named after another lens's data name must be refused")
	}
	if slices.Contains(s.RegisteredLenses(), "regime") {
		t.Fatal("a refused lens must not be registered")
	}
}

// F153: re-registering from a copy that drops or edits lens.json's `name` silently moved the
// lens's data key, stranding everything mined under the old one. Refuse it while the old name
// holds data, and say how to keep it; with nothing stranded, or the same data name, it succeeds.
func TestReRegisterRefusesToStrandData(t *testing.T) {
	s := tempStore(t)
	if err := s.RegisterLens("ce", writeLensSrcDir(t, "regime", "x", "y")); err != nil {
		t.Fatal(err)
	}
	// No data under "regime" yet: the move strands nothing, so it is allowed (and moved back).
	if err := s.RegisterLens("ce", writeLensSrcDir(t, "", "x", "y")); err != nil {
		t.Fatalf("re-register with no data under the old name must succeed: %v", err)
	}
	if err := s.RegisterLens("ce", writeLensSrcDir(t, "regime", "x", "y")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendObservations([]Observation{{ID: "o1", Session: "s1", Lens: "regime", Dimension: "d", Observation: "o"}}); err != nil {
		t.Fatal(err)
	}
	src := writeLensSrcDir(t, "", "new-extract", "y")
	err := s.RegisterLens("ce", src)
	if err == nil || !strings.Contains(err.Error(), `set "name": "regime" in `+filepath.Join(src, "lens.json")) {
		t.Fatalf("re-register that strands data must be refused and name the fix, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.LensesDir(), "ce", "extract.md")); string(got) != "x" {
		t.Fatalf("a refused re-register must leave the registered copy alone, extract.md = %q", got)
	}
	// Keeping the data name is the ordinary update.
	if err := s.RegisterLens("ce", writeLensSrcDir(t, "regime", "new-extract", "y")); err != nil {
		t.Fatalf("re-register that keeps the data name must succeed: %v", err)
	}
}
