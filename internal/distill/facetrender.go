package distill

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/store"
)

// The prior-facet half of the review prompt, budgeted.
//
// WHY THIS EXISTS. reviewLens used to send every current facet's FULL value as JSON. On a
// mature archive that is the entire call: measured on a real 2,039-facet lens, the facet
// block was 2,411,507 chars (~600k tokens) against a 16,000-char observation budget — 99.4%
// prior state, 0.6% new evidence. `claude -p` REFUSED it outright (exit 1, empty stderr) and
// the lens had not folded for 24 days; a 920,574-char sibling lens was accepted but took
// longer than the 10-minute wall and was killed. Both failures were reproduced A/B on clones
// of the real archive, which is also how the rival "stale prompt" explanation was eliminated.
//
// WHAT THE MODEL ACTUALLY NEEDS. The prior facets serve three distinct jobs, and only one of
// them needs the prose:
//
//	identity      don't duplicate an existing facet -> needs dimension+key ONLY. This is the
//	              irreducible floor: applyFacet keys on lens|dimension|key, so a facet hidden
//	              from the prompt is silently minted as a BRAND-NEW duplicate with no warning.
//	reinforcement applyFacet keeps max(current, returned) confidence, so a reinforcement is a
//	              no-op unless the model can see and exceed current_confidence -> needs the
//	              number only.
//	change        deciding "does this new evidence CONTRADICT the stored value" -> the only
//	              job that needs the full text, and only for facets this window bears on.
//
// So: every facet's identity and confidence, always; a clipped head of every value; and full
// prose for the facets most likely to be contradicted.
//
// WHY A CLIPPED HEAD CARRIES THE SIGNAL. The review prompts trained the model to lead each
// value with a changelog verdict ("NEW (2026-08-19, four observations, poignancy 7/6/6/4)",
// "UPGRADE — contradicts_prior=true, 0.7 → 0.75"). Measured over every consecutive version
// pair in a real archive: all 161 of one lens's real changes diverge from their predecessor
// within 62 bytes, and a sibling lens's 1,550 within 240 bytes in 98.7% of cases. The change
// signal is front-loaded by convention, so a head clip preserves it rather than losing it.
//
// WHY "has changed before" IS THE FULL-PROSE SELECTOR. The obvious selectors were measured
// dead on real data: embedding top-K recalls 80.3% at K=200; recency gives ZERO facets inside
// 14 days on the lens that matters; poignancy puts 88% of observations in a 4-7 band. Version
// count is free (ReadFacets already loads every version), needs no embedder, and self-tunes
// per lens — 49 of 2,039 on one lens, 380 of 720 on another, from one global setting.
//
// The block headers live HERE, in Go, and deliberately not in the lens prompt files: those are
// copied into an archive once at seed time and never updated, so prompt text provably never
// reaches an existing install (that is its own bug). Text in Go reaches everyone on upgrade.
const (
	// clipMarker ends a clipped value. Deliberately not a bare ellipsis: it must be
	// detectable in a reply so an echoed stub can be refused (see truncationEchoed), and a
	// bare "…" appears in real prose.
	clipMarker = " …[clipped]"

	// Two variants, because claiming truncation when nothing was truncated is a lie that
	// costs real calls: the model would distrust complete values and assert contradictions
	// expecting a re-ask it does not need.
	priorIndexHeaderClipped = "CURRENT PROFILE (L2) — ALL %d FACETS, one per line as `dimension/key [confidence] value`.\n" +
		"Values longer than %d characters are TRUNCATED and end with \"" + clipMarker + "\".\n" +
		"Reuse a dimension+key from this list EXACTLY when your evidence bears on that facet;\n" +
		"a key absent from this list is new. Do NOT re-emit a facet the new observations say nothing about.\n"

	priorIndexHeaderWhole = "CURRENT PROFILE (L2) — ALL %d FACETS, one per line as `dimension/key [confidence] value`.\n" +
		"Values are complete.\n" +
		"Reuse a dimension+key from this list EXACTLY when your evidence bears on that facet;\n" +
		"a key absent from this list is new. Do NOT re-emit a facet the new observations say nothing about.\n"

	priorFullHeader = "\nFACETS WITH A CHANGE HISTORY — FULL CURRENT VALUE (%d), most recently changed first.\n" +
		"These are the ones that have moved before. Judge contradiction against the full text here.\n"

	// priorClipRule is appended when anything was clipped. It is the model-facing half of the
	// blind-close guard in reviewer.go: a contradiction against a value the model could only
	// read truncated is re-asked with the full text rather than applied.
	priorClipRule = "\nIf your evidence contradicts a facet you can only see TRUNCATED, still assert it with\n" +
		"contradicts_prior: true — you will be re-asked with that facet's full value before anything\n" +
		"is recorded. NEVER copy a truncated value (one ending \"" + clipMarker + "\") back as a value.\n"
)

