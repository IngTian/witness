package opencode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// `opencode --version` on a packaged Windows build interleaves crash-reporter and startup
// logging with the version, so the old whitespace-field Sscanf parser found NOTHING and witness
// reported "native session isolation unavailable … upgrade to 1.18.0+" against a real 1.18.14.
// Verbatim output from the failing run:
func TestParseOpenCodeVersionHandlesWindowsCrashReporterNoise(t *testing.T) {
	const windowsOut = `17:41:48.929 (crash) > crash reporter started {
  path: 'C:\Users\tzy20\AppData\Roaming\ai.opencode.desktop\Crashpad'
}
17:41:48.947         > app starting { version: '1.18.14', packaged: true, onboardingTest: false }`

	v, err := ParseOpenCodeVersion(windowsOut)
	if err != nil {
		t.Fatalf("failed to find the version in real Windows output; witness would wrongly claim "+
			"the install is too old and refuse to distill: %v", err)
	}
	if v.Major != 1 || v.Minor != 18 || v.Patch != 14 {
		t.Errorf("parsed %v, want 1.18.14 (a clock like 17:41:48.929 must NOT be read as a version)", v)
	}
}

// The LABELED pattern must be tried before the bare one, and this is the case that proves it.
//
// Found by reverting: with `openCodeVersionLabeled` deleted, every other version test still
// passed — so nothing justified having two patterns, and the labeled one looked like dead
// complexity. It is not. When ANY other version-shaped number precedes the real one, the bare
// pattern picks the wrong value and the 1.18 gate then decides on it:
//
//	"loaded plugin 2.3.4 …  version: '1.18.14'"   bare -> 2.3.4  (passes the gate for the
//	                                               wrong reason — a 2.x plugin, not opencode)
//	"config schema 0.9.1 ok; version: 1.18.14"    bare -> 0.9.1  (FAILS the gate, refusing a
//	                                               perfectly good install)
//
// The second is the dangerous direction: it is exactly the class of false "upgrade to 1.18.0+"
// rejection this whole fix exists to stop.
func TestParseOpenCodeVersionPrefersTheLabelledVersionOverAnEarlierNumber(t *testing.T) {
	for _, tc := range []struct {
		name, in     string
		major, minor int
	}{
		{"a plugin version precedes it",
			"loaded plugin 2.3.4 from disk\napp starting { version: '1.18.14', packaged: true }", 1, 18},
		{"a schema version precedes it",
			"config schema 0.9.1 ok; version: 1.18.14", 1, 18},
		{"a build number precedes it",
			"build 20.1.3\n> app starting { version: '1.19.0' }", 1, 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseOpenCodeVersion(tc.in)
			if err != nil {
				t.Fatalf("no version found at all: %v", err)
			}
			if v.Major != tc.major || v.Minor != tc.minor {
				t.Errorf("parsed %v, want %d.%d — an earlier number was mistaken for the "+
					"version, so the 1.18 gate decides on the wrong value",
					v, tc.major, tc.minor)
			}
		})
	}
}

func TestParseOpenCodeVersionShapes(t *testing.T) {
	for _, tc := range []struct {
		name                string
		in                  string
		wantErr             bool
		major, minor, patch int
	}{
		{"clean v2 output", "opencode v2.2.7\n", false, 2, 2, 7},
		{"bare version", "2.3.0", false, 2, 3, 0},
		{"prerelease suffix", "v2.2.7-beta.1", false, 2, 2, 7},
		{"quoted in a json-ish blob", `{ version: '2.3.1' }`, false, 2, 3, 1},
		{"clock timestamps only", "17:41:48.929 starting\n17:41:49.001 ready", true, 0, 0, 0},
		{"not found at all", "opencode: command not found", true, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseOpenCodeVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseOpenCodeVersion(%q) succeeded, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOpenCodeVersion(%q) failed: %v", tc.in, err)
			}
			if v.Major != tc.major || v.Minor != tc.minor || v.Patch != tc.patch {
				t.Errorf("parsed %v, want %d.%d.%d", v, tc.major, tc.minor, tc.patch)
			}
		})
	}
}

func TestCheckVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		v       Version
		wantErr error
	}{
		{Version{1, 19, 5}, ErrOpenCodeV1},
		{Version{1, 18, 0}, ErrOpenCodeV1},
		{Version{2, 2, 6}, ErrOpenCodeTooOld},
		{Version{2, 0, 0}, ErrOpenCodeTooOld},
		{Version{2, 2, 7}, nil},
		{Version{2, 3, 0}, nil},
		{Version{2, 9, 0}, nil},
		{Version{3, 0, 0}, ErrOpenCodeTooNew},
	} {
		t.Run(tc.v.String(), func(t *testing.T) {
			err := CheckVersion(tc.v)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("CheckVersion(%v) = %v, want nil", tc.v, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("CheckVersion(%v) = %v, want %v", tc.v, err, tc.wantErr)
			}
			// The error text must contain the version string for debugging
			if !strings.Contains(err.Error(), tc.v.String()) {
				t.Errorf("error text %q does not contain version %v", err.Error(), tc.v)
			}
		})
	}

	// v1 error text must mention the major upgrade path
	err := CheckVersion(Version{1, 19, 5})
	if !strings.Contains(err.Error(), "opencode upgrade --major") {
		t.Errorf("v1 error %q does not mention 'opencode upgrade --major'", err)
	}

	// too-old v2 error text must mention upgrade
	err = CheckVersion(Version{2, 2, 6})
	if !strings.Contains(err.Error(), "opencode upgrade") {
		t.Errorf("too-old error %q does not mention 'opencode upgrade'", err)
	}
}

func TestAboveTestedComparesMajorMinorOnly(t *testing.T) {
	for _, tc := range []struct {
		v    Version
		want bool
	}{
		{Version{2, 3, 9}, false},
		{Version{2, 4, 0}, true},
		{Version{2, 2, 7}, false},
	} {
		got := AboveTested(tc.v)
		if got != tc.want {
			t.Errorf("AboveTested(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestProbeVersionIsBoundedAndParses(t *testing.T) {
	// Stub returning a valid version
	prevCmd := openCodeVersionCommand
	t.Cleanup(func() { openCodeVersionCommand = prevCmd })

	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("opencode v2.3.0\n"), nil
	}
	v, err := ProbeVersion(context.Background())
	if err != nil {
		t.Fatalf("ProbeVersion() failed: %v", err)
	}
	if v.Major != 2 || v.Minor != 3 || v.Patch != 0 {
		t.Errorf("ProbeVersion() = %v, want 2.3.0", v)
	}

	// Stub that blocks should timeout
	withShortTimeouts(t, 150*time.Millisecond)
	openCodeVersionCommand = func(ctx context.Context, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	_, err = ProbeVersion(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ProbeVersion() with blocking stub should fail")
	}
	if elapsed > 10*time.Second {
		t.Errorf("ProbeVersion() took %v, should have timed out much sooner", elapsed)
	}
}
