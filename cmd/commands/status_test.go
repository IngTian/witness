// cmd/commands/status_test.go
package commands

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

func TestStatusCmdShape(t *testing.T) {
	c := newStatusCmd()
	if c.Use != "status" {
		t.Errorf("Use = %q, want status", c.Use)
	}
	if c.Hidden {
		t.Error("status must be a visible top-level command")
	}
	if f := c.Flags().Lookup("json"); f == nil {
		t.Error("status must have a --json flag")
	}
}

// While the free-model pause is armed, status says so in one factual row and --json carries the
// reset time; otherwise neither appears, so an archive that never used a free model reads as before.
func TestStatusShowsAnArmedFreeModelPause(t *testing.T) {
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	for name, tc := range map[string]struct {
		stored string
		armed  bool
	}{
		"armed":   {until.Format(time.RFC3339), true},
		"past":    {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), false},
		"unarmed": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WITNESS_HOME", t.TempDir())
			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetMetaString(freeModelsPausedKey, tc.stored); err != nil {
				t.Fatal(err)
			}
			st.Close()

			human := captureStdout(t, func() { _ = cmdDistillStatus(false) })
			row := "paused until " + until.Local().Format("2006-01-02 15:04") + " (the free allowance resets then)"
			if got := strings.Contains(human, "free models"); got != tc.armed || (tc.armed && !strings.Contains(human, row)) {
				t.Errorf("free-models row shown = %v, want %v reading %q:\n%s", got, tc.armed, row, human)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(captureStdout(t, func() { _ = cmdDistillStatus(true) })), &out); err != nil {
				t.Fatal(err)
			}
			got, present := out[freeModelsPausedKey]
			if present != tc.armed || (tc.armed && got != until.Format(time.RFC3339)) {
				t.Errorf("--json %s = %v (present %v), want present %v with %s", freeModelsPausedKey, got, present, tc.armed, until.Format(time.RFC3339))
			}
		})
	}
}
