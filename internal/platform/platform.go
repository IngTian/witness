// Package platform holds the session-source registry (Claude Code, OpenCode, Codex,
// ingested files) and the shared identity/stats types. Core witness code depends on
// these small boundaries instead of embedding any one source's details throughout the
// CLI. The per-source implementations live in the claude/, opencode/, codex/ and file/
// subpackages; this file holds the cross-source constants and value types.
package platform

import (
	"context"
	"slices"
)

const AgentClaude = "claude"

// AppendFrom decides how an importer whose source can rewrite history (OpenCode, Codex) applies
// a transcript: old is the turn keys the last import stored, cur the source's keys now, rawCount
// the session's L0 rows. It appends cur[from:] when old is a prefix of cur that the archive still
// holds. Rows past the stored keys are turns `witness merge` appended from another machine's copy
// (that copy arrives under a minted id, so no key describes them); they stay, and so does the
// session's progress, because what the source added goes after them. Requiring the counts to be
// equal replaced the session, deleting them and re-mining it. No stored keys over existing rows
// says nothing about those rows, so it is not ok either. When not ok, pass the whole transcript
// to ApplyRawImport with replace=true: the store keeps the unchanged (role, text) prefix.
func AppendFrom(old, cur []string, rawCount int) (from int, ok bool) {
	if (len(old) > 0 || rawCount == 0) && len(old) <= len(cur) && rawCount >= len(old) && slices.Equal(cur[:len(old)], old) {
		return len(old), true
	}
	return 0, false
}

type ImportStats struct {
	Agent    string
	Sessions int
	Records  int
	Failed   int // source inputs skipped after recoverable errors; successful imports are retained
}

// ImportProgress is told how many source inputs an import has finished out of the total it has
// found: an OpenCode session, a Codex history thread or rollout file. A first import is every
// session the user ever had — thousands on a mature OpenCode archive — and `witness import`
// printed nothing until the end. An importer reports only once its inputs are all counted, so
// total is final from the first report; a failed input counts as finished.
type ImportProgress func(done, total int)

type importProgressKey struct{}

// WithImportProgress hands fn to whichever Importer runs under ctx. It rides the context the way
// net/http/httptrace's hooks do — an observer, not an input — so the Importer interface and the
// adapters with nothing to count (Claude, file) are untouched, and every caller that does not
// ask (the capture hooks, the OpenCode plugin's --quiet import) stays exactly as silent.
func WithImportProgress(ctx context.Context, fn ImportProgress) context.Context {
	return context.WithValue(ctx, importProgressKey{}, fn)
}

// ImportProgressFrom returns the reporter WithImportProgress put in ctx, or a no-op.
func ImportProgressFrom(ctx context.Context) ImportProgress {
	if fn, _ := ctx.Value(importProgressKey{}).(ImportProgress); fn != nil {
		return fn
	}
	return func(int, int) {}
}