// renderPriorFacets builds the prior-facet blocks for one lens and reports which facets were
// shown in FULL, so the caller can refuse to close a version the model only saw truncated.
//
// clip <= 0 restores the old unbounded behaviour (every value in full, no marker, every key in
// shownFull) — the escape hatch if a lens genuinely needs it.
func renderPriorFacets(facets []store.Facet, lensName string, clip, topFull int) (text string, shownFull map[string]bool, indexN, fullN int) {
	mine := make([]store.Facet, 0, len(facets))
	for _, f := range facets {
		if f.Lens == lensName && f.Current() != nil {
			mine = append(mine, f)
		}
	}
	shownFull = make(map[string]bool, len(mine))
	if len(mine) == 0 {
		return "CURRENT PROFILE (L2): empty — no facets yet for this lens.\n", shownFull, 0, 0
	}

	// Unbounded escape hatch: behave as before, and mark everything as fully shown so the
	// blind-close guard never fires.
	if clip <= 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "CURRENT PROFILE (L2) — ALL %d FACETS, full values.\n", len(mine))
		for i := range mine {
			cur := mine[i].Current()
			fmt.Fprintf(&b, "%s/%s [%.2f] %s\n", mine[i].Dimension, mine[i].Key, cur.Confidence, cur.Value)
			shownFull[facetKey(mine[i].Dimension, mine[i].Key)] = true
		}
		return b.String(), shownFull, len(mine), len(mine)
	}

	// Full-prose tier: facets that have changed before, most recently changed first. Sorted
	// on a copy of the slice order so the index block below stays in its original order.
	changed := make([]int, 0, len(mine))
	for i := range mine {
		if len(mine[i].Versions) > 1 {
			changed = append(changed, i)
		}
	}
	sort.SliceStable(changed, func(a, b int) bool {
		ra := lastRecordedAt(mine[changed[a]])
		rb := lastRecordedAt(mine[changed[b]])
		if ra != rb {
			return ra > rb // most recently changed first
		}
		// Deterministic tiebreak so the prompt is stable across runs (and so a content
		// signature over it would be meaningful).
		return facetKey(mine[changed[a]].Dimension, mine[changed[a]].Key) <
			facetKey(mine[changed[b]].Dimension, mine[changed[b]].Key)
	})
	if topFull > 0 && len(changed) > topFull {
		changed = changed[:topFull]
	}
	for _, i := range changed {
		shownFull[facetKey(mine[i].Dimension, mine[i].Key)] = true
	}

	// Render the index lines BEFORE the header, so the header can tell the truth about
	// whether anything was actually clipped.
	var lines strings.Builder
	anyClipped := false
	for i := range mine {
		cur := mine[i].Current()
		val, clipped := clipRunes(cur.Value, clip)
		anyClipped = anyClipped || clipped
		fmt.Fprintf(&lines, "%s/%s [%.2f] %s\n", mine[i].Dimension, mine[i].Key, cur.Confidence, val)
	}

	var b strings.Builder
	if anyClipped {
		fmt.Fprintf(&b, priorIndexHeaderClipped, len(mine), clip)
	} else {
		fmt.Fprintf(&b, priorIndexHeaderWhole, len(mine))
	}
	b.WriteString(lines.String())
	if len(changed) > 0 {
		fmt.Fprintf(&b, priorFullHeader, len(changed))
		for _, i := range changed {
			cur := mine[i].Current()
			fmt.Fprintf(&b, "%s/%s [%.2f] %s\n", mine[i].Dimension, mine[i].Key, cur.Confidence, cur.Value)
		}
	}
	if anyClipped {
		b.WriteString(priorClipRule)
	}
	return b.String(), shownFull, len(mine), len(changed)
}

// renderNamedFacets renders just the named facets in FULL — the re-ask payload for the
// blind-close guard. Bounded by construction: the caller only defers facets the model itself
// asserted a contradiction against.
func renderNamedFacets(facets []store.Facet, lensName string, want map[string]bool) string {
	var b strings.Builder
	n := 0
	for i := range facets {
		if facets[i].Lens != lensName {
			continue
		}
		cur := facets[i].Current()
		if cur == nil || !want[facetKey(facets[i].Dimension, facets[i].Key)] {
			continue
		}
		fmt.Fprintf(&b, "%s/%s [%.2f] %s\n", facets[i].Dimension, facets[i].Key, cur.Confidence, cur.Value)
		n++
	}
	return fmt.Sprintf("FACETS YOU ASSERTED A CONTRADICTION AGAINST — FULL CURRENT VALUE (%d).\n"+
		"You saw these truncated. Re-judge against the full text: emit contradicts_prior: true ONLY if\n"+
		"the evidence genuinely conflicts with what you can now read in full; otherwise reinforce it.\n", n) + b.String()
}

// clipRunes truncates at a RUNE boundary, not a byte one. RenderChunks was deliberately fixed
// the same way (internal/platform/registry.go): a byte budget is ~3x tighter for CJK, and this
// archive is part-Chinese, so a byte clip would both under-fill and risk splitting a rune.
func clipRunes(s string, clip int) (string, bool) {
	if clip <= 0 || utf8.RuneCountInString(s) <= clip {
		return s, false
	}
	n := 0
	for i := range s {
		if n == clip {
			return s[:i] + clipMarker, true
		}
		n++
	}
	return s, false
}

// truncationEchoed reports whether a returned value contains the clip marker — i.e. the model
// copied a truncated stub back as a value. Left unguarded that is corrupting: with
// contradicts_prior set, applyFacet's change branch closes the good full-text version and
// makes the stub current.
func truncationEchoed(v string) bool { return strings.Contains(v, clipMarker) }

func facetKey(dimension, key string) string { return dimension + "|" + key }

// lastRecordedAt is the recorded_at of a facet's newest version — when the reviewer last wrote
// it, which is the ordering that answers "changed most recently".
func lastRecordedAt(f store.Facet) string {
	if len(f.Versions) == 0 {
		return ""
	}
	return f.Versions[len(f.Versions)-1].RecordedAt
}
