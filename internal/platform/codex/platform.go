// Package codex imports local Codex conversations and provides an isolated,
// tool-free distillation runner with its own login.
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
	_ platform.Importer            = Platform{}
	_ platform.RunnerProvider      = Platform{}
	_ platform.ModelRequirer       = Platform{}
	_ platform.RunnerLoginGate     = Platform{}
	_ platform.RunnerAuthenticator = Platform{}
	_ platform.ModelFormat         = Platform{}
	_ platform.PlanAllowance       = (*runner)(nil)
)

func (Platform) Name() string          { return "codex" }
func (Platform) SessionPrefix() string { return SessionPrefix }

func (Platform) ReadyToBind(ctx context.Context, root string) error { return LoginReady(ctx, root) }
func (Platform) Login(ctx context.Context, root string) error       { return Login(ctx, root) }
func (Platform) Logout(ctx context.Context, root string) error {
	return loginCommand(ctx, root, true)
}

func (Platform) ModelFormat() (string, string) { return "model", "Codex model slug (bare name)" }
func (Platform) ValidModel(model string) bool {
	return validModelName(model)
}
func (Platform) Import(ctx context.Context, st store.ImportStore, ids []string) (platform.ImportStats, error) {
	unlock, ok := st.ImportLock("codex")
	if !ok {
		return platform.ImportStats{Agent: "codex"}, nil
	}
	defer unlock()
	return (&Importer{Store: st}).Import(ctx, ids)
}
