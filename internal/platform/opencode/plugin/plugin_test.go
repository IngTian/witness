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
	if !strings.Contains(body, `type.startsWith("message.")`) || !strings.Contains(body, "clearQuietTimer()") {
		t.Fatal("embedded plugin should treat message updates as activity without importing them")
	}
	if !strings.Contains(body, `const args = ["import", "--agent", "opencode", "--quiet", "--no-kick"]`) || strings.Contains(body, `"import", "--agent", "opencode", "--quiet", "--auto"`) {
		t.Fatal("embedded plugin should reconcile OpenCode sessions through L0-only import")
	}
	if !strings.Contains(body, "sync()") || !strings.Contains(body, `type === "session.idle"`) || !strings.Contains(body, `type === "session.status"`) || !strings.Contains(body, `status?.type === "idle"`) {
		t.Fatal("embedded plugin should reconcile on init and both idle event forms")
	}
	if !strings.Contains(body, "const pendingSessions = new Set()") || !strings.Contains(body, "const sessionWaiters = new Map()") || !strings.Contains(body, "const batchWaiters = claimWaiters(coveredSessions)") || !strings.Contains(body, "const idleCycles = new Map()") || !strings.Contains(body, "let activeImport = null") || !strings.Contains(body, "drain()") {
		t.Fatal("embedded plugin should serialize, wait for, and deduplicate idle imports")
	}
	if !strings.Contains(body, "const IMPORT_GRACE_MS = 5000") || !strings.Contains(body, "let disposing = false") || !strings.Contains(body, "waitForIdle()") {
		t.Fatal("embedded plugin should drain imports gracefully before disposal")
	}
	if !strings.Contains(body, "const QUIET_PERIOD_MS = 5 * 60 * 1000") || !strings.Contains(body, "scheduleQuietWorker()") || !strings.Contains(body, `spawnWitness(["worker-run", "--auto"])`) || !strings.Contains(body, "clearQuietTimer()") {
		t.Fatal("embedded plugin should start one auto worker after a resettable quiet period")
	}
	if !strings.Contains(body, `spawnWitness(["worker", "stop", "--auto-only"])`) {
		t.Fatal("embedded plugin should stop only auto workers on disposal")
	}
}
