package file

import (
	"context"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Platform is the "file" source adapter (issue #44): records pushed in via
// `witness ingest`, not pulled from a native store. Sessions are namespaced with the
// "file:" prefix. The whole engine (mine→review→summarize→serve) treats a file session
// exactly like any other — this adapter only supplies identity + the shared renderer.
type Platform struct{}

func init() { platform.Register(Platform{}) }

func (Platform) Name() string { return "file" }

// SessionPrefix namespaces ingested sessions in L0 so they never collide with
// claude: / opencode: (or the unmarked==claude default).
func (Platform) SessionPrefix() string { return "file:" }

// RenderInputs shapes the session with the shared, source-agnostic policy — identical
// to the Claude/OpenCode adapters. A file record's Text is flat prose.
func (Platform) RenderInputs(raw []store.RawRecord, policy platform.ChunkPolicy) []string {
	return platform.RenderChunks(raw, policy)
}

// Import is a no-op: ingest is PUSH (the `witness ingest` command writes L0 directly),
// not a PULL-by-session-id reconcile like OpenCode. The interface is satisfied trivially.
func (Platform) Import(context.Context, store.ImportStore, []string) (platform.ImportStats, error) {
	return platform.ImportStats{Agent: "file"}, nil
}
