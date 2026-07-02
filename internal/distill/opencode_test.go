package distill

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestBuildOpenCodeRunCmdIsolation(t *testing.T) {
	cmd := buildOpenCodeRunCmd(context.Background(), "openai/gpt-5.5", "EXTRACT", "/tmp/corpus.md")
	for _, want := range []string{"run", "--pure", "--format", "json", "--agent", openCodeAgentName, "--file", "/tmp/corpus.md", "--model", "openai/gpt-5.5"} {
		if !slices.Contains(cmd.Args, want) {
			t.Fatalf("missing %q in args: %v", want, cmd.Args)
		}
	}
	joinedEnv := strings.Join(cmd.Env, "\n")
	if !strings.Contains(joinedEnv, "WITNESS_WORKER=1") || !strings.Contains(joinedEnv, "OPENCODE_DISABLE_CLAUDE_CODE=1") {
		t.Fatalf("missing recursion/isolation env: %s", joinedEnv)
	}
	if !strings.Contains(joinedEnv, "OPENCODE_CONFIG_CONTENT=") || !strings.Contains(joinedEnv, "EXTRACT") || !strings.Contains(joinedEnv, "UNTRUSTED") {
		t.Fatalf("agent prompt config missing: %s", joinedEnv)
	}
}

func TestParseOpenCodeRunOutput(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"text","part":{"id":"p1","type":"text","text":""}}`,
		`{"type":"text","part":{"id":"p1","type":"text","text":"first"}}`,
		`{"type":"message.part.updated","part":{"id":"p2","type":"reasoning","text":"hidden"}}`,
		`{"type":"text","part":{"id":"p2","type":"text","text":"second"}}`,
	}, "\n")
	got := parseOpenCodeRunOutput(out)
	if got != "first\n\nsecond" {
		t.Fatalf("got %q", got)
	}
}

func TestParseOpenCodeModels(t *testing.T) {
	out := strings.Join([]string{
		"openai/gpt-5.5",
		"openai/gpt-5.5-fast",
		"openai/gpt-5.5",
		"metadata without model",
	}, "\n")
	got := parseOpenCodeModels(out)
	want := []string{"openai/gpt-5.5", "openai/gpt-5.5-fast"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestModelHint(t *testing.T) {
	if got := modelHint(nil); !strings.Contains(got, "returned no models") {
		t.Fatalf("empty hint = %q", got)
	}
	if got := modelHint([]string{"openai/gpt-5.5"}); !strings.Contains(got, "openai/gpt-5.5") {
		t.Fatalf("model hint = %q", got)
	}
}

func TestRunWithRejectsUnknownRunner(t *testing.T) {
	if _, err := RunWith(context.Background(), "bogus", "", "", ""); err == nil {
		t.Fatalf("unknown runner should fail")
	}
}
