package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fake CLI refuses the user's CODEX_API_KEY ("dummy" from setUpChild) and an inherited
// proxy, so every passing case also proves the check runs on its own key and loopback.
func selfCheckChild(t *testing.T, mode string) error {
	t.Helper()
	setUpChild(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("WITNESS_CODEX_HELPER_MODE", mode)
	return selfCheck(context.Background(), "codex", []byte(helperCatalog), "test")
}

func TestSelfCheckPassesAToolFreeCLI(t *testing.T) {
	if err := selfCheckChild(t, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSelfCheckRefusesOfferedTools(t *testing.T) {
	if err := selfCheckChild(t, "leak-tools"); err == nil || !strings.Contains(err.Error(), "tools offered") {
		t.Fatalf("want a tools-offered refusal, got %v", err)
	}
}

func TestSelfCheckRefusesWhenItCannotSeeTools(t *testing.T) {
	// drift: the uncatalogued request fails the contract for another reason (its
	// instructions text), which must not count as having seen tools.
	for _, mode := range []string{"blind", "drift"} {
		t.Run(mode, func(t *testing.T) {
			if err := selfCheckChild(t, mode); err == nil || !strings.Contains(err.Error(), "cannot see") {
				t.Fatalf("want a cannot-see refusal, got %v", err)
			}
		})
	}
}

func TestSelfCheckSeesToolsOfferedAsAnInputItem(t *testing.T) {
	if err := selfCheckChild(t, "lite"); err != nil {
		t.Fatal(err)
	}
}

// countChecks counts self-check calls (the only argv that points codex at a provider
// URL) and fails the test if a real generation call starts without one when guarded.
func countChecks(t *testing.T, guardExec bool) *int {
	t.Helper()
	n := new(int)
	previous := commandContext
	commandContext = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "openai_base_url=") }) {
			*n++
		} else if guardExec && slices.Contains(args, "exec") {
			t.Fatalf("a generation call started after a failed check: %v", args)
		}
		return previous(ctx, bin, args...)
	}
	t.Cleanup(func() { commandContext = previous })
	return n
}

func TestUnpinnedVersionIsCheckedOnceThenRemembered(t *testing.T) {
	for _, seed := range []string{"not json", "null"} { // a corrupt record is re-checked
		t.Run(seed, func(t *testing.T) {
			r := setUpChild(t)
			t.Setenv("WITNESS_CODEX_HELPER_VERSION", "0.172.0")
			path := filepath.Join(r.root, "codex", "verified.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
				t.Fatal(err)
			}
			checks := countChecks(t, false)
			ctx := context.Background()
			if err := r.Open(ctx); err != nil {
				t.Fatal(err)
			}
			if *checks != 2 {
				t.Fatalf("first use ran %d check calls, want 2 (tool-free + sensitivity)", *checks)
			}
			if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"0.172.0 `) || !strings.Contains(string(b), `:["test"]`) {
				t.Fatalf("verified record = %s", b)
			}
			if err := r.Open(ctx); err != nil {
				t.Fatal(err)
			}
			if got, err := r.Run(ctx, "test", "witness-system", "corpus-only"); err != nil || got != "answer" {
				t.Fatalf("Run = %q, %v", got, err)
			}
			if *checks != 2 {
				t.Fatalf("a remembered version was checked again (%d calls)", *checks)
			}
		})
	}
}

// A record vouches for the binary that was checked, not for every binary that prints
// the same version (source builds, a reinstall, a runtime folder copied elsewhere).
func TestRecordIsPerBinary(t *testing.T) {
	r := setUpChild(t)
	t.Setenv("WITNESS_CODEX_HELPER_VERSION", "0.172.0")
	bin := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(bin, []byte("stand-in"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_CODEX_BINARY", bin)
	checks := countChecks(t, false)
	ctx := context.Background()
	if err := r.Open(ctx); err != nil {
		t.Fatal(err)
	}
	other := time.Now().Add(-time.Hour)
	if err := os.Chtimes(bin, other, other); err != nil {
		t.Fatal(err)
	}
	if err := r.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if *checks != 4 {
		t.Fatalf("a different binary reporting the same version reused the record (%d check calls, want 4)", *checks)
	}
}

func TestPinnedVersionSkipsTheCheck(t *testing.T) {
	r := setUpChild(t)
	checks := countChecks(t, false)
	if err := r.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "test", "witness-system", "corpus-only"); err != nil {
		t.Fatal(err)
	}
	if *checks != 0 {
		t.Fatalf("pinned version ran %d check calls", *checks)
	}
}

func TestFailedCheckRefusesAndIsNotRemembered(t *testing.T) {
	r := helperRunner(t) // opened on a pinned version; the CLI then changes under the drain
	t.Setenv("WITNESS_CODEX_HELPER_VERSION", "0.172.0")
	t.Setenv("WITNESS_CODEX_HELPER_MODE", "leak-tools")
	countChecks(t, true)
	_, err := r.Run(context.Background(), "test", "witness-system", "corpus-only")
	checkPreflightError(t, err, "tools offered")
	checkPreflightError(t, err, "Codex preflight:")
	checkPreflightError(t, r.Open(context.Background()), "tools offered")
	if b, err := os.ReadFile(filepath.Join(r.root, "codex", "verified.json")); err == nil && strings.Contains(string(b), "0.172.0") {
		t.Fatalf("a failed check was remembered: %s", b)
	}
}
