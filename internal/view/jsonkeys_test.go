package view

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Every JSON key the Model sends must be one page.js reads. Renaming a tag (Trait `today`,
// `trendFrom`, Topic `axisStart`) passes every other Go test, and the page then renders "NaN%
// sure" or picks the wrong axis; view-e2e.sh would catch it but is manual and not in CI.
//
// It only covers that direction. A Go field DELETED along with its tag, or a typo on the JS side
// reading a key Go never sends, still passes; those need the headless check.
func TestEveryModelKeyIsReadByThePage(t *testing.T) {
	js, err := os.ReadFile("assets/page.js")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
				keys[name] = true
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(Model{}))
	if len(keys) < 50 {
		t.Fatalf("walked only %d keys; the reflect walk is broken and this test is vacuous", len(keys))
	}
	for k := range keys {
		// Read as a property: `x.key`, or `key:` in a destructure/literal.
		if !regexp.MustCompile(`\.` + k + `\b|\b` + k + `\s*:`).Match(js) {
			t.Errorf("page.js never reads the Model key %q — renamed in Go but not in the page?", k)
		}
	}
}
