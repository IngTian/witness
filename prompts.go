// Package witness (repo root) exists solely to embed the built-in prompt
// templates into the binary at compile time, so a single downloaded executable
// is fully self-contained and needs no sibling prompts/ directory to run.
//
// go:embed can only reach files at or below the embedding file's own directory,
// and prompts/ must stay at the repo root (the README, the $CLAUDE_PLUGIN_ROOT
// plugin layout, and the Unix install all reference prompts/ there). So the embed
// lives here at the root rather than inside internal/lens, which consumes it via
// the Prompts export.
//
// This is the "embed the small templates, keep the big model on disk" split:
// prompts/ totals ~40KB; the 448MB embedding model stays external (too large to
// embed, and the capture hooks don't need it).
package witness

import "embed"

// Prompts holds the built-in prompt templates (prompts/default, prompts/summarize
// read at runtime; prompts/lens/* shipped as reference examples). Consumed by
// internal/lens as the always-available fallback when no on-disk prompts dir is
// configured.
//
//go:embed prompts
var Prompts embed.FS
