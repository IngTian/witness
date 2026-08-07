//go:build windows

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func openCodeExe() (string, error) {
	name := "opencode"
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		dir = filepath.Clean(dir)
		if strings.Contains(filepath.ToSlash(dir), "@opencode-aidesktop") {
			continue
		}
		candidate := filepath.Join(dir, name+".exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return exec.LookPath(name)
}
