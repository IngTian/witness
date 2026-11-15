package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No build tag: this runs in make test. A contract test missing the tag would spawn real OpenCode
// in make test.
func TestContractFilesAreBuildTagged(t *testing.T) {
	files, _ := filepath.Glob("contract_*_test.go")
	if len(files) == 0 {
		t.Fatal("no contract_*_test.go files: the glob or the naming drifted")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if first, _, _ := strings.Cut(string(b), "\n"); !strings.HasPrefix(first, "//go:build opencodecontract") {
			t.Errorf("%s: first line %q, want //go:build opencodecontract…", f, first)
		}
	}
}
