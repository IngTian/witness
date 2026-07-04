package plugin

import (
	_ "embed"
	"encoding/json"
)

//go:embed claude-witness.js
var body string

// Source builds the installed OpenCode plugin by prepending the absolute witness
// shim path. Keep the JavaScript body in claude-witness.js as the single source.
func Source(shim string) string {
	shimJSON, _ := json.Marshal(shim)
	return "globalThis.WITNESS_SHIM = " + string(shimJSON) + "\n\n" + body
}

// Body returns the runtime-agnostic plugin JavaScript body for tests.
func Body() string { return body }
