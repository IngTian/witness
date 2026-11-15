//go:build opencodecontract

package opencode

// The runner against a REAL OpenCode v2 binary and two local mock providers (no model calls):
//   - WITNESS_CONTRACT_MOCK_PORT: run/critic/scripts/mock.py (models ok, slow; it echoes the
//     system-prompt marks it saw as "MOCK-REPLY <marks>");
//   - WITNESS_CONTRACT_FREE_MOCK_PORT: run/freetier/scripts/mock.py (model free answers the
//     gateway's 429 FreeUsageLimitError).
// Opt-in by build tag, so `make test` never compiles it. PR 3 replaces this plumbing with its
// harness and runs it in CI.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func TestContractRunnerRoundTrip(t *testing.T) {
	bin, port, freePort := os.Getenv("WITNESS_CONTRACT_OPENCODE_BIN"), os.Getenv("WITNESS_CONTRACT_MOCK_PORT"), os.Getenv("WITNESS_CONTRACT_FREE_MOCK_PORT")
	if bin == "" || port == "" || freePort == "" {
		t.Fatal("set WITNESS_CONTRACT_OPENCODE_BIN, WITNESS_CONTRACT_MOCK_PORT and WITNESS_CONTRACT_FREE_MOCK_PORT (the tag is opt-in)")
	}
	if v := os.Getenv(platform.DisableExternalRunnersEnv); v != "" {
		t.Fatalf("%s=%q: Open would refuse, so this test could only fail or pass vacuously", platform.DisableExternalRunnersEnv, v)
	}
	tmp := t.TempDir()
	for k, sub := range map[string]string{"HOME": "home", "XDG_DATA_HOME": "data", "XDG_CONFIG_HOME": "config", "XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache"} {
		dir := filepath.Join(tmp, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(k, dir)
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9") // anything non-local fails: no real model call
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv(openCodeBinEnv, bin)

	provider := func(name, port string, models map[string]any) map[string]any {
		return map[string]any{"npm": "@ai-sdk/openai-compatible", "name": name, "models": models,
			"options": map[string]any{"baseURL": "http://127.0.0.1:" + port + "/v1", "apiKey": "mock-key"}}
	}
	privateConfigOverlay = map[string]any{"provider": map[string]any{
		// Neither mock lists variants, so #high is declared here (the legacy shape v2 migrates).
		"mock":     provider("Mock", port, map[string]any{"ok": map[string]any{"name": "ok", "variants": map[string]any{"high": map[string]any{}}}, "slow": map[string]any{"name": "slow"}}),
		"mockfree": provider("MockFree", freePort, map[string]any{"free": map[string]any{"name": "free"}}),
	}}
	prevMax := maxConcurrentGenerations
	maxConcurrentGenerations = 16 // read at construction; item 3 measures 16 on one serve
	t.Cleanup(func() { privateConfigOverlay, maxConcurrentGenerations = nil, prevMax })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := filepath.Join(tmp, "runtime")
	open := func() *runner {
		r := &runner{cfg: store.Config{RuntimeRoot: root, RunnerModels: []string{"mock/ok"}}}
		if err := r.Open(ctx); err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	r := open()
	v, _ := ProbeVersion(ctx)
	t.Logf("opencode %s (%s): serve up at %s", v, bin, r.server.baseURL)

	// 1, 2: each prompt is its own agent; the second is hot-added to the running serve. The mock
	// echoes exactly the marks it saw in the system messages, so equality proves the right prompt
	// and nothing else (no user-global AGENTS.md) reached the provider.
	for i, mark := range []string{"SYS-A-MARKER", "SYS-B-MARKER"} {
		reply, err := r.Run(ctx, "mock/ok", mark+" lens instructions", "DATA")
		if want := "MOCK-REPLY " + mark; err != nil || reply != want {
			t.Fatalf("item %d: Run = %q, %v; want %q", i+1, reply, err, want)
		}
		t.Logf("item %d: PASS %q", i+1, reply)
	}

	// #variant: fail only when v2 lists the declared variant and the Run still fails (witness's ref
	// is then wrong); a variant v2 dropped is logged for the report.
	listed := r.server.validateModels(ctx, []string{"mock/ok#high"})
	reply, err := r.Run(ctx, "mock/ok#high", "SYS-A-MARKER lens instructions", "DATA")
	switch {
	case err == nil && reply == "MOCK-REPLY SYS-A-MARKER":
		t.Logf("item #high: PASS %q (listed: %v)", reply, listed)
	case listed == nil:
		t.Fatalf("item #high: v2 lists mock/ok#high but Run = %q, %v", reply, err)
	default:
		t.Logf("item #high: NOTE v2 does not list the declared variant (%v); Run = %q, %v", listed, reply, err)
	}

	// 3: 16 at once on one serve. Recorded, not asserted: production stays at 8 until this holds.
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reply, err := r.Run(ctx, "mock/slow", "SYS-C-MARKER lens instructions", fmt.Sprint("DATA ", i)); err != nil {
				errs[i] = err
			} else if reply != "MOCK-REPLY SYS-C-MARKER" {
				errs[i] = fmt.Errorf("reply %q", reply)
			}
		}()
	}
	wg.Wait()
	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			t.Logf("item 3: run %d: %v", i, err)
		}
	}
	t.Logf("item 3: %d/16 succeeded in %s wall (one mock/slow call takes ~3 s; 8-way would need >= 6 s)", 16-failed, time.Since(start).Round(10*time.Millisecond))

	// 4: the gateway's free-limit 429 is a QuotaExhaustedError, whatever the provider.
	_, err = r.Run(ctx, "mockfree/free", "SYS-A-MARKER lens instructions", "DATA")
	var quota *platform.QuotaExhaustedError
	if !errors.As(err, &quota) {
		t.Fatalf("item 4: Run(mockfree/free) = %v; want *platform.QuotaExhaustedError", err)
	}
	t.Logf("item 4: PASS reset in %s (runtime %s)", time.Until(quota.ResetAt).Round(time.Second), quota.Runtime)

	// 5: the unavailable-model hint names the provider's env var, read from /api/integration.
	err = r.ValidateModels(ctx, "anthropic/nope")
	switch {
	case err == nil:
		t.Fatal("item 5: ValidateModels(anthropic/nope) = nil")
	case strings.Contains(err.Error(), "ANTHROPIC_API_KEY"):
		t.Logf("item 5: PASS %v", err)
	default:
		body, gerr := r.server.doJSON(ctx, http.MethodGet, "/api/integration/anthropic", nil, http.StatusOK)
		t.Logf("item 5: NOTE integration route shape differs from source: %v; GET /api/integration/anthropic = %.400s, %v", err, body, gerr)
	}

	// 6: a second runner on the same root (two serves, one private DB), both answering at once.
	r2 := open()
	var replies [2]string
	var runErrs [2]error
	for i, rr := range []*runner{r, r2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i], runErrs[i] = rr.Run(ctx, "mock/ok", "SYS-B-MARKER lens instructions", "DATA")
		}()
	}
	wg.Wait()
	for i := range replies {
		if runErrs[i] != nil || replies[i] != "MOCK-REPLY SYS-B-MARKER" {
			t.Fatalf("item 6: runner %d Run = %q, %v", i+1, replies[i], runErrs[i])
		}
	}
	t.Logf("item 6: PASS two serves on %s both round-trip", root)

	// 7: Close leaves nothing behind. Every serve and its children inherit OPENCODE_DB=<root>/…, so
	// the scan matches environments, and it must see the live serves first or it proves nothing.
	if runtime.GOOS != "darwin" {
		t.Log("item 7: SKIP the environment scan is macOS-only here (PR 3 replaces it)")
		return
	}
	live := procsUnder(t, root)
	if len(live) < 2 {
		t.Fatalf("item 7: the scan sees %d processes before Close, want both serves: %q", len(live), live)
	}
	for _, rr := range []*runner{r, r2} {
		if err := rr.Close(); err != nil {
			t.Errorf("item 7: Close: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for left := procsUnder(t, root); len(left) > 0; left = procsUnder(t, root) {
		if time.Now().After(deadline) {
			t.Fatalf("item 7: %d processes under %s survived Close: %q", len(left), root, left)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("item 7: PASS %d processes named the runtime root before Close, none after", len(live))
}

// procsUnder lists the live processes whose argv or environment contains root (`ps -E`, macOS).
func procsUnder(t *testing.T, root string) []string {
	out, err := exec.Command("ps", "-axEww", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var hits []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, root) {
			hits = append(hits, l[:min(len(l), 120)])
		}
	}
	return hits
}
