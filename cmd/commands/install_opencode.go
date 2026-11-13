package commands

import (
	"context"
	"errors"

	"github.com/IngTian/witness/internal/platform/opencode"
)

const openCodePluginName = opencode.PluginFileName

// opencodeDir is where `wire opencode` writes the plugin and MCP entry. It is OpenCode's own
// lookup, opencode.ConfigDir — the one the auth probe uses too — rather than a copy here: the copy
// that lived here skipped XDG_CONFIG_HOME, so with XDG set the wiring landed where OpenCode never
// reads and still reported success.
//
// This file exists for that one import, the same category as install_codex.go: the engine fence
// (TestEngineHasNoPlatformDispatch) exempts a venue's adapter file by name, not the whole tree.
func opencodeDir() string { return opencode.ConfigDir() }

// openCodeV2Check refuses `wire opencode` when the installed OpenCode is v2 (#218). v2 never
// loads witness's plugin, so wiring it reported success while nothing was captured — issue
// #10's failure shape. Only v2 blocks: an absent or older OpenCode wires as it always has (the
// user may install it afterwards, and the runner gate reports an old one at distill time). A
// var so tests never spawn the user's real `opencode`.
var openCodeV2Check = func() error {
	if err := opencode.ValidateOpenCodeCapability(context.Background(), ""); errors.Is(err, opencode.ErrOpenCodeV2) {
		return err
	}
	return nil
}
