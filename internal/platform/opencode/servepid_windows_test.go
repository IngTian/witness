//go:build windows

package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
