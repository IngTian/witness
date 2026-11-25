package codex

import (
	"context"
	"strings"
	"testing"
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
	if err := selfCheckChild(t, "blind"); err == nil || !strings.Contains(err.Error(), "cannot see") {
		t.Fatalf("want a cannot-see refusal, got %v", err)
	}
}
