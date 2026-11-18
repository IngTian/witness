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

// While a runtime's usage pause is armed, status says so in one factual row and --json carries the
// reset time under runner_usage_paused_until; otherwise neither appears, so an archive that never
// hit a limit reads as before. v0.14.0's free_models_paused_until is stored too and stays unread.
func TestStatusShowsAnArmedUsagePause(t *testing.T) {
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
			for _, key := range []string{usagePauseKey("opencode"), "free_models_paused_until"} {
				if err := st.SetMetaString(key, tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			st.Close()

			human := captureStdout(t, func() { _ = cmdDistillStatus(false) })
			row := "paused until " + until.Local().Format("2006-01-02 15:04") + " (the allowance resets then)"
			if got := strings.Contains(human, "opencode allowance"); got != tc.armed || (tc.armed && !strings.Contains(human, row)) {
				t.Errorf("opencode allowance row shown = %v, want %v reading %q:\n%s", got, tc.armed, row, human)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(captureStdout(t, func() { _ = cmdDistillStatus(true) })), &out); err != nil {
				t.Fatal(err)
			}
			if _, present := out["free_models_paused_until"]; present {
				t.Error("status --json must no longer carry free_models_paused_until")
			}
			pauses, _ := out["runner_usage_paused_until"].(map[string]any) // nil when absent
			got, present := pauses["opencode"]
			if present != tc.armed || (tc.armed && got != until.Format(time.RFC3339)) {
				t.Errorf("--json runner_usage_paused_until.opencode = %v (present %v), want present %v with %s", got, present, tc.armed, until.Format(time.RFC3339))
			}
		})
	}
}
