package platform_test

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

func TestAppendFrom(t *testing.T) {
	for _, tc := range []struct {
		name     string
		old, cur []string
		raw      int
		from     int
		ok       bool
	}{
		{"fresh", nil, []string{"a", "b"}, 0, 0, true},
		{"pure append", []string{"a"}, []string{"a", "b"}, 1, 1, true},
		{"unchanged", []string{"a", "b"}, []string{"a", "b"}, 2, 2, true},
		{"merge tail", []string{"a", "b"}, []string{"a", "b", "c"}, 3, 2, true},
		{"edit", []string{"a", "b"}, []string{"a", "X"}, 2, 0, false},
		{"rewind", []string{"a", "b", "c"}, []string{"a", "b"}, 3, 0, false},
		{"no keys with raw", nil, []string{"a"}, 1, 0, false},
		{"raw short of keys", []string{"a", "b"}, []string{"a", "b", "c"}, 1, 0, false},
	} {
		if from, ok := platform.AppendFrom(tc.old, tc.cur, tc.raw); from != tc.from || ok != tc.ok {
			t.Errorf("%s: AppendFrom = (%d, %v), want (%d, %v)", tc.name, from, ok, tc.from, tc.ok)
		}
	}
}
