package commands

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

// Runner choices in help and hints come from the registry (#199 F005): a hand-typed
// <claude|opencode> is right only until a runner is added or dropped. A fake runner is
// registered first, so a call site that still types the list by hand visibly misses it.
func TestRunnerChoiceListsTheRegisteredRunnersAtEverySite(t *testing.T) {
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	choice := runnerChoice()
	names := strings.Split(strings.TrimSuffix(strings.TrimPrefix(choice, "<"), ">"), "|")
	for _, n := range []string{"claude", "opencode", "fakeleak", "codex", "file"} {
		listed, runs := slices.Contains(names, n), platform.ValidateRunnerName(n) == nil
		if listed != runs {
			t.Errorf("runnerChoice() = %s: %q listed=%v, but it can run a model=%v", choice, n, listed, runs)
		}
	}

	set := "witness config set runner " + choice
	if !strings.Contains(newWireCmd().Long, set) {
		t.Errorf("wire --help does not offer %q:\n%s", set, newWireCmd().Long)
	}
	if !strings.Contains(newConfigCmd().Long, "distillation runtime: "+choice) {
		t.Errorf("config --help does not list %s:\n%s", choice, newConfigCmd().Long)
	}

	readFixture(t)
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no model: the embedder check fails fast
	t.Setenv("PATH", t.TempDir())           // and no runner binary to probe
	if out, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) }); !strings.Contains(out, set) {
		t.Errorf("doctor does not hint %q:\n%s", set, out)
	}

	const venue = "fakerunnerchoice"
	called := false
	registerFakeCaptureOnlyEditor(t, venue, &called)
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "w"))
	if out, err := captureStdoutErr(t, func() error { return cmdInstall([]string{venue}) }); err != nil || !strings.Contains(out, set) {
		t.Errorf("wire's result does not hint %q (err %v):\n%s", set, err, out)
	}
}
