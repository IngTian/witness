package claude

import (
	"context"
	"encoding/json"
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

// RenderInputs shapes the session by the shared, source-agnostic policy. Claude is
// hook-fed flat text (no structured parts) and was ALWAYS sent whole; that is still
// the default (policy.MaxChars <= 0), and the measured #57 conclusion says it should
// be — whole-session mining is what preserves arc rules. The only behavior change is
// that a Claude session now ALSO honors a positive chunk budget, so a user with a
// giant session that times out whole can opt into the same last-resort split OpenCode
// gets, instead of the CC path being hard-wired to one oversized call (#56 B1).
func (Platform) RenderInputs(raw []store.RawRecord, policy platform.ChunkPolicy) []string {
	return platform.RenderChunks(raw, policy)
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
func (Platform) Import(context.Context, *store.Store, []string) (platform.ImportStats, error) {
	return platform.ImportStats{Agent: "claude"}, nil
}
