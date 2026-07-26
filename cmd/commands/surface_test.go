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

func TestRootSurface(t *testing.T) {
	root := newRootCmd()
	visible := map[string]bool{}
	for _, c := range root.Commands() {
		if !c.Hidden && c.Name() != "help" && c.Name() != "completion" {
			visible[c.Name()] = true
		}
	}
	// present:
	for _, want := range []string{"profile", "status", "lens", "config", "install", "uninstall", "doctor", "export", "cleanup"} {
		if !visible[want] {
			t.Errorf("expected visible command %q", want)
		}
	}
	// gone from the visible front door:
	for _, gone := range []string{"distill", "review", "version", "import"} {
		if visible[gone] {
			t.Errorf("%q must NOT be a visible top-level command anymore", gone)
		}
	}
	// groups registered
	if len(root.Groups()) == 0 {
		t.Error("root should register cobra command groups")
	}
}
