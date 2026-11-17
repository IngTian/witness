package lens

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// Every file the loader reads from a lens directory must be in store.LensFiles, because that is
// exactly how an optional prompt file was lost from v0.6.0 to v0.7.3. A source scan, because the
// property is "no other read exists".
func TestLoadDirReadsOnlyLensFiles(t *testing.T) {
	src, err := os.ReadFile("lens.go")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range store.LensFiles {
		listed[f.Name] = true
	}
	// The aliases lens.go may name a file by. A new one fails below until it is added here
	// AND to store.LensFiles.
	aliases := map[string]string{"ConfigFile": ConfigFile, "ExtractFile": ExtractFile, "ReviewFile": ReviewFile}
	reads := regexp.MustCompile(`filepath\.Join\(dir, ([^)]+)\)`).FindAllStringSubmatch(string(src), -1)
	if len(reads) == 0 {
		t.Fatal("found no lens-directory reads in lens.go — the scan is vacuous")
	}
	for _, m := range reads {
		if m[1] == `"lens.md"` {
			continue // the pre-#75 format, probed only to word an error; never loaded
		}
		if name, ok := aliases[m[1]]; !ok || !listed[name] {
			t.Errorf("the loader reads %s, which is not in store.LensFiles: RegisterLens would not copy it", m[1])
		}
	}
}

// LensConfig's struct tags must spell the store's lens.json keys (tags cannot reference
// constants), or a `witness lens set` write lands under a key the loader never reads.
func TestLensConfigTagsMatchStoreKeys(t *testing.T) {
	typ := reflect.TypeOf(LensConfig{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" {
			got = append(got, name)
		}
	}
	want := slices.Clone(store.LensKeys)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("lens.json keys = %v, want the store's %v", got, want)
	}
}
