package opencode

import (
	"testing"
	"time"
)

// The owner's call (2026-11-13): one OpenCode generation may run 20 minutes, the same wall as
// claudeRunTimeout (internal/platform/claude/deadline_test.go). Compaction's union writes are sized
// against it (distill's compactUnionMaxChars = 60k chars); lowering it silently re-breaks those
// writes and resends up to 60k characters to the shared free allowance on every failed attempt.
func TestAnOpenCodeGenerationMayRunTwentyMinutes(t *testing.T) {
	if generateTimeout != 20*time.Minute {
		t.Errorf("generateTimeout = %s; compaction's union cap (60k chars) is sized for a 20-minute wall", generateTimeout)
	}
	if requestLifetimeBudget() != queueWaitBudget+generateTimeout+time.Minute {
		t.Error("the ctx ceiling must cover both phases")
	}
}
