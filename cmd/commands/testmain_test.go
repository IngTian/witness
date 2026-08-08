package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "witness-commands-test-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	assets := filepath.Join(root, "empty-assets")
	if err := os.Mkdir(assets, 0o700); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	_ = os.Setenv("WITNESS_HOME", filepath.Join(root, "witness"))
	_ = os.Setenv("WITNESS_OPENCODE_DB", filepath.Join(root, "opencode.db"))
	_ = os.Setenv("WITNESS_ASSETS", assets)
	_ = os.Setenv("WITNESS_SKIP_MODEL_DOWNLOAD", "1")
	_ = os.Setenv(platform.DisableExternalRunnersEnv, "1")
	// LOCALAPPDATA belongs in the package-wide sandbox, not in one test.
	//
	// The Windows install path is DESTRUCTIVE and validates late: installBundle copies
	// os.Executable() to <LOCALAPPDATA>\witness\witness.exe before probeSrcTree can reject a
	// non-built source tree (install_windows.go). Under `go test` os.Executable() is the TEST
	// binary and copyFile's os.SameFile short-circuit compares different directories, so it does
	// not fire — running this package's suite on Windows would overwrite a user's installed
	// witness.exe and put a build temp dir on their PATH.
	//
	// Redirecting it here rather than per-test is deliberate: any FUTURE test that reaches
	// installSelf/installBundle/ensureOnUserPath inherits the containment automatically. A
	// per-test t.Setenv only protects the test that remembers to write it, and this hazard is
	// invisible on the platform the tests are usually run on.
	_ = os.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
