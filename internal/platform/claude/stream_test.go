package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
)

// stubClaude makes Run's child a shell that prints stream and exits code; `claude` is never spawned.
func stubClaude(t *testing.T, stream string, code int) {
	t.Helper()
	unsetDisableSwitch(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no `sh` to stand in for the CLI: %v", err)
	}
	prev := buildRunCmdFn
	buildRunCmdFn = func(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
		c := exec.CommandContext(ctx, sh, "-c", fmt.Sprintf("cat; exit %d", code))
		c.Stdin = strings.NewReader(stream)
		return c
	}
	t.Cleanup(func() { buildRunCmdFn = prev })
}

func rateLimitLine(status string, at int64) string {
	return fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":%q,"resetsAt":%d}}`+"\n", status, at)
}

const limitResult = `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your Opus limit · resets Mon 12:00am"}`

// testdata/usage-limit.stream.jsonl is hand-built (see testdata/README.md): its resetsAt and the
// stub's exit code are what the fixture records.
const (
	capturedResetEpoch = 1795000000
	capturedExit       = 1
)

// A usage-limit refusal must come back as the quota error cmd's gate pauses on, with the exact reset.
func TestClaudeUsageLimitPauses(t *testing.T) {
	assertPause := func(t *testing.T, want int64) {
		t.Helper()
		_, err := testRunner(t).Run(context.Background(), "", "sys", "corpus")
		var q *platform.QuotaExhaustedError
		if !errors.As(err, &q) || q.Runtime != "claude" || !q.ResetAt.Equal(time.Unix(want, 0)) {
			t.Fatalf("want QuotaExhaustedError{claude, %s}, got %v", time.Unix(want, 0), err)
		}
	}
	t.Run("captured stream", func(t *testing.T) {
		raw, err := os.ReadFile("testdata/usage-limit.stream.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`"resetsAt":\s*(\d+)`).FindSubmatch(raw)
		if m == nil || string(m[1]) != strconv.FormatInt(capturedResetEpoch, 10) {
			t.Fatalf("CLI must report the mock's reset in epoch SECONDS")
		}
		want := time.Now().Add(time.Hour).Unix() // the captured epoch is stale by now; rewrite it, no clock seam
		stubClaude(t, string(bytes.ReplaceAll(raw, m[1], []byte(strconv.FormatInt(want, 10)))), capturedExit)
		assertPause(t, want)
	})
	t.Run("model-specific reset just inside 8 days", func(t *testing.T) {
		want := time.Now().Add(8*24*time.Hour - time.Minute).Unix()
		stubClaude(t, fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":%d,"rateLimitType":"seven_day_opus"}}`, want)+"\n"+limitResult, 1)
		assertPause(t, want)
	})
}

// Fail safe: anything short of a rejected event with a plausible reset on a failed run is an
// ordinary failure carrying the CLI's own message, never a pause.
func TestClaudeFailuresThatAreNotAUsageLimit(t *testing.T) {
	cloud, err := os.ReadFile("testdata/cloud-credential-error.stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		name, stream string
		code         int
		wantErr      string
	}{
		{"no event", limitResult, 1, "hit your Opus limit"},
		{"allowed_warning", rateLimitLine("allowed_warning", now.Add(time.Hour).Unix()) + limitResult, 1, "hit your"},
		{"rejected without resetsAt", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}` + "\n" + limitResult, 1, "hit your"},
		{"rejected in the past", rateLimitLine("rejected", now.Add(-time.Hour).Unix()) + limitResult, 1, "hit your"},
		{"rejected in milliseconds", rateLimitLine("rejected", now.Add(time.Hour).UnixMilli()) + limitResult, 1, "hit your"},
		{"rejected beyond 8 days", rateLimitLine("rejected", now.Add(8*24*time.Hour+time.Minute).Unix()) + limitResult, 1, "hit your"},
		{"bedrock 429", `{"type":"result","is_error":true,"result":"API Error: Request rejected (429) · this may be a temporary capacity issue"}`, 1, "Request rejected (429)"},
		// Real 2.1.327 capture: a failed run says subtype "success", so only is_error can tell.
		{"cloud credential error", string(cloud), 1, "Could not load AWS credentials"},
		{"is_error on exit 0", `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 500"}`, 0, "API Error: 500"},
		{"exit 0 without a result", `{"type":"system","subtype":"init"}`, 0, "no result event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubClaude(t, tc.stream, tc.code)
			_, err := testRunner(t).Run(context.Background(), "", "sys", "corpus")
			var q *platform.QuotaExhaustedError
			if err == nil || errors.As(err, &q) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an ordinary failure containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A rejected event the CLI recovered from (it retried or fell back) must not cost the reply.
func TestClaudeSuccessAfterARejectedEventReturnsTheReply(t *testing.T) {
	stubClaude(t, rateLimitLine("rejected", time.Now().Add(time.Hour).Unix())+`{"type":"result","subtype":"success","is_error":false,"result":"[]"}`, 0)
	if got, err := testRunner(t).Run(context.Background(), "", "sys", "corpus"); err != nil || got != "[]" {
		t.Fatalf("Run = %q, %v; want the result text", got, err)
	}
}

// The real success stream (hook and thinking_tokens events), plus a debug line, junk and a blank
// line, yields exactly the result event's text.
func TestClaudeSuccessStreamReturnsTheResultVerbatim(t *testing.T) {
	raw, err := os.ReadFile("testdata/success.stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var last struct{ Type, Result string }
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if last.Type != "result" || !strings.Contains(last.Result, "PROBE-7Q") {
		t.Fatal("fixture is not proof call 2's stream")
	}
	stubClaude(t, "[ANTHROPIC_LOG] debug: POST /v1/messages\n"+lines[0]+"\n{not json\n\n"+strings.Join(lines[1:], "\n")+"\n", 0)
	if got, err := testRunner(t).Run(context.Background(), "", "sys", "corpus"); err != nil || got != last.Result {
		t.Fatalf("Run = %q, %v; want the result event's text %q", got, err, last.Result)
	}
}

// Every Claude model draws on the login's one limit, so cmd's gate pauses them all.
func TestClaudeRunnerSharesOneAllowance(t *testing.T) {
	sa, ok := testRunner(t).(platform.SharedAllowance)
	if !ok || !sa.OnSharedAllowance("") || !sa.OnSharedAllowance("claude-opus-4-5") {
		t.Fatal("the claude runner must report every model on the shared allowance")
	}
}
