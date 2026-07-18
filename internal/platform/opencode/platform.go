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

func (Platform) Name() string { return "opencode" }

func (Platform) SessionPrefix() string { return SessionPrefix }

// RenderInputs shapes the session by the shared, source-agnostic policy: whole by
// default (policy.MaxChars <= 0), split into overlapping windows only when a session
// overflows a positive budget. OpenCode used to chunk UNCONDITIONALLY at a fixed 24K
// budget; the #57 measurements showed that structurally under-extracts long arc-heavy
// sessions (~70% recall loss, 20× drift) — the root of the "OpenCode quality is low"
// report (#56 B1). It now rides the same platform.RenderChunks as Claude, so both
// runtimes shape identically and only a giant session is ever split.
func (Platform) RenderInputs(raw []store.RawRecord, policy platform.ChunkPolicy) []string {
	return platform.RenderChunks(raw, policy)
}

// Import reconciles OpenCode's SQLite store into L0. It takes the sync lock INSIDE
// the method (so cmd need not know about it) and maps the internal stats onto the
// shared platform.ImportStats. A held lock means another import is in flight —
// return zero stats, not an error.
func (Platform) Import(ctx context.Context, st store.ImportStore, sessionIDs []string) (platform.ImportStats, error) {
	// Same lock file as before (".opencode-sync.lock"); the store no longer names
	// the platform — this package owns the "opencode" key.
	unlock, ok := st.ImportLock("opencode")
	if !ok {
		return platform.ImportStats{Agent: "opencode"}, nil
	}
	defer unlock()
	s, err := (&Importer{Store: st}).Import(ctx, sessionIDs)
	if err != nil {
		return platform.ImportStats{Agent: "opencode"}, err
	}
	return platform.ImportStats{
		Agent:      "opencode",
		Sessions:   s.Sessions,
		Records:    s.Records,
		MaxUpdated: s.MaxUpdated,
	}, nil
}
