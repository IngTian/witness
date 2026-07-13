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
	if !strings.Contains(body, `const args = ["import", "--agent", "opencode", "--quiet", "--auto"]`) {
		t.Fatal("embedded plugin should reconcile OpenCode sessions through import")
	}
	if !strings.Contains(body, "sync()") || !strings.Contains(body, `type === "session.idle"`) || !strings.Contains(body, `type === "session.status"`) || !strings.Contains(body, `status?.type === "idle"`) {
		t.Fatal("embedded plugin should reconcile on init and both idle event forms")
	}
	if !strings.Contains(body, "const pendingSessions = new Set()") || !strings.Contains(body, "const sessionWaiters = new Map()") || !strings.Contains(body, "const batchWaiters = claimWaiters(coveredSessions)") || !strings.Contains(body, "const modernIdleWaiters = new Map()") || !strings.Contains(body, "let activeImport = null") || !strings.Contains(body, "drain()") {
		t.Fatal("embedded plugin should serialize, wait for, and deduplicate idle imports")
	}
	if !strings.Contains(body, "const IMPORT_GRACE_MS = 5000") || !strings.Contains(body, "let disposing = false") || !strings.Contains(body, "waitForIdle()") {
		t.Fatal("embedded plugin should drain imports gracefully before disposal")
	}
}
