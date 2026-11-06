//go:build windows

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The probe's decode is the one part no portable test can reach, so read THIS test process
// through the same calls the reaper uses and compare with what the runtime already knows. Nothing
// is spawned. A wrong UNICODE_STRING decode or a refused handle fails here instead of silently
// turning gate 2 into "never ours" — the way the reaper has gone dead twice before.
func TestServeProbeReadsThisProcess(t *testing.T) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("OpenProcess(self): %v", err)
	}
	defer windows.CloseHandle(h)

	got, err := processCommandLine(h)
	if err != nil {
		t.Fatalf("processCommandLine(self): %v", err)
	}
	if want := windows.UTF16PtrToString(windows.GetCommandLine()); got != want {
		t.Errorf("processCommandLine(self) = %q, want %q", got, want)
	}

	img, err := processImageName(h)
	if err != nil {
		t.Fatalf("processImageName(self): %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Base(img), filepath.Base(exe)) {
		t.Errorf("processImageName(self) = %q, want the test binary %q", img, exe)
	}
}

// fakeServeEnv tells a re-run of this test binary to play the serve in
// TestIsOurServePIDRecognisesALiveServe.
const fakeServeEnv = "WITNESS_TEST_FAKE_OPENCODE_SERVE"

// TestFakeServeHelper is not a test of its own: it is the child the test below starts, and it only
// has to stay alive (bounded, in case the parent dies before its cleanup) while it is probed.
func TestFakeServeHelper(t *testing.T) {
	if os.Getenv(fakeServeEnv) != "1" {
		t.Skip("helper process for TestIsOurServePIDRecognisesALiveServe")
	}
	time.Sleep(time.Minute)
}

// Every other check of the reaper asks for the "not ours" answer, and "never ours" is how it has
// gone dead twice. So start a process shaped like a witness serve and require isOurServePID to
// claim it: this test binary copied to opencode.exe (copying is fine on Windows) and given the
// serve's flags. The directory has a space, so argv[0] arrives quoted and the quoted-executable
// fingerprint is checked end to end. The real opencode is never run.
func TestIsOurServePIDRecognisesALiveServe(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "John Smith")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "opencode.exe")
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, "-test.run=^TestFakeServeHelper$",
		"serve", "--pure", "--hostname", "127.0.0.1", "--port", "1", "--log-level", "ERROR")
	cmd.Env = append(os.Environ(), fakeServeEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir, so it runs first: Windows cannot delete a running exe.
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	if !isOurServePID(cmd.Process.Pid) {
		t.Fatalf("isOurServePID(%d) = false for a live %q serve", cmd.Process.Pid, exe)
	}
}
