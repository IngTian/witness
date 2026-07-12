package platform_test

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

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
