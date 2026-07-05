package distill

import (
	"strings"

	"github.com/IngTian/witness/internal/store"
)

const (
	openCodeSessionPrefix       = "opencode:"
	openCodeChunkOverlapRecords = 2
)

// Tuned as a character budget rather than a token budget because the local input
// model is plain text. Tests may lower it; production keeps chunks comfortably
// below long-context timeout territory while preserving several turns together.
var openCodeChunkMaxChars = 24_000

// distillInputs is the source-specific rendering seam for L0 -> model input.
// Claude Code currently only gives witness flattened hook text, so its legacy
// single-transcript behavior is preserved. OpenCode has structured message parts;
// its importer/capture path admits only natural-language text parts, and this
// renderer chunks long sessions so command/file outputs omitted upstream do not
// force a single huge model call.
func distillInputs(session string, raw []store.RawRecord) []string {
	if strings.HasPrefix(session, openCodeSessionPrefix) {
		return renderOpenCodeChunks(raw, openCodeChunkMaxChars, openCodeChunkOverlapRecords)
	}
	return []string{renderTranscript(raw)}
}

func renderOpenCodeChunks(raw []store.RawRecord, maxChars, overlapRecords int) []string {
	if len(raw) == 0 {
		return nil
	}
	if maxChars <= 0 {
		return []string{renderTranscript(raw)}
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
		chunks = append(chunks, renderTranscript(raw[start:end]))
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
