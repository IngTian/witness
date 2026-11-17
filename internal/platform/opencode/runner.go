package opencode

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

var errRunBeforeOpen = errors.New("opencode runner: Run called before Open")

// NewRunner mints the OpenCode distillation runner, bound to cfg's models. It
// reuses ONE `opencode serve` process across every Run in a drain. Its database is
// rooted under the witness store, so Close only stops that private process.
func (Platform) NewRunner(cfg store.Config) platform.Runner {
	return &runner{cfg: cfg}
}

type runner struct {
	cfg    store.Config
	server *OpenCodeServer
}

func (r *runner) Open(ctx context.Context) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("opencode runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	// <root>/xdg/opencode/auth.json was a stale secret witness copied; plus a v1 DB and pid file
	// nothing reads, and the removed native-session cache's manifests. An empty root removes
	// nothing, and StartOpenCodeServerIn refuses it.
	removeRetiredRuntime(r.cfg.RuntimeRoot)
	srv, err := StartOpenCodeServerIn(ctx, r.cfg.RuntimeRoot, r.cfg.RunnerModels)
	if err != nil {
		return err
	}
	r.server = srv
	return nil
}

func (r *runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if r.server == nil {
		return "", errRunBeforeOpen
	}
	return r.server.Run(ctx, model, systemPrompt, input)
}

func (r *runner) Close() error {
	if r.server == nil {
		return nil // never opened (no work this drain) — nothing to stop or sweep
	}
	return r.server.Close()
}

func (r *runner) ValidateModels(ctx context.Context, models ...string) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("opencode runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	srv, err := StartOpenCodeServerIn(ctx, r.cfg.RuntimeRoot, models)
	if err != nil {
		return err
	}
	return srv.Close()
}

// isFreeModel: only OpenCode Zen's own opencode/* models are free, and their allowance is counted
// per network (evidence freetier §4). opencode-go/* is the paid subscription, a different provider.
func isFreeModel(model string) bool { return strings.HasPrefix(strings.TrimSpace(model), "opencode/") }

func (*runner) OnFreeModel(model string) bool { return isFreeModel(model) }

// Discovered by type assertion like the platform capabilities; pinned the same way.
var _ platform.FreeAllowance = (*runner)(nil)

func (*runner) InvocationHint() string { return "opencode serve --stdio" }

// (SweepsOnClose() false used to be here. The capability is gone from the port — see
// platform.Runner — because this was its only implementation and the shared-DB sweep it reported on
// was deleted with #119's private database. Open sweeps only week-old serve dirs; each serve ends on
// its own stdin lease.)
