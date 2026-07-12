package distill

import (
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// distillInputs is the source-specific L0 -> model-input seam. It resolves the
// session's OWNING platform (persisted session_meta.platform, else id prefix, else
// Claude) and delegates to that platform's InputRenderer. This replaces the old
// inline "opencode:" prefix check + the duplicated chunker, so the shaping rule
// lives in exactly one place per platform (issue #21). This is the PER-SESSION
// axis — independent of which runner (engine) does the mining.
func distillInputs(st *store.Store, session string, raw []store.RawRecord) []string {
	return platform.ForSession(st, session).RenderInputs(raw)
}
