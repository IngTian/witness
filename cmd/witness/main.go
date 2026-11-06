// Command witness is a distillation engine that keeps the history of how things
// changed. It captures Claude Code, OpenCode and Codex sessions, plus any files
// you ingest, and distills them into a dated record of how their subject changed
// (raw turns → observations → facets → a narrative profile), with provenance. One
// pure-Go binary: a local SQLite archive plus plain markdown, served over MCP.
//
// This is only the entry point. The CLI lives in
// github.com/IngTian/witness/cmd/commands, one file per cobra command, and so does
// the build identity: the Makefile's -ldflags set commands.version, commit and
// buildTime.
package main

import (
	"os"

	"github.com/IngTian/witness/cmd/commands"
)

func main() {
	os.Exit(commands.Run())
}
