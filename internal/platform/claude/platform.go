package claude

import (
	"strings"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Platform is the Claude Code runtime adapter's registry face (issue #21). Claude
// sessions are hook-fed and unprefixed; the whole session is one flat transcript.
type Platform struct{}

func init() { platform.Register(Platform{}) }

func (Platform) Name() string { return "claude" }

// SessionPrefix is empty: Claude Code session ids are stored unprefixed, and an
// unprefixed session is the default/unmarked source. Keep this "" — the
// asymmetric "unmarked == Claude" rule (ForSession's default) depends on it.
func (Platform) SessionPrefix() string { return "" }

// RenderInputs flattens the whole session into a single transcript. Claude only
// gives witness flattened hook text (no structured parts), so there is nothing to
// chunk — one input per session.
func (Platform) RenderInputs(raw []store.RawRecord) []string {
	return []string{RenderTranscript(raw)}
}

// RenderTranscript renders raw records as "ROLE: text\n\n". Exported because it is
// the shared, source-neutral transcript format the OpenCode chunker also emits per
// chunk (both feed the same mining prompt), so it lives on the default platform
// rather than being duplicated.
func RenderTranscript(raw []store.RawRecord) string {
	var b strings.Builder
	for _, r := range raw {
		b.WriteString(strings.ToUpper(r.Role))
		b.WriteString(": ")
		b.WriteString(r.Text)
		b.WriteString("\n\n")
	}
	return b.String()
}
