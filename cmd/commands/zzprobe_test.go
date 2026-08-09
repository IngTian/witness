package commands

import (
	"path/filepath"
	"testing"
)

// Simulate a STALE installed plugin (v0.6.1 and earlier shipped
// ["import","--agent","opencode","--quiet","--auto"]) hitting the current binary.
func TestProbeStalePluginArgs(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	previousSpawner := importWorkerSpawner
	importWorkerSpawner = func() {}
	t.Cleanup(func() { importWorkerSpawner = previousSpawner })
	err := cmdImport([]string{"--agent", "opencode", "--quiet", "--auto"})
	t.Logf("stale v0.6.1 plugin arg vector => %v", err)
	// and an unknown flag for comparison
	err2 := cmdImport([]string{"--agent", "opencode", "--quiet", "--bogus"})
	t.Logf("unknown flag => %v", err2)
}
