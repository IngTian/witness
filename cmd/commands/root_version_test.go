package commands

import (
	"runtime/debug"
	"testing"
)

// install.sh (and `go build`/`go install`) pass no ldflags, so before this every such binary
// said "dev (commit none, built unknown)" — and both stale-binary incidents were diagnosable only
// from build metadata. Go stamps the VCS state into a plain build; --version must read it. A
// Makefile/release build is the other half: its ldflags must come through untouched.
func TestStampFillsOnlyWhatLdflagsLeftAtDefault(t *testing.T) {
	vcs := func(version, modified string) debug.BuildInfo {
		return debug.BuildInfo{
			Main: debug.Module{Version: version},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "2f0f03bcabd4634e73102994e0f5b9612a6a908c"},
				{Key: "vcs.time", Value: "2026-11-06T09:48:01Z"},
				{Key: "vcs.modified", Value: modified},
			},
		}
	}
	for _, tc := range []struct {
		name string
		in   [3]string // version, commit, buildTime as ldflags (or the defaults) left them
		info debug.BuildInfo
		ok   bool
		want [3]string
	}{
		{
			name: "dirty checkout",
			in:   [3]string{"dev", "none", "unknown"},
			info: vcs("v0.12.1-0.20261106094801-2f0f03bcabd4+dirty", "true"), ok: true,
			want: [3]string{"v0.12.1-0.20261106094801-2f0f03bcabd4+dirty", "2f0f03b+dirty", "2026-11-06T09:48:01Z"},
		},
		{
			name: "clean tag",
			in:   [3]string{"dev", "none", "unknown"},
			info: vcs("v0.12.0", "false"), ok: true,
			want: [3]string{"v0.12.0", "2f0f03b", "2026-11-06T09:48:01Z"},
		},
		{
			// `go install module@version` from the proxy: a version, no VCS settings.
			name: "module proxy",
			in:   [3]string{"dev", "none", "unknown"},
			info: debug.BuildInfo{Main: debug.Module{Version: "v0.12.0"}}, ok: true,
			want: [3]string{"v0.12.0", "none", "unknown"},
		},
		{
			// `go test`, or a build outside any checkout: nothing to read, keep the defaults.
			name: "no metadata",
			in:   [3]string{"dev", "none", "unknown"},
			info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, ok: true,
			want: [3]string{"dev", "none", "unknown"},
		},
		{
			name: "no build info at all",
			in:   [3]string{"dev", "none", "unknown"},
			ok:   false,
			want: [3]string{"dev", "none", "unknown"},
		},
		{
			// A Makefile/release build: ldflags win even though VCS info is present.
			name: "ldflags pass through",
			in:   [3]string{"v9.9.9", "abc1234", "2030-01-01T00:00:00Z"},
			info: vcs("v0.12.1-0.20261106094801-2f0f03bcabd4+dirty", "true"), ok: true,
			want: [3]string{"v9.9.9", "abc1234", "2030-01-01T00:00:00Z"},
		},
		{
			// `make build` outside a checkout: git describe/rev-parse fall back to dev/none, but
			// `date` still stamps a real build time — a missing vcs.time must not reset it.
			name: "makefile buildTime kept",
			in:   [3]string{"dev", "none", "2030-01-01T00:00:00Z"},
			info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, ok: true,
			want: [3]string{"dev", "none", "2030-01-01T00:00:00Z"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bi := &tc.info
			if !tc.ok {
				bi = nil // what debug.ReadBuildInfo returns alongside false
			}
			v, c, at := stamp(tc.in[0], tc.in[1], tc.in[2], bi, tc.ok)
			if got := [3]string{v, c, at}; got != tc.want {
				t.Fatalf("stamp(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
