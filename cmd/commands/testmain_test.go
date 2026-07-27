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
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
