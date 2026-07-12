// Package platform holds the agent-runtime adapters (Claude Code, OpenCode) and
// the shared identity/stats types. Core witness code depends on these small
// boundaries instead of embedding Claude Code or OpenCode details throughout the
// CLI. The per-runtime implementations live in the claude/ and opencode/
// subpackages; this file holds the cross-runtime constants and value types.
package platform

const (
	AgentClaude   = "claude"
	AgentOpenCode = "opencode"
)

type ImportStats struct {
	Agent      string
	Sessions   int
	Records    int
	MaxUpdated int64
}
