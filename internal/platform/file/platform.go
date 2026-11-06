package file

import "github.com/IngTian/witness/internal/platform"

// Platform is the "file" source adapter (issue #44): records pushed in via
// `witness ingest`, not pulled from a native store. Sessions are namespaced with the
// "file:" prefix. The whole engine (mine→review→summarize→serve) treats a file session
// exactly like any other — this adapter supplies identity only. It has no Import:
// ingest is PUSH (the `witness ingest` command writes L0 directly), and its flat-prose
// records are shaped by the shared RenderChunks.
type Platform struct{}

func init() { platform.Register(Platform{}) }

func (Platform) Name() string { return "file" }

// SessionPrefix namespaces ingested sessions in L0 so they never collide with
// claude: / opencode: (or the unmarked==claude default).
func (Platform) SessionPrefix() string { return "file:" }
