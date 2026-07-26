package commands

import "testing"

func TestPlumbingCommandsHidden(t *testing.T) {
	if !newImportCmd().Hidden {
		t.Error("import must be hidden (plugin/hook plumbing, not a user verb)")
	}
	obs := newObservationsCmd()
	for _, s := range obs.Commands() {
		switch s.Name() {
		case "record", "delete":
			if !s.Hidden {
				t.Errorf("observations %s must be hidden (MCP-parity, not a human verb)", s.Name())
			}
		case "search":
			if s.Hidden {
				t.Error("observations search must stay visible (human recall)")
			}
		}
	}
}
