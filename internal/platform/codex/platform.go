// Package codex imports local Codex conversations. Capture and distillation are
// independent: this source deliberately does not supply a Codex model runner.
package codex

import (
	"context"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

type Platform struct{}

func init()                            { platform.Register(Platform{}) }
func (Platform) Name() string          { return "codex" }
func (Platform) SessionPrefix() string { return "codex:" }
func (Platform) RenderInputs(raw []store.RawRecord, policy platform.ChunkPolicy) []string {
	return platform.RenderChunks(raw, policy)
}
func (Platform) Import(ctx context.Context, st store.ImportStore, ids []string) (platform.ImportStats, error) {
	unlock, ok := st.ImportLock("codex")
	if !ok {
		return platform.ImportStats{Agent: "codex"}, nil
	}
	defer unlock()
	return (&Importer{Store: st}).Import(ctx, ids)
}
