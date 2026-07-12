package opencode

import (
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/platform/claude"
	"github.com/IngTian/witness/internal/store"
)

// SessionPrefix (the "opencode:" L0 id namespace) is defined in import.go — the
// single source of truth. The former distill.openCodeSessionPrefix duplicate is
// gone; distill now resolves the prefix through this platform via ForSession.

const chunkOverlapRecords = 2

// chunkMaxChars is a character budget (the local input model is plain text, not
// token-metered). A var so tests can lower it; production keeps chunks well below
// long-context timeout territory while preserving several turns together.
var chunkMaxChars = 24_000

// SetChunkMaxCharsForTest overrides the chunk budget and returns a restore func.
// Exported for cross-package tests (the distill worker exercises chunking end to
// end); production never calls it.
func SetChunkMaxCharsForTest(n int) (restore func()) {
	old := chunkMaxChars
	chunkMaxChars = n
	return func() { chunkMaxChars = old }
}

// Platform is the OpenCode runtime adapter's registry face (issue #21). OpenCode
// sessions are prefixed and can carry long structured logs, so mining input is
// chunked rather than sent as one huge call.
type Platform struct{}

func init() { platform.Register(Platform{}) }

func (Platform) Name() string { return "opencode" }

func (Platform) SessionPrefix() string { return SessionPrefix }

// RenderInputs chunks the session into overlapping transcripts so a long session
// (whose command/file outputs were filtered upstream but whose remaining text is
// still large) does not force a single oversized model call.
func (Platform) RenderInputs(raw []store.RawRecord) []string {
	return renderChunks(raw, chunkMaxChars, chunkOverlapRecords)
}

// renderChunks splits raw into overlapping windows under maxChars, each rendered
// with the shared transcript format. Moved verbatim from distill/input.go
// (renderOpenCodeChunks); behavior is unchanged.
func renderChunks(raw []store.RawRecord, maxChars, overlapRecords int) []string {
	if len(raw) == 0 {
		return nil
	}
	if maxChars <= 0 {
		return []string{claude.RenderTranscript(raw)}
	}
	var chunks []string
	start := 0
	for start < len(raw) {
		end := start
		chars := 0
		for end < len(raw) {
			entryChars := len(raw[end].Role) + len(raw[end].Text) + 4
			if end > start && chars+entryChars > maxChars {
				break
			}
			chars += entryChars
			end++
		}
		if end == start {
			end++
		}
		chunks = append(chunks, claude.RenderTranscript(raw[start:end]))
		if end >= len(raw) {
			break
		}
		next := end
		if overlapRecords > 0 && end-start > overlapRecords {
			next = end - overlapRecords
		}
		if next <= start {
			next = end
		}
		start = next
	}
	return chunks
}
