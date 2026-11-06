package commands

import (
	"runtime/debug"
	"testing"
)

// install.sh (and `go build`/`go install`) pass no ldflags, so before this every such binary
// said "dev (commit none, built unknown)" — and both stale-binary incidents were diagnosable only
// from build metadata. Go stamps the VCS state into a plain build; --version must read it.
func TestBuildStampReadsVCSInfoFromAPlainBuild(t *testing.T) {
	for _, tc := range []struct {
		name                string
		info                debug.BuildInfo
		version, commit, at string
	}{
		{
			name: "dirty checkout",
			info: debug.BuildInfo{
				Main: debug.Module{Version: "v0.12.1-0.20261106094801-2f0f03bcabd4+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "2f0f03bcabd4634e73102994e0f5b9612a6a908c"},
					{Key: "vcs.time", Value: "2026-11-06T09:48:01Z"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			version: "v0.12.1-0.20261106094801-2f0f03bcabd4+dirty", commit: "2f0f03b+dirty", at: "2026-11-06T09:48:01Z",
		},
		{
			name: "clean tag",
			info: debug.BuildInfo{
				Main: debug.Module{Version: "v0.12.0"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "2f0f03bcabd4634e73102994e0f5b9612a6a908c"},
					{Key: "vcs.time", Value: "2026-11-06T09:48:01Z"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			version: "v0.12.0", commit: "2f0f03b", at: "2026-11-06T09:48:01Z",
		},
		{
			// `go install module@version` from the proxy: a version, no VCS settings.
			name:    "module proxy",
			info:    debug.BuildInfo{Main: debug.Module{Version: "v0.12.0"}},
			version: "v0.12.0", commit: "none", at: "unknown",
		},
		{
			// `go test`, or a build outside any checkout: nothing to read, keep the defaults.
			name:    "no metadata",
			info:    debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			version: "dev", commit: "none", at: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, c, at := buildStamp(&tc.info)
			if v != tc.version || c != tc.commit || at != tc.at {
				t.Fatalf("buildStamp = (%q, %q, %q), want (%q, %q, %q)", v, c, at, tc.version, tc.commit, tc.at)
			}
		})
	}
}
