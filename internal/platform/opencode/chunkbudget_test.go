package opencode

import "testing"

// TestEffectiveChunkMaxChars locks the WITNESS_CHUNK_MAX_CHARS override contract:
// a valid value wins, unset/garbage falls back to the compiled default, and a
// non-positive value is passed through verbatim so renderChunks' "<=0 means send
// whole" branch stays reachable from the env (the knob used to measure chunk-size
// vs. arc-preservation in issue #57).
func TestEffectiveChunkMaxChars(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want int
	}{
		{"unset falls back to default", false, "", chunkMaxChars},
		{"valid override wins", true, "48000", 48000},
		{"garbage falls back to default", true, "not-a-number", chunkMaxChars},
		{"empty falls back to default", true, "", chunkMaxChars},
		{"non-positive passes through (never-chunk sentinel)", true, "0", 0},
		{"negative passes through", true, "-1", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("WITNESS_CHUNK_MAX_CHARS", tc.val)
			} else {
				// t.Setenv can't unset; rely on the ambient env being clean. If a
				// stray value leaks in, skip rather than assert a wrong default.
				if _, ok := lookupChunkEnv(); ok {
					t.Skip("WITNESS_CHUNK_MAX_CHARS set in ambient env; skipping unset case")
				}
			}
			if got := effectiveChunkMaxChars(); got != tc.want {
				t.Fatalf("effectiveChunkMaxChars() = %d, want %d", got, tc.want)
			}
		})
	}
}
