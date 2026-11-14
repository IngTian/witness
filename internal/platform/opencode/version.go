package opencode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Version represents an OpenCode semantic version.
type Version struct {
	Major, Minor, Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// MinVersion is the minimum supported OpenCode version (2.2.7).
var MinVersion = Version{2, 2, 7}

// NewestTestedMinor is the newest minor version tested in CI (2.3.x). Versions above this
// (compared on major.minor only) trigger a warning in doctor and wire, but never refuse.
var NewestTestedMinor = Version{2, 3, 0}

// openCodeVersionLabeled / openCodeVersionBare extract the semver from `opencode --version`.
//
// The old parser split on whitespace and Sscanf'd each field, which assumed the output is a
// bare "1.18.13". On Windows it is not: the packaged build writes crash-reporter and startup
// logging to the same stream, so a real 1.18.14 install produced (verbatim, from issue #10)
//
//	17:41:48.929 (crash) > crash reporter started {
//	  path: 'C:\Users\tzy20\AppData\Roaming\ai.opencode.desktop\Crashpad'
//	}
//	17:41:48.947         > app starting { version: '1.18.14', packaged: true, ... }
//
// and every field failed to parse: the version is QUOTED ('1.18.14',) so Sscanf rejects the
// leading quote, and the clock timestamps are not N.N.N either. So witness reported "native
// session isolation unavailable … upgrade to 1.18.0+" against an install that was already
// new enough — blocking distillation entirely, with a message pointing at the wrong problem.
//
// Two patterns, tried in order:
//   - labeled: a `version: 'X.Y.Z'` (or version=X.Y.Z) key wins, because when the tool names
//     its own version that is authoritative;
//   - bare: any X.Y.Z not preceded by a digit, '.' or ':' — the ':' exclusion is what stops a
//     clock like 17:41:48.929 from being read as version 41.48.929.
var (
	openCodeVersionLabeled = regexp.MustCompile(`(?i)version['"\s:=]+v?(\d+)\.(\d+)\.(\d+)`)
	openCodeVersionBare    = regexp.MustCompile(`(?:^|[^\d.:])v?(\d+)\.(\d+)\.(\d+)(?:[^\d.]|$)`)
)

// ParseOpenCodeVersion extracts the semantic version from `opencode --version` output.
func ParseOpenCodeVersion(out string) (Version, error) {
	for _, re := range []*regexp.Regexp{openCodeVersionLabeled, openCodeVersionBare} {
		if m := re.FindStringSubmatch(out); m != nil {
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			patch, _ := strconv.Atoi(m[3])
			return Version{major, minor, patch}, nil
		}
	}
	return Version{}, fmt.Errorf("no OpenCode version in %q", out)
}

// ErrOpenCodeV1 marks an OpenCode v1 install. v1 is not supported; witness needs 2.2.7 or newer.
var ErrOpenCodeV1 = errors.New("OpenCode v1 is not supported: witness needs OpenCode 2.2.7 or newer " +
	"— run `opencode upgrade --major` to upgrade to v2, or install it fresh " +
	"(witness v0.13.0 was the last release for OpenCode v1)")

// ErrOpenCodeTooOld marks an OpenCode v2 install below MinVersion.
var ErrOpenCodeTooOld = errors.New("OpenCode version is too old: witness needs 2.2.7 or newer " +
	"— run `opencode upgrade` to upgrade")

// ErrOpenCodeTooNew marks an OpenCode v3+ install (not yet supported).
var ErrOpenCodeTooNew = errors.New("OpenCode 3 is not supported yet")

// CheckVersion checks if v is in the supported range [2.2.7, 3.0.0).
func CheckVersion(v Version) error {
	if v.Major < 2 {
		return fmt.Errorf("found OpenCode %s: %w", v, ErrOpenCodeV1)
	}
	if v.Major >= 3 {
		return fmt.Errorf("found OpenCode %s: %w", v, ErrOpenCodeTooNew)
	}
	if v.Major == 2 && (v.Minor < MinVersion.Minor || (v.Minor == MinVersion.Minor && v.Patch < MinVersion.Patch)) {
		return fmt.Errorf("found OpenCode %s: %w", v, ErrOpenCodeTooOld)
	}
	return nil
}

// AboveTested returns true if v is above NewestTestedMinor (compared on major.minor only).
func AboveTested(v Version) bool {
	if v.Major != NewestTestedMinor.Major {
		return v.Major > NewestTestedMinor.Major
	}
	return v.Minor > NewestTestedMinor.Minor
}

// ProbeVersion runs `opencode --version` and parses the output, bounded by openCodeProbeTimeout.
func ProbeVersion(ctx context.Context) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, openCodeProbeTimeout)
	defer cancel()
	out, err := openCodeVersionCommand(ctx, "")
	if err != nil {
		return Version{}, fmt.Errorf("opencode --version: %w", err)
	}
	return ParseOpenCodeVersion(string(out))
}

// ErrOpenCodeRunnerPending marks the unfinished v2 runner port. The capture side supports v2,
// but the runner does not yet (PR 2). An intermediate main must never drive the v1 serve
// protocol against v2 (the routes return 404 and fail silently).
var ErrOpenCodeRunnerPending = errors.New("distilling through OpenCode v2 is not built yet (#218); " +
	"capture works — route these lenses to another runner for now")
