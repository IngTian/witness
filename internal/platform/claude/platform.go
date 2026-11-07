package claude

import (
	"encoding/json"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Platform is the Claude Code runtime adapter's registry face (issue #21). Claude
// sessions are hook-fed (capture writes L0 live, so there is nothing to Import) and
// unprefixed; they are shaped by the shared RenderChunks.
type Platform struct{}

func init() { platform.Register(Platform{}) }

// Capabilities are discovered by type assertion, so a signature that drifts would silently
// drop one (an Import that no longer matches imports nothing and reports "up to date").
// These make that a compile error instead.
var (
	_ platform.Capturer       = Platform{}
	_ platform.RunnerProvider = Platform{}
)

func (Platform) Name() string { return "claude" }

// SessionPrefix is empty: Claude Code session ids are stored unprefixed, and an
// unprefixed session is the default/unmarked source. Keep this "" — the
// asymmetric "unmarked == Claude" rule (ForSession's default) depends on it.
func (Platform) SessionPrefix() string { return "" }

// Capture unmarshals the Claude Code hook payload and writes one L0 record. The
// []byte signature is the uniform Capturer contract; the typed HookEvent is an
// internal detail. On a successful write it stamps the session's owning platform
// so ForSession is column-authoritative for it. A malformed payload returns its
// decode error, which the caller only logs — capture must never break a session.
func (Platform) Capture(st store.CaptureStore, data []byte, now time.Time) error {
	var e HookEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}
	if err := Capture(st, e, now); err != nil {
		return err
	}
	if e.SessionID != "" {
		st.SetSessionPlatform(e.SessionID, "claude")
	}
	return nil
}
