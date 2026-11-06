//go:build !windows

package commands

import "testing"

// A browser opens only where there is a desktop: macOS always, elsewhere only with an X11 or
// Wayland display — xdg-open on a headless box would fall back to a terminal browser or fail.
func TestBrowserCommand(t *testing.T) {
	for _, c := range []struct {
		goos, display, wayland, want string
	}{
		{"darwin", "", "", "open"},
		{"linux", "", "", ""},
		{"linux", ":0", "", "xdg-open"},
		{"linux", "", "wayland-0", "xdg-open"},
		{"freebsd", ":0", "", "xdg-open"},
	} {
		env := map[string]string{"DISPLAY": c.display, "WAYLAND_DISPLAY": c.wayland}
		if got := browserCommand(c.goos, func(k string) string { return env[k] }); got != c.want {
			t.Errorf("browserCommand(%s, DISPLAY=%q WAYLAND_DISPLAY=%q) = %q, want %q",
				c.goos, c.display, c.wayland, got, c.want)
		}
	}
}
