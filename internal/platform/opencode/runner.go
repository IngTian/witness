package opencode

import (
	"context"
	"errors"
	"fmt"

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
	native *nativeRuntime
}

func (r *runner) Open(ctx context.Context) error {
	if platform.ExternalRunnersDisabled() {
		return fmt.Errorf("opencode runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	if r.cfg.RuntimeRoot == "" {
		return fmt.Errorf("opencode isolated runtime root is unavailable")
	}
	// <root>/xdg held a COPY of the user's auth.json made by the v1 prepareAuth; 2.2.7+ imports
	// such a file into a fresh DB (runner §(d) Credentials), and 7.6 copies nothing.
	removeV1Runtime(r.cfg.RuntimeRoot)
	srv, err := StartOpenCodeServerIn(ctx, r.cfg.RuntimeRoot, r.cfg.RunnerModels)
	if err != nil {
		return err
	}
	r.server = srv
	r.native = newNativeRuntime(r.cfg.RuntimeRoot, srv)
	return r.native.reconcile()
}

func (r *runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if r.server == nil {
		return "", errRunBeforeOpen
	}
	if n := platform.NativeSessionFromContext(ctx); n != nil {
		// Same generation deadline the legacy path gets inside OpenCodeServer.Run: the
		// native path bypasses that method, and the caller's ctx has no deadline, so
		// without this wrap a stalled serve process polls forever holding WorkerLock.
		ctx, cancel := context.WithTimeout(ctx, requestLifetimeBudget())
		defer cancel()
		return r.native.run(ctx, n, model, systemPrompt, input)
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

// Discovered by type assertion like the platform capabilities; pinned the same way.
var _ platform.NativeSessionSupport = (*runner)(nil)

func (*runner) InvocationHint() string      { return "opencode serve --stdio" }
func (*runner) SupportsNativeSession() bool { return true }

// (SweepsOnClose() false used to be here. The capability is gone from the port — see
// platform.Runner — because this was its only implementation and the shared-DB sweep it reported on
// was deleted with #119's private database. Open sweeps only week-old serve dirs; each serve ends on
// its own stdin lease.)
