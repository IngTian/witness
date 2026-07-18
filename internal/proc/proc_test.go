package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// errStub is a sentinel error returned by the Fake in the GracefulStop test.
var errStub = errors.New("stub error")

func TestPsField(t *testing.T) {
	cases := []struct {
		line     string
		wantN    int
		wantRest string
		wantOK   bool
	}{
		{"  4321   1 opencode serve --pure", 4321, "1 opencode serve --pure", true},
		{"1 opencode serve", 1, "opencode serve", true},
		{"\t99\tclaude -p", 99, "claude -p", true},
		{"notanumber rest", 0, "", false},
		{"", 0, "", false},
		{"12345", 0, "", false}, // no command column
	}
	for _, tc := range cases {
		n, rest, ok := psField(tc.line)
		if ok != tc.wantOK || n != tc.wantN || rest != tc.wantRest {
			t.Fatalf("psField(%q) = (%d, %q, %v), want (%d, %q, %v)", tc.line, n, rest, ok, tc.wantN, tc.wantRest, tc.wantOK)
		}
	}
}

// TestOrphanPIDs is the core safety test for the reap primitive: only an ORPHANED
// process (ppid==1) whose command satisfies the predicate may be selected. A live
// sibling (ppid!=1 — still owned by its launcher), a non-matching process, the
// scanner's own pid, and init itself must all be skipped. The predicate here stands
// in for a real fingerprint (opencode owns its own; see TestIsStrayServeLine there).
func TestOrphanPIDs(t *testing.T) {
	const self = 700
	want := func(cmdline string) bool { return strings.Contains(cmdline, "MATCH") }
	psOut := strings.Join([]string{
		"  4321     1 /bin/thing MATCH orphan",   // orphan + match → reap
		"  4400  4399 /bin/thing MATCH sibling",  // LIVE sibling (ppid!=1) → skip
		"  4500     1 /bin/other no-fingerprint", // orphan but no match → skip
		"   700     1 /bin/thing MATCH self",     // self → skip
		"     1     0 /sbin/launchd",             // init → skip
		"  garbage line that does not parse",     // skip
	}, "\n")

	got := OrphanPIDs(psOut, self, want)
	if wantPIDs := []int{4321}; !slices.Equal(got, wantPIDs) {
		t.Fatalf("OrphanPIDs = %v, want %v", got, wantPIDs)
	}
}

// TestFakeDrivesWithoutSyscalls proves a caller can be exercised against the port
// with the Fake and no real OS process is touched: the cmd is configured but never
// started, terminate/reap/notify are recorded, and the reap predicate is applied to
// a synthetic ps fixture through the shared OrphanPIDs primitive.
func TestFakeDrivesWithoutSyscalls(t *testing.T) {
	var ctl Control = &Fake{
		ReapPS: strings.Join([]string{
			"  900   1 /bin/x FINGERPRINT orphan",
			"  901 900 /bin/x FINGERPRINT live",
		}, "\n"),
		TerminateErr: nil,
	}
	f := ctl.(*Fake)

	cmd := exec.Command("true")
	ctl.Detach(cmd)
	ctl.BindToParent(cmd)
	if err := ctl.TerminateGroup(4242); err != nil {
		t.Fatalf("TerminateGroup: %v", err)
	}
	ctl.ReapOrphans(func(c string) bool { return strings.Contains(c, "FINGERPRINT") })
	ctx, stop := ctl.NotifyStop(context.Background())
	defer stop()
	if ctx.Err() != nil {
		t.Fatalf("NotifyStop ctx should start live, got %v", ctx.Err())
	}

	if len(f.Detached) != 1 || f.Detached[0] != cmd {
		t.Fatalf("Detach not recorded: %+v", f.Detached)
	}
	if len(f.Bound) != 1 || f.Bound[0] != cmd {
		t.Fatalf("BindToParent not recorded: %+v", f.Bound)
	}
	if !slices.Equal(f.Terminated, []int{4242}) {
		t.Fatalf("Terminated = %v", f.Terminated)
	}
	if f.ReapCalls != 1 || f.ReapPredicate == nil {
		t.Fatalf("ReapOrphans not recorded: calls=%d pred=%v", f.ReapCalls, f.ReapPredicate != nil)
	}
	// Only the orphan (ppid==1) matching the fingerprint is selected; the live
	// sibling (ppid=900) is excluded by the orphan gate.
	if !slices.Equal(f.ReapSelected, []int{900}) {
		t.Fatalf("ReapSelected = %v, want [900]", f.ReapSelected)
	}
	if f.NotifyStops != 1 {
		t.Fatalf("NotifyStops = %d", f.NotifyStops)
	}
	if cmd.Process != nil {
		t.Fatalf("cmd should never have been started by the fake")
	}
}

// TestFakeGracefulStopRecords proves the Fake records GracefulStop calls (including a
// nil process, the no-op path) and surfaces GracefulStopErr, without touching a real
// process. This lets a caller (e.g. OpenCodeServer.Close) be tested for "did it route
// the graceful stop through the port?".
func TestFakeGracefulStopRecords(t *testing.T) {
	f := &Fake{GracefulStopErr: errStub}
	var ctl Control = f
	if err := ctl.GracefulStop(nil); err != errStub {
		t.Fatalf("GracefulStop err = %v, want errStub", err)
	}
	p := &os.Process{Pid: 1234}
	_ = ctl.GracefulStop(p)
	if len(f.GracefulStops) != 2 {
		t.Fatalf("GracefulStops = %d, want 2", len(f.GracefulStops))
	}
	if f.GracefulStops[0] != nil {
		t.Fatalf("first GracefulStop should record nil (no-op path), got %v", f.GracefulStops[0])
	}
	if f.GracefulStops[1] != p {
		t.Fatalf("second GracefulStop should record the process, got %v", f.GracefulStops[1])
	}
}

// TestSysGracefulStopNilIsNoop confirms the real controller treats a nil process as a
// no-op (never panics), matching the callers that hold an optional child handle.
func TestSysGracefulStopNilIsNoop(t *testing.T) {
	if err := System().GracefulStop(nil); err != nil {
		t.Fatalf("GracefulStop(nil) = %v, want nil", err)
	}
}
