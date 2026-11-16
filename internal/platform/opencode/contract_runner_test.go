//go:build opencodecontract

package opencode

// The runner against a REAL OpenCode v2 binary and the harness's in-process mock provider. Opt-in
// by build tag, so `make test` never compiles it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// newContractRunner creates a runner on a scratch env with the mock provider, opens it and
// registers cleanup. before runs after the scratch env exists and before Open (nil = nothing).
func newContractRunner(t *testing.T, before func(scratch)) (*runner, *mockProvider, context.Context, store.Config) {
	requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	s.setProcessEnv(t)
	m := startMock(t)
	prev := privateConfigOverlay
	t.Cleanup(func() { privateConfigOverlay = prev })
	var overlay map[string]any
	if err := json.Unmarshal([]byte(m.ProviderConfig()), &overlay); err != nil {
		t.Fatalf("unmarshal provider config: %v", err)
	}
	privateConfigOverlay = overlay // PR 2's seam: in-process, so this also runs on darwin, no /etc write
	if before != nil {
		before(s)
	}
	cfg := store.Config{Runner: "opencode", RuntimeRoot: filepath.Join(s.Root, "runtime"), TriageModel: "mock/ok", DistillModel: "mock/ok", RunnerModels: []string{"mock/ok"}}
	r := Platform{}.NewRunner(cfg)
	rr, ok := r.(*runner)
	if !ok {
		t.Fatalf("NewRunner returned %T, want *runner", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := rr.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rr.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rr, m, ctx, cfg
}

func TestContractRunnerRoundTrip(t *testing.T) {
	t.Run("isolation", func(t *testing.T) {
		r, m, ctx, _ := newContractRunner(t, func(s scratch) {
			// The two leaks the brief forbids: user-global AGENTS.md and OPENCODE_CONFIG instructions.
			agentsPath := filepath.Join(s.ConfigDir, "opencode", "AGENTS.md")
			if err := os.MkdirAll(filepath.Dir(agentsPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(agentsPath, []byte("GLOBAL-AGENTS-MD-MARKER"), 0o600); err != nil {
				t.Fatal(err)
			}
			leak := filepath.Join(s.Root, "leak.md")
			if err := os.WriteFile(leak, []byte("OPENCODE-CONFIG-LEAK-MARKER"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(s.Root, "leak-config.json")
			b, _ := json.Marshal(map[string][]string{"instructions": {leak}})
			if err := os.WriteFile(cfg, b, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OPENCODE_CONFIG", cfg)
		})
		out, err := r.Run(ctx, "mock/ok", "LENS-SYSTEM-MARKER instructions", "CONTRACT-INPUT")
		if err != nil || !strings.Contains(out, "MOCK-REPLY") {
			t.Fatalf("Run: %q %v", out, err)
		}
		calls := m.Calls("ok")
		if len(calls) != 1 {
			t.Fatalf("%d provider calls, want 1 (a title request would double free-allowance use)", len(calls))
		}
		c := calls[0]
		if !strings.Contains(c.System, "LENS-SYSTEM-MARKER") || !strings.Contains(c.User, "CONTRACT-INPUT") {
			t.Fatalf("missing lens prompt or input in %+v", c)
		}
		if c.Tools != 0 {
			t.Errorf("deny-all agent was offered %d tools: %v (raw: %s)", c.Tools, c.ToolNames, c.RawTools)
		}
		for _, leak := range []string{"GLOBAL-AGENTS-MD-MARKER", "OPENCODE-CONFIG-LEAK-MARKER"} {
			if strings.Contains(c.System, leak) {
				t.Errorf("%s reached the lens prompt", leak)
			}
		}
	})

	t.Run("variant", func(t *testing.T) {
		r, _, ctx, _ := newContractRunner(t, nil)
		// Fail when v2 lists the declared variant and the Run still fails (witness's ref is then
		// wrong); a variant v2 dropped fails in CI too (settled).
		listErr := r.server.validateModels(ctx, []string{"mock/ok#high"})
		reply, err := r.Run(ctx, "mock/ok#high", "LENS-SYSTEM-MARKER instructions", "DATA")
		switch {
		case err == nil && reply == "MOCK-REPLY ok":
			t.Logf("PASS %q (list error: %v)", reply, listErr)
		case listErr == nil:
			t.Fatalf("v2 lists mock/ok#high but Run = %q, %v", reply, err)
		default:
			settled(t, "v2 does not list the declared variant (%v); Run = %q, %v", listErr, reply, err)
		}
	})

	t.Run("validate-models", func(t *testing.T) {
		r, _, ctx, _ := newContractRunner(t, nil)
		// The unavailable-model hint names the provider's env var, read from /api/integration.
		err := r.ValidateModels(ctx, "anthropic/nope")
		switch {
		case err == nil:
			t.Fatal("ValidateModels(anthropic/nope) = nil")
		case strings.Contains(err.Error(), "ANTHROPIC_API_KEY"):
			t.Logf("PASS %v", err)
		default:
			body, gerr := r.server.doJSON(ctx, http.MethodGet, "/api/integration/anthropic", nil, http.StatusOK)
			settled(t, "integration route shape differs from source: %v; GET /api/integration/anthropic = %.400s, %v", err, body, gerr)
		}
	})

	t.Run("second-runner", func(t *testing.T) {
		r1, _, ctx, cfg := newContractRunner(t, nil)
		// A second runner on the same root (two serves, one private DB), both answering at once.
		r2 := Platform{}.NewRunner(cfg).(*runner)
		if err := r2.Open(ctx); err != nil {
			t.Fatalf("Open second runner: %v", err)
		}
		defer func() {
			if err := r2.Close(); err != nil {
				t.Errorf("Close second runner: %v", err)
			}
		}()
		if r1.server.baseURL == r2.server.baseURL {
			t.Fatalf("both runners share one serve at %s, want two distinct serves", r1.server.baseURL)
		}
		var replies [2]string
		var runErrs [2]error
		var wg sync.WaitGroup
		for i, rr := range []platform.Runner{r1, r2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				replies[i], runErrs[i] = rr.Run(ctx, "mock/ok", "LENS-SYSTEM-MARKER instructions", "DATA")
			}()
		}
		wg.Wait()
		for i := range replies {
			if runErrs[i] != nil || replies[i] != "MOCK-REPLY ok" {
				t.Fatalf("runner %d Run = %q, %v", i+1, replies[i], runErrs[i])
			}
		}
		t.Logf("PASS two serves on %s both round-trip", cfg.RuntimeRoot)
	})
}

// settled reports a broken premise the contract runs have settled: a NOTE on a developer's machine,
// a failure in CI (WITNESS_CONTRACT_CHANNEL is set), where it means v2 changed.
func settled(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("WITNESS_CONTRACT_CHANNEL") != "" {
		t.Fatalf(format, args...)
	}
	t.Logf("NOTE "+format, args...)
}

func TestContractRunnerQuotaExhausted(t *testing.T) {
	r, m, ctx, _ := newContractRunner(t, nil)
	start := time.Now()
	_, err := r.Run(ctx, "mock/free", "sys", "in")
	end := time.Now()
	var q *platform.QuotaExhaustedError
	if !errors.As(err, &q) || q.Runtime != "opencode" {
		t.Fatalf("err = %v, want *QuotaExhaustedError{Runtime: opencode}", err)
	}
	if q.ResetAt.Before(start.Add(time.Hour-5*time.Second)) || q.ResetAt.After(end.Add(time.Hour+5*time.Second)) {
		t.Fatalf("ResetAt %v, want completion + retryAfterMs (1h)", q.ResetAt)
	}
	if n := len(m.Calls("free")); n != 1 || end.Sub(start) > 30*time.Second {
		t.Fatalf("%d calls in %s: a refusal must stop at the first try", n, end.Sub(start))
	}
}

func TestContractRunnerSixteenParallel(t *testing.T) {
	prev := maxConcurrentGenerations
	maxConcurrentGenerations = 16
	t.Cleanup(func() { maxConcurrentGenerations = prev })
	// spec 7.5 "verify 16": 16-way verified on 2.2.7 and 2.3.0, Linux and Windows; production keeps 8
	// (maxConcurrentGenerations) pending the owner's decision.
	// Distinct system prompts load agents serially by design (OpenCodeServer.ensureAgent holds agentMu),
	// and that is not a generation cap. This test uses one shared system prompt (one agent).
	r, m, ctx, _ := newContractRunner(t, nil)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, err := r.Run(ctx, "mock/slow", "shared system prompt", fmt.Sprintf("PAR-%02d", i))
			if err != nil {
				errs[i] = err
			} else if reply != "MOCK-REPLY slow" {
				errs[i] = fmt.Errorf("reply %q", reply)
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	calls := m.Calls("slow")
	if len(calls) != 16 {
		t.Fatalf("%d provider calls, want 16", len(calls))
	}
	// Each PAR-xx must appear in exactly one call's User.
	re := regexp.MustCompile(`PAR-\d\d`)
	seen := make(map[string]int)
	for _, c := range calls {
		if mark := re.FindString(c.User); mark != "" {
			seen[mark]++
		}
	}
	if len(seen) != 16 {
		t.Errorf("found %d distinct PAR-xx markers, want 16: %v", len(seen), maps.Keys(seen))
	}
	for mark, count := range seen {
		if count != 1 {
			t.Errorf("%s appeared in %d calls, want 1", mark, count)
		}
	}
	peak := m.PeakInFlight()
	if peak != 16 {
		t.Errorf("peak in-flight %d, want 16 (wall %s; one mock/slow call takes ~2 s, so 16-way should be ~2 s, 8-way ~4 s)", peak, wall.Round(10*time.Millisecond))
	} else {
		t.Logf("PASS 16 generations in parallel (peak %d, wall %s)", peak, wall.Round(10*time.Millisecond))
	}
}
