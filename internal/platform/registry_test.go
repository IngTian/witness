package platform_test

import (
	"context"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

// A platform whose Name() is non-lowercase/padded must still be resolvable —
// Register and ByName must normalize identically, or a third-party platform (the
// PR's headline extensibility) would register yet be unreachable via ForSession/
// RunnerFor. Uses a throwaway name so it doesn't collide with the real platforms.
func TestRegisterNormalizesNameLikeByName(t *testing.T) {
	platform.Register(mixedCasePlatform{})
	if _, ok := platform.ByName("mixedcaseplat"); !ok {
		t.Fatal("a platform registered with a MixedCase Name() must resolve via its normalized name")
	}
	if _, ok := platform.ByName("  MixedCasePlat "); !ok {
		t.Fatal("ByName must resolve the same padded/mixed-case form Register accepted")
	}
}

type mixedCasePlatform struct{}

func (mixedCasePlatform) Name() string                              { return " MixedCasePlat " }
func (mixedCasePlatform) SessionPrefix() string                     { return "mixedcaseplat:" }
func (mixedCasePlatform) RenderInputs(r []store.RawRecord) []string { return []string{""} }
func (mixedCasePlatform) Import(context.Context, *store.Store, []string) (platform.ImportStats, error) {
	return platform.ImportStats{}, nil
}

func TestByNameAndDefault(t *testing.T) {
	if _, ok := platform.ByName("claude"); !ok {
		t.Fatal("claude not registered")
	}
	if _, ok := platform.ByName("opencode"); !ok {
		t.Fatal("opencode not registered")
	}
	if _, ok := platform.ByName("nope"); ok {
		t.Fatal("unknown platform must not resolve (fail-closed)")
	}
	// Case/space-insensitive lookup.
	if _, ok := platform.ByName("  OpenCode "); !ok {
		t.Fatal("ByName should normalize case/whitespace")
	}
	if platform.Default().Name() != "claude" {
		t.Fatalf("Default() = %q, want claude", platform.Default().Name())
	}
}

func TestForSessionByPrefixAndDefault(t *testing.T) {
	// No store: resolution falls through to prefix, then Default. The asymmetric
	// rule is "opencode:"-prefixed => OpenCode, everything else => Claude.
	cases := map[string]string{
		"opencode:abc":   "opencode",
		"abc-123":        "claude", // unmarked
		"claude:s":       "claude", // a "claude:" prefix is NOT special; only opencode: is
		"opencodeX:trap": "claude", // must be the exact prefix, not a substring
	}
	for session, want := range cases {
		if got := platform.ForSession(nil, session).Name(); got != want {
			t.Fatalf("ForSession(nil, %q) = %q, want %q", session, got, want)
		}
	}
}

func TestForSessionColumnOverridesPrefix(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// A session whose id has NO opencode prefix but whose persisted column says
	// opencode must resolve to opencode — the column is authoritative over prefix.
	st.SetSessionPlatform("weird-id", "opencode")
	if got := platform.ForSession(st, "weird-id").Name(); got != "opencode" {
		t.Fatalf("column should override prefix: got %q, want opencode", got)
	}

	// An unclassified session (no row) still resolves by prefix/default.
	if got := platform.ForSession(st, "opencode:x").Name(); got != "opencode" {
		t.Fatalf("unset column should fall back to prefix: got %q", got)
	}
	if got := platform.ForSession(st, "plain").Name(); got != "claude" {
		t.Fatalf("unset column, no prefix should default to claude: got %q", got)
	}
}

func TestRenderInputsPerPlatform(t *testing.T) {
	raw := []store.RawRecord{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi"},
	}
	cc, _ := platform.ByName("claude")
	if got := cc.RenderInputs(raw); len(got) != 1 {
		t.Fatalf("claude should render one transcript, got %d", len(got))
	}
	// OpenCode with the default (large) budget also fits in one chunk here; the
	// chunking-splits-many case is covered by the distill worker integration test.
	oc, _ := platform.ByName("opencode")
	if got := oc.RenderInputs(raw); len(got) != 1 {
		t.Fatalf("opencode short transcript should be one chunk, got %d", len(got))
	}
}
