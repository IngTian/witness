package plugin

import (
	"strings"
	"testing"
)

func TestBodyExportsDefaultPlugin(t *testing.T) {
	body := Body()
	if !strings.Contains(body, "export default plugin") {
		t.Fatal("OpenCode package loading expects a default plugin export")
	}
	if !strings.Contains(body, "export const Witness = plugin") || !strings.Contains(body, "export const ClaudeWitness = plugin") {
		t.Fatal("named plugin exports should remain available for local/plugin-name loading")
	}
}
