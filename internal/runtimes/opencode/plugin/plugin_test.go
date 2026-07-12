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
	if strings.Contains(body, "message.updated") {
		t.Fatal("embedded plugin should not capture message.updated events anymore")
	}
	if !strings.Contains(body, `spawnWitness(["import", "--agent", "opencode", "--quiet", "--auto"])`) {
		t.Fatal("embedded plugin should reconcile OpenCode sessions through import")
	}
	if !strings.Contains(body, "syncOpenCode()") || !strings.Contains(body, `type === "session.idle"`) {
		t.Fatal("embedded plugin should reconcile on init and session.idle")
	}
}
