package file

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

func TestFilePlatformShape(t *testing.T) {
	p := Platform{}
	if p.Name() != "file" {
		t.Errorf("Name() = %q, want file", p.Name())
	}
	if p.SessionPrefix() != "file:" {
		t.Errorf("SessionPrefix() = %q, want file:", p.SessionPrefix())
	}
}

func TestFilePlatformRegistered(t *testing.T) {
	// The blank-import side effect: a "file:"-prefixed session resolves to this platform.
	if _, ok := platform.ByName("file"); !ok {
		t.Fatal("file platform not registered via init()")
	}
}
