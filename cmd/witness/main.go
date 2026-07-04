// Command witness is the single self-contained binary behind the claude-witness
// plugin. The CLI surface lives in github.com/IngTian/claude-witness/cmd/commands
// — one file per cobra command — so this file stays minimal: it only wires
// build-time identity and delegates to commands.Run.
package main

import (
	"os"

	"github.com/IngTian/claude-witness/cmd/commands"
)

func main() {
	os.Exit(commands.Run())
}
