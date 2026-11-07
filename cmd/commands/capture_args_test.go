package commands

import (
	"os"
	"testing"
)

// Capture must never break a session (AGENTS.md), and that includes a malformed --agent:
// `capture --agent " "` used to exit 1, because cobra's parsed flag was rebuilt into argv and
// re-parsed by a second parser that refused a blank value.
func TestCaptureNeverFailsOnABlankOrUnknownAgent(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv(noDefaultLensEnv, "1")
	for _, agent := range []string{" ", "", "no-such-agent"} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString("{}")
		_ = w.Close()
		prev := os.Stdin
		os.Stdin = r
		c := newInternalCaptureCmd()
		c.SetArgs([]string{"--agent", agent})
		err = c.Execute()
		os.Stdin = prev
		_ = r.Close()
		if err != nil {
			t.Errorf("capture --agent %q returned %v; a hook must always exit 0", agent, err)
		}
	}
}

// worker-run takes flags only; cobra refuses a stray positional instead of a hand parser.
func TestWorkerRunRejectsPositionalArgs(t *testing.T) {
	c := newInternalWorkerCmd()
	c.SetArgs([]string{"stray"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err == nil {
		t.Fatal("worker-run accepted a positional argument")
	}
}
