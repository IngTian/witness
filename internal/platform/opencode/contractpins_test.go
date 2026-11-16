package opencode

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
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

// The contract workflow runs real OpenCode only at the versions pins.txt pins, so the pins must span
// exactly the range version.go claims is tested: bumping MinVersion or NewestTestedMinor without
// pinning (and running) that version fails here, in make test, not in a daily run nobody reads.
func TestContractWorkflowMatchesSupportRange(t *testing.T) {
	b, err := os.ReadFile("../../../scripts/opencode-contract/pins.txt")
	if err != nil {
		t.Fatal(err)
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	pinned := map[string]map[string]bool{} // version → its pinned assets
	var versions []Version
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !sha.MatchString(f[2]) {
			t.Fatalf("pins.txt line %q, want <version> <asset> <64-hex sha256>", line)
		}
		if pinned[f[0]] == nil {
			v, err := ParseOpenCodeVersion(f[0])
			if err != nil || v.String() != f[0] {
				t.Fatalf("pins.txt version %q is not a bare X.Y.Z (%v)", f[0], err)
			}
			versions, pinned[f[0]] = append(versions, v), map[string]bool{}
		}
		if pinned[f[0]][f[1]] { // fetch.sh would take the first, whichever sha that is
			t.Fatalf("pins.txt pins %s %s twice", f[0], f[1])
		}
		pinned[f[0]][f[1]] = true
	}
	if len(versions) == 0 {
		t.Fatal("pins.txt pins nothing")
	}
	slices.SortFunc(versions, func(a, b Version) int {
		return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch))
	})
	lo, hi := versions[0], versions[len(versions)-1]
	if lo != MinVersion {
		t.Errorf("lowest pin %v, want MinVersion %v", lo, MinVersion)
	}
	if hi.Major != NewestTestedMinor.Major || hi.Minor != NewestTestedMinor.Minor {
		t.Errorf("highest pin %v, want NewestTestedMinor's %d.%d", hi, NewestTestedMinor.Major, NewestTestedMinor.Minor)
	}
	var names []string
	for _, v := range versions {
		names = append(names, v.String())
		for _, a := range []string{"opencode-linux-x64-baseline.tar.gz", "opencode-windows-x64.zip"} {
			if !pinned[v.String()][a] {
				t.Errorf("pins.txt: %v has no %s pin", v, a)
			}
		}
	}
	if !pinned[hi.String()]["opencode-desktop-win-x64.exe"] { // the desktop leg installs OPENCODE_NEWEST
		t.Errorf("pins.txt: %v has no opencode-desktop-win-x64.exe pin", hi)
	}

	wb, err := os.ReadFile("../../../.github/workflows/opencode-contract.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(wb)
	var matrix []string
	if m := regexp.MustCompile(`opencode: \[([^\]]*)\]`).FindStringSubmatch(wf); m != nil {
		for _, v := range strings.Split(m[1], ",") {
			matrix = append(matrix, strings.Trim(v, ` "'`))
		}
	}
	if !slices.Equal(matrix, names) {
		t.Errorf("workflow matrix opencode: %q, want the pinned versions %q", matrix, names)
	}
	if want := `OPENCODE_NEWEST: "` + hi.String() + `"`; !strings.Contains(wf, want) {
		t.Errorf("workflow lacks %s", want)
	}
	// The trigger paths, read from on.pull_request.paths only (a path in a comment or a step triggers
	// nothing): everything a contract test checks. worker.go and cli.go hold the worker's
	// containment and Detach, which only T7 exercises.
	var paths []string
	if _, rest, ok := strings.Cut(wf, "\n  pull_request:"); ok {
		if _, rest, ok = strings.Cut(rest, "\n    paths:\n"); ok {
			for _, l := range strings.Split(rest, "\n") {
				if l = strings.TrimSpace(l); strings.HasPrefix(l, "#") {
					continue
				}
				p, ok := strings.CutPrefix(l, "- ")
				if !ok {
					break
				}
				paths = append(paths, strings.Trim(p, `"`))
			}
		}
	}
	for _, p := range []string{"internal/platform/opencode/**", "npm/opencode/**", "internal/proc/**",
		"cmd/commands/worker.go", "cmd/commands/cli.go", "scripts/opencode-contract/**", ".github/workflows/opencode-contract.yml"} {
		if !slices.Contains(paths, p) {
			t.Errorf("workflow on.pull_request.paths %q lacks %q", paths, p)
		}
	}
	// Every exact ^…$ test target (the curl and desktop legs' -run, T11's step and -skip) must name a
	// contract test: a -run that matches nothing passes, so a renamed test would leave its leg
	// green while it tests nothing.
	files, _ := filepath.Glob("contract_*_test.go")
	var src strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src.Write(b)
	}
	targets := regexp.MustCompile(`\^(TestContract\w+)\$`).FindAllStringSubmatch(wf, -1)
	if len(targets) == 0 {
		t.Error("workflow has no ^TestContract…$ target: this check or the workflow drifted")
	}
	for _, m := range targets {
		if !strings.Contains(src.String(), "func "+m[1]+"(") {
			t.Errorf("workflow runs ^%s$, but no contract_*_test.go defines func %s(", m[1], m[1])
		}
	}
	// Under it the runner refuses and every contract test fails (requireContract).
	if strings.Contains(wf, platform.DisableExternalRunnersEnv) {
		t.Errorf("workflow mentions %s", platform.DisableExternalRunnersEnv)
	}
}
