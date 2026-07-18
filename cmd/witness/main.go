// Command witness is a local memory & self-improvement engine for Claude Code
// and OpenCode. It captures your coding sessions and distills how your patterns,
// habits, and knowledge evolve over time — a person-centric growth archive with
// provenance, served over an MCP server plus plain files, as a single pure-Go
// binary. Think second brain / AI memory for how you think and grow, not project
// memory for what your code did. (File & PDF ingestion is on the roadmap, so the
// same distillation engine can track how knowledge evolves across notes and
// documents too.)
//
// This is the entry point: the single self-contained binary behind the witness
// plugin. The CLI surface lives in github.com/IngTian/witness/cmd/commands — one
// file per cobra command — so this file stays minimal: it only wires build-time
// identity and delegates to commands.Run.
package main

import (
	"os"

	"github.com/IngTian/witness/cmd/commands"
)

func main() {
	os.Exit(commands.Run())
}
