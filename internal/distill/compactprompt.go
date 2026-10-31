package distill

import "strings"

// The two compaction prompts, one per stage.
//
// WHY TWO. Measured on a real 105-facet dimension: one call asked to decide the grouping AND
// write every merged value emitted ~40,000 chars and was KILLED at the runner's 10-minute wall,
// for three different prompts including an explicitly aggressive one. The conservative prompt
// survived only because it merged PAIRS — 9m57s, three seconds from the same cliff. Generation
// time tracks output length, so a single call cannot merge deeply at any wording. Asking for the
// grouping ALONE returned 8,191 chars in 3m17s with merges averaging 9.2 facets, projecting an
// 87.6% reduction, and the groups matched an independent by-hand clustering of the same
// dimension. Large input is cheap; large output is what costs.
//
// Both live in Go, not in the lens registry, for the same reason the review prompt's block
// headers do: lens prompt files are copied into an archive once at seed time and never updated,
// so text authored there provably never reaches an existing install. Compaction is also
// structural rather than domain-specific — "are these the same trait" does not change between a
// coding lens and a trading lens.
//
// ponytail: one shared pair of prompts, no per-lens override. Add a lenses/<name>/compact.md
// seam the first time a real lens needs different grouping judgment, not before.

func (c *Compactor) groupPrompt() string {
	if strings.TrimSpace(c.Prompt) != "" {
		return c.Prompt // experiment/override seam, used by the strategy sweep
	}
	return compactGroupPrompt
}

func (c *Compactor) writePrompt() string {
	if strings.TrimSpace(c.WritePrompt) != "" {
		return c.WritePrompt
	}
	return compactWritePrompt
}

// STAGE 1 — grouping. Names only, so the reply stays small and the call stays fast.
const compactGroupPrompt = `You are consolidating a behavioural profile. You are given every facet in ONE dimension of
ONE lens. Each facet is a claim about one person, mined from their work sessions over time.

The profile only ever grew: nothing ever removed or combined a facet, and nothing ever asked
whether two facets were the same thing. So what you are looking at is NOT this many traits. It
is a handful of real underlying dispositions — on the order of ten — each described many times
from different angles, because every session wrote its own description without checking whether
the trait was already recorded.

YOUR ONLY JOB IN THIS PASS IS GROUPING. Decide which facets describe the same underlying
disposition. Do NOT write merged values — the wording is a separate later step, and a long reply
here will be cut off. Emit only names and a one-line reason.

Expect groups of EIGHT or TWELVE facets, not pairs. Aim to end with roughly one tenth as many
facets as you were shown, and no more than fifteen.

Work in this order: first decide what the few real dispositions in this dimension ARE, then
assign every facet to one of them. Do not start by looking for similar pairs.

Judge by MECHANISM: two facets are one disposition if the same underlying thing about the person
produces both, even in different material. A general disposition and its specific instances are
ONE disposition. Different examples of one mechanism are ONE disposition. Only a genuinely
different mechanism earns its own facet.

Return a JSON array of actions. Only these two:

{"action": "merge", "dimension": "<the dimension>", "key": "<survivor key>",
 "absorb": ["<key>", "<key>", ...], "confidence": <0.0-1.0>, "why": "<one line>"}

{"action": "retire", "dimension": "<the dimension>", "key": "<key>", "why": "<one line>"}

Your "why" is RECORDED PERMANENTLY against every facet the action touches — it is the only
explanation anyone will ever have for why these were judged one trait. Name the shared
mechanism, not the similarity.

RULES

  "key" is the survivor: either one of the absorbed keys, or a NEW snake_case key you invent
  naming the disposition itself. Prefer a new general key when abstracting several facets.

  "absorb" lists at least TWO keys and must never contain the survivor.

  Every key you name must appear in the batch, and each key may be named at most ONCE across the
  whole array.

  RETIRE is for facets that are not traits: a restatement another facet covers completely, one
  incident rather than a pattern, an artifact of how the session was recorded, something true of
  anyone, or a facet whose own text says its content moved elsewhere. Low confidence, few
  observations and age are NOT reasons on their own.

  A facet carrying a disposition nothing else covers should simply be left out of the array.

Output ONLY the JSON array. No prose, no code fence.`

// STAGE 2 — writing one merged value. Prose out, not JSON: a 4,000-char value with a stray
// quote or newline would break a JSON parse and lose the whole call for no benefit.
const compactWritePrompt = `You are writing ONE facet of a behavioural profile. You are given several existing facets that
have been judged to describe the SAME underlying disposition, and your job is to write the
single facet that replaces all of them.

This is a consolidation, not a summary. The facets you are given are about to be archived and
this value becomes the only one anyone reads.

WHAT MUST SURVIVE. Every distinct thing any input facet contributes:

  - every concrete instance and example, with its detail intact
  - every observation id (the obs_... references) exactly as written
  - every quoted phrase of the person's own words
  - every date, count and measurement
  - every COST, limit, caveat or provenance note
  - every falsifier — the conditions that would raise, lower or close the claim

If an input says something only once, it still has to appear. Dropping a detail because it is
mentioned by only one facet is the single way this task fails.

LENGTH — AIM FOR THE UNION, NOT THE SUM.

  Target: about as long as the LONGEST facet you were given, and never more than twice it. It
  must not be SHORTER than the longest one — that would mean you dropped content, and it will be
  rejected.

  That band is achievable because these facets overlap heavily. They describe ONE mechanism, and
  each was written by a separate session that did not know the others existed, so the same claim
  appears four or nine times in four or nine different wordings. The union of what they say is
  far smaller than their total length; the repetition is the bulk of it.

  So: state each distinct thing ONCE. Do not restate one mechanism in four phrasings because
  four facets did. Do not keep four separate "why this matters" passages that make the same
  point. Do not re-explain the trait before each instance.

  What you must NOT drop to hit the band: any concrete instance, any observation id, any quoted
  phrase, any date or measurement, any cost or falsifier. Those are the irreducible content and
  together they are a small fraction of the text. Cut repeated DESCRIPTION, never evidence.

STRUCTURE. Organise a long value so it stays readable: name the disposition in the first
sentence, then number or label the distinct faces of it, then the cost or limit, then the
falsifiers. Keep the register of the inputs — specific and behavioural, citing evidence — not
abstract personality description.

STAY IN YOUR LANE. If you are shown other traits from the same dimension, they are being written
by other calls. Anything belonging to one of them is theirs to cover; do not restate it here.
Overlapping with a sibling re-creates the redundancy this process exists to remove.

OUTPUT FORMAT — two parts, separated by a line containing only three dashes:

CLAIM: <one or two sentences: what this trait IS, stated plainly>
---
<the full value, as described above>

The CLAIM is what gets sent to every future review call, so it has to stand alone: name the
disposition and its mechanism in a way that someone could judge new evidence against. No
instances, no observation ids, no falsifiers, no cross-references — those live in the full value
below. Think of it as the sentence you would use to tell someone what this trait is.

Everything after the dashes is the complete value: the instances, the evidence, the costs, the
falsifiers, all of it, exactly as instructed above. Nothing is dropped because the claim exists;
the claim is an additional short handle, not a replacement.

No JSON, no code fence, no preamble, no commentary about what you did.`
