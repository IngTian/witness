// cmd/commands/status_test.go
package commands

import "testing"

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
