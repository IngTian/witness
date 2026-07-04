// Package runtimes holds agent-runtime adapters. Core witness code should depend
// on these small boundaries instead of embedding Claude Code or OpenCode details
// throughout the CLI.
package runtimes

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
