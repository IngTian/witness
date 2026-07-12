package platform_test

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

type fakeInstaller struct{ installed, uninstalled *bool }

func (f fakeInstaller) Install() error   { *f.installed = true; return nil }
func (f fakeInstaller) Uninstall() error { *f.uninstalled = true; return nil }

// The install-target registry dispatches by name (fail-closed on unknown) and
// accepts a third target with no edits to the dispatch site — the seam that lets
// the install command drop its switch and a fake platform be install-able.
func TestInstallerRegistry(t *testing.T) {
	if _, ok := platform.InstallerFor("no-such-target"); ok {
		t.Fatal("unknown target must not resolve (fail-closed)")
	}

	var installed, uninstalled bool
	// A unique name so this test doesn't collide with cmd-registered installers in
	// a combined binary (this is package platform_test, so only fakes exist here).
	platform.RegisterInstaller("faketarget", fakeInstaller{&installed, &uninstalled})

	in, ok := platform.InstallerFor("faketarget")
	if !ok {
		t.Fatal("registered target should resolve")
	}
	if err := in.Install(); err != nil || !installed {
		t.Fatalf("Install not dispatched: err=%v installed=%v", err, installed)
	}
	if err := in.Uninstall(); err != nil || !uninstalled {
		t.Fatalf("Uninstall not dispatched: err=%v uninstalled=%v", err, uninstalled)
	}

	found := false
	for _, name := range platform.InstallTargets() {
		if name == "faketarget" {
			found = true
		}
	}
	if !found {
		t.Fatal("InstallTargets should list a registered target")
	}
}
