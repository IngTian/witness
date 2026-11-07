// Package codex imports local Codex conversations. Capture and distillation are
// independent: this source deliberately does not supply a Codex model runner.
package codex

import (
	"context"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// SessionPrefix namespaces Codex sessions in L0. The ONE spelling: the importer builds every id from it.
const SessionPrefix = "codex:"

type Platform struct{}

func init() { platform.Register(Platform{}) }

// Capabilities are discovered by type assertion, so a signature that drifts would silently
// drop one (an Import that no longer matches imports nothing and reports "up to date").
// These make that a compile error instead.
var (
	_ platform.Importer = Platform{}
)

func (Platform) Name() string          { return "codex" }
func (Platform) SessionPrefix() string { return SessionPrefix }
func (Platform) Import(ctx context.Context, st store.ImportStore, ids []string) (platform.ImportStats, error) {
	unlock, ok := st.ImportLock("codex")
	if !ok {
		return platform.ImportStats{Agent: "codex"}, nil
	}
	defer unlock()
	return (&Importer{Store: st}).Import(ctx, ids)
}
