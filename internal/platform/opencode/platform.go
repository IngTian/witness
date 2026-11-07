package opencode

import (
	"context"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// SessionPrefix (the "opencode:" L0 id namespace) is defined in import.go — the
// single source of truth. The former distill.openCodeSessionPrefix duplicate is
// gone; distill now resolves the prefix through this platform via ForSession.

// Platform is the OpenCode runtime adapter's registry face (issue #21). OpenCode
// sessions are prefixed and can carry long structured logs.
type Platform struct{}

func init() { platform.Register(Platform{}) }

// Capabilities are discovered by type assertion, so a signature that drifts would silently
// drop one (an Import that no longer matches imports nothing and reports "up to date").
// These make that a compile error instead.
var (
	_ platform.Importer            = Platform{}
	_ platform.RunnerProvider      = Platform{}
	_ platform.NativeSessionSource = Platform{}
)

func (Platform) Name() string { return "opencode" }

func (Platform) SessionPrefix() string { return SessionPrefix }

// SupportsNativeSessions marks OpenCode as a platform whose sessions live in its own conversation
// store, so a runner may retain a scratch context keyed to one and resume it after a crash. This is
// the platform.NativeSessionSource capability — it replaces an `== platform.AgentOpenCode` name
// comparison the engine used to make on its own.
func (Platform) SupportsNativeSessions() bool { return true }

// Import reconciles OpenCode's SQLite store into L0. It takes the sync lock INSIDE
// the method (so cmd need not know about it). A held lock means another import is
// in flight — return zero stats, not an error.
func (Platform) Import(ctx context.Context, st store.ImportStore, sessionIDs []string) (platform.ImportStats, error) {
	// Same lock file as before (".opencode-sync.lock"); the store no longer names
	// the platform — this package owns the "opencode" key.
	unlock, ok := st.ImportLock("opencode")
	if !ok {
		return platform.ImportStats{Agent: "opencode"}, nil
	}
	defer unlock()
	return (&Importer{Store: st}).Import(ctx, sessionIDs)
}
