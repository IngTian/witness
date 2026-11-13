package plugin

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed witness.js
var body string

const shimPrefix = "globalThis.WITNESS_SHIM = "

// Source builds the installed OpenCode plugin by prepending the absolute witness
// shim path. Keep the JavaScript body in witness.js as the single source.
func Source(shim string) string {
	shimJSON, _ := json.Marshal(shim)
	return shimPrefix + string(shimJSON) + "\n\n" + body
}

// InstalledShim reads the shim baked into an installed plugin's first line, and current reports
// whether the file is exactly what Source writes for it. So a v1 file left by an older witness,
// or a hand-edited one, is told apart from the current body without re-resolving the install
// target (which on Windows copies the binary).
func InstalledShim(src string) (shim string, current bool) {
	line, _, _ := strings.Cut(src, "\n")
	q, ok := strings.CutPrefix(line, shimPrefix)
	if !ok || json.Unmarshal([]byte(q), &shim) != nil {
		return "", false
	}
	return shim, src == Source(shim)
}
