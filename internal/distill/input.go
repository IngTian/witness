package distill

import (
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// distillInputs is the source-specific L0 -> model-input seam. It resolves the
// session's OWNING platform (persisted session_meta.platform, else id prefix, else
// Claude) and delegates to that platform's InputRenderer under the config's chunk
// policy. This replaces the old inline "opencode:" prefix check + the duplicated
// chunker, so the shaping rule lives in exactly one place per platform (issue #21).
// This is the PER-SESSION axis — independent of which runner (engine) does the mining.
//
// cfg carries the ChunkMaxChars budget (issue #57): 0 (default) sends the whole
// session, a positive value splits an oversized one. The budget is source-agnostic —
// the same value governs whichever platform owns the session — so Claude and OpenCode
// shape identically and neither under-extracts a long session.
func distillInputs(st *store.Store, cfg store.Config, session string, raw []store.RawRecord) []string {
	policy := platform.ChunkPolicy{MaxChars: cfg.ChunkMaxChars}
	return platform.ForSession(st, session).RenderInputs(raw, policy)
}
