package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDirResolutionOrder locks the precedence contract that both embed.go and
// lens.go depend on: env override > CLAUDE_PLUGIN_ROOT > exe-relative > cwd. The
// order is load-bearing — a Windows exec-form hook sets none of the env vars, so
// the exe-relative step is the only thing that finds the model/prompts there.
func TestDirResolutionOrder(t *testing.T) {
	// Isolate from the ambient environment; restore after.
	for _, k := range []string{"WITNESS_ASSETS", "CLAUDE_PLUGIN_ROOT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	t.Run("env override wins over everything", func(t *testing.T) {
		t.Setenv("CLAUDE_PLUGIN_ROOT", "/plugin/root")
		t.Setenv("WITNESS_ASSETS", "/explicit/assets")
		if got := Dir(filepath.Join("assets", "e5-small"), "WITNESS_ASSETS"); got != "/explicit/assets" {
			t.Fatalf("env override: got %q, want /explicit/assets", got)
		}
	})

	t.Run("CLAUDE_PLUGIN_ROOT used when no env override", func(t *testing.T) {
		os.Unsetenv("WITNESS_ASSETS")
		t.Setenv("CLAUDE_PLUGIN_ROOT", "/plugin/root")
		want := filepath.Join("/plugin/root", "prompts")
		if got := Dir("prompts", "WITNESS_PROMPTS"); got != want {
			t.Fatalf("plugin root: got %q, want %q", got, want)
		}
	})

	t.Run("empty envOverride name skips the override check", func(t *testing.T) {
		os.Unsetenv("WITNESS_ASSETS")
		t.Setenv("CLAUDE_PLUGIN_ROOT", "/plugin/root")
		want := filepath.Join("/plugin/root", "prompts")
		if got := Dir("prompts", ""); got != want {
			t.Fatalf("empty override name: got %q, want %q", got, want)
		}
	})
}

// TestExeRelativeFindsSibling proves the exe-relative probe (the Windows
// exec-form path) resolves assets laid out beside the executable, with no env
// vars set. We can't move the test binary, so we exercise exeRelative against a
// temp layout by temporarily standing in for os.Executable via a real file: we
// verify the two documented candidate shapes (exeDir/subdir and exeDir/../subdir)
// both resolve when present.
func TestExeRelativeCandidates(t *testing.T) {
	// Layout A — installed: <root>/witness(.exe) + <root>/assets/e5-small.
	rootA := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootA, "assets", "e5-small"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Layout B — build: <repo>/bin/witness-os-arch + <repo>/assets/e5-small.
	rootB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootB, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootB, "assets", "e5-small"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		exeDir string
		want   string
	}{
		{"installed layout (sibling)", rootA, filepath.Join(rootA, "assets", "e5-small")},
		{"build layout (parent)", filepath.Join(rootB, "bin"), filepath.Join(rootB, "assets", "e5-small")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := probeFrom(tt.exeDir, filepath.Join("assets", "e5-small"))
			if !ok {
				t.Fatalf("expected to resolve under %s, got miss", tt.exeDir)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("miss when neither candidate exists", func(t *testing.T) {
		empty := t.TempDir()
		if _, ok := probeFrom(empty, filepath.Join("assets", "e5-small")); ok {
			t.Fatal("expected miss for empty dir")
		}
	})
}

// A first `witness model fetch` has no assets dir to probe for, so CreateDir anchors on prompts/,
// which every layout ships beside the binary — never on the cwd the user happens to stand in.
func TestCreateFromAnchorsOnPromptsBesideTheBinary(t *testing.T) {
	sub := filepath.Join("assets", "e5-small")
	repo := t.TempDir() // build layout: <repo>/bin/witness-* + <repo>/prompts
	for _, d := range []string{"bin", "prompts"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := createFrom(filepath.Join(repo, "bin"), sub), filepath.Join(repo, sub); got != want {
		t.Errorf("build layout: got %q, want %q", got, want)
	}
	inst := t.TempDir() // installed layout: <dir>/witness + <dir>/prompts
	if err := os.MkdirAll(filepath.Join(inst, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := createFrom(inst, sub), filepath.Join(inst, sub); got != want {
		t.Errorf("installed layout: got %q, want %q", got, want)
	}
	if got := createFrom(t.TempDir(), sub); got != sub { // `go run`: nothing beside the binary
		t.Errorf("no bundle beside the binary: got %q, want the cwd fallback %q", got, sub)
	}
}
