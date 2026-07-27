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
	if err := newNativeRuntime(r.cfg.RuntimeRoot, nil).prepareAuth(); err != nil {
		return err
	}
	srv, err := StartOpenCodeServerIn(ctx, r.cfg.RuntimeRoot, r.cfg.TriageModel, r.cfg.DistillModel)
	if err != nil {
		return err
	}
	r.server = srv
	r.native = newNativeRuntime(r.cfg.RuntimeRoot, srv)
	if err := r.native.reconcile(); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

func (r *runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if r.server == nil {
		return "", errRunBeforeOpen
	}
	if n := platform.NativeSessionFromContext(ctx); n != nil {
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
	return ValidateOpenCodeModels(ctx, models...)
}

func (*runner) InvocationHint() string { return "opencode serve" }

// ConcurrentRunSafe is true: OpenCodeServer.Run now holds its mutex only for the
// closed-check (server.go), not the whole request, and each Run drives its own
// isolated OpenCode session over the shared http.Client. A benchmark against a
// real `opencode serve` confirmed the server accepts many concurrent isolated
// sessions (see the local concurrency probe / issue #22), so the engine may mine
// several OpenCode sessions at once. If the configured PROVIDER rate-limits, the
// excess requests queue at the provider and witness's existing backoff absorbs it
// — that is a provider property, not a witness serialization constraint.
func (*runner) ConcurrentRunSafe() bool     { return true }
func (*runner) SupportsNativeSession() bool { return true }

func (*runner) SweepsOnClose() bool { return false }
