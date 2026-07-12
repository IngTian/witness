package claude

import (
	"context"
	"encoding/json"
	"strings"
	"time"

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

// Capture unmarshals the Claude Code hook payload and writes one L0 record. The
// []byte signature is the uniform Capturer contract; the typed HookEvent is an
// internal detail. On a successful write it stamps the session's owning platform
// so ForSession is column-authoritative for it. Best-effort: a malformed payload
// is not an error (returns ok=false) — capture must never break a session.
func (Platform) Capture(st *store.Store, data []byte, now time.Time) (bool, error) {
	var e HookEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return false, err
	}
	if err := Capture(st, e, now); err != nil {
		return false, err
	}
	if e.SessionID != "" {
		st.SetSessionPlatform(e.SessionID, "claude")
	}
	return true, nil
}

// Import is a no-op: Claude Code is hook-fed (capture writes L0 live), so there is
// no external native store to reconcile from.
func (Platform) Import(context.Context, *store.Store) (platform.ImportStats, error) {
	return platform.ImportStats{Agent: "claude"}, nil
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
