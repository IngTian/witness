package commands

import "testing"

func TestRenderPrimitivesPlainWhenNoColor(t *testing.T) {
	old := useColor
	useColor = false
	defer func() { useColor = old }()

	if got := header("lenses"); got != "lenses" {
		t.Errorf("header plain = %q, want %q", got, "lenses")
	}
	if got := enabledGlyph(true); got != "*" {
		t.Errorf("enabledGlyph(true) plain = %q, want %q", got, "*")
	}
	if got := enabledGlyph(false); got != "-" {
		t.Errorf("enabledGlyph(false) plain = %q, want %q", got, "-")
	}
	// kvRow pads the name to a fixed width and appends value + note.
	got := kvRow("runner", "opencode", "lens override")
	if want := "runner        opencode (lens override)"; got != want {
		t.Errorf("kvRow plain =\n %q\nwant\n %q", got, want)
	}
	// note omitted when empty
	if got := kvRow("runner", "opencode", ""); got != "runner        opencode" {
		t.Errorf("kvRow no-note plain = %q", got)
	}
	if got := footer("3 lenses"); got != "3 lenses" {
		t.Errorf("footer plain = %q, want %q", got, "3 lenses")
	}
}
