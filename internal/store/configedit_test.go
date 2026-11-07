package store

import (
	"os"
	"testing"
)

// Every config writer goes through editConfigLines, which leaves the file alone when the
// edit changes nothing: re-setting a key to its value, disabling a lens that is not enabled
// and removing an absent key never rewrite config.toml, and removing never creates it.
func TestConfigWritersSkipNoOpRewrites(t *testing.T) {
	s := tempStore(t)
	if err := s.SetConfigString("triage_model", "m"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for name, write := range map[string]func() error{
		"set same value": func() error { return s.SetConfigString("triage_model", "m") },
		"disable absent": func() error { return s.DisableLens("never-enabled") },
		"remove absent":  func() error { return s.removeConfigKey("no_such_key") },
	} {
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after, err := os.Stat(s.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Errorf("%s rewrote config.toml though nothing changed", name)
		}
	}

	if err := os.Remove(s.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	for name, write := range map[string]func() error{
		"remove":  func() error { return s.removeConfigKey("runner") },
		"disable": func() error { return s.DisableLens("x") },
	} {
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(s.ConfigPath()); !os.IsNotExist(err) {
			t.Errorf("%s on a missing config.toml created it (stat err %v)", name, err)
		}
	}
}
