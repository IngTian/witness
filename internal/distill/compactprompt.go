package distill

// The compaction prompts: one per stage, plus the regroup round between them.
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
// All three live in Go, not in the lens registry, for the same reason the review prompt's block
// headers do: lens prompt files are copied into an archive once at seed time and never updated,
// so text authored there provably never reaches an existing install. Compaction is also
// structural rather than domain-specific — "are these the same trait" does not change between a
// coding lens and a trading lens — so the wording names the lens's SUBJECT rather than assuming a
// person (#199 G001).
//
// ponytail: one shared set of prompts, no per-lens override. Add a lenses/<name>/compact.md
// seam the first time a real lens needs different grouping judgment, not before.

// STAGE 1 — grouping. Names only, so the reply stays small and the call stays fast.
const compactGroupPrompt = `You are consolidating a profile. You are given every facet in ONE dimension of ONE lens. A
lens tracks one subject — usually a person, but it can be a market, a project or anything else —
and each facet is a claim about that subject, mined from its records over time.

The profile only ever grew: nothing ever removed or combined a facet, and nothing ever asked
whether two facets were the same thing. So what you are looking at is NOT this many traits. It
is a handful of real underlying dispositions — on the order of ten — each described many times
from different angles, because every record that was mined wrote its own description without checking
whether the trait was already recorded.

YOUR ONLY JOB IN THIS PASS IS GROUPING. Decide which facets describe the same underlying
disposition. Do NOT write merged values — the wording is a separate later step, and a long reply
here will be cut off. Emit only names and a one-line reason.

Expect groups of EIGHT or TWELVE facets, not pairs. Aim to end with roughly one tenth as many
facets as you were shown, and no more than fifteen.

Work in this order: first decide what the few real dispositions in this dimension ARE, then
assign every facet to one of them. Do not start by looking for similar pairs.

Judge by MECHANISM: two facets are one disposition if the same underlying thing about the subject
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
  incident rather than a pattern, an artifact of how the record was captured, something true of
  any subject, or a facet whose own text says its content moved elsewhere. Low confidence, few
  observations and age are NOT reasons on their own.

  A facet carrying a disposition nothing else covers should simply be left out of the array.

Output ONLY the JSON array. No prose, no code fence.`

// REGROUP — joins what round 1's batches formed separately (Compactor.regroup). Merges only, and
// none of stage 1's "one tenth" or "groups of eight" targets: the units are already consolidated,
// so a quota here would push distinct traits together.
const compactRegroupPrompt = `You are finishing the consolidation of ONE dimension of ONE lens. A lens tracks one subject —
usually a person, but it can be a market, a project or anything else — and each facet is a claim
about that subject.

The dimension was too large for one call, so its facets were grouped in separate batches that
could not see each other. You are given every UNIT that grouping left: a trait one batch formed (its
key and why it was formed), or a facet no batch combined with anything (its key and its claim).
So the same disposition may appear more than once — formed independently in two batches, or as a
lone facet that belongs to a trait another batch formed.

YOUR ONLY JOB IS TO JOIN THOSE. Merge units only when the same underlying mechanism produces them.
Units that are merely related, or different mechanisms in the same area, stay apart. Whatever you
leave out is kept exactly as it is, so when unsure, leave it out. Do not write values; that is a
later step.

Return a JSON array of merges:

{"action": "merge", "dimension": "<the dimension>", "key": "<survivor key>",
 "absorb": ["<unit key>", ...], "why": "<one line>"}

RULES

  "key" is the survivor: one of the units being merged, or a NEW snake_case key naming the
  disposition, never any other existing key. "absorb" never contains "key".

  A merge joins at least TWO units: everything in "absorb", plus "key" when "key" is a unit.

  Every key in "absorb" must be one of the units you were given, and each may be named at most ONCE
  across the whole array.

  Merge only. Nothing is retired in this pass.

  Your "why" is RECORDED PERMANENTLY against every facet the merge touches. Name the shared
  mechanism, not the similarity.

If no units are the same disposition, return [].

Output ONLY the JSON array. No prose, no code fence.`

// STAGE 2 — writing one merged value. Prose out, not JSON: a 4,000-char value with a stray
// quote or newline would break a JSON parse and lose the whole call for no benefit.
const compactWritePrompt = `You are writing ONE facet of a profile. You are given several existing facets that
have been judged to describe the SAME underlying disposition, and your job is to write the
single facet that replaces all of them.

This is a consolidation, not a summary. The facets you are given are about to be archived and
this value becomes the only one anyone reads.

WHAT MUST SURVIVE. Every distinct thing any input facet contributes:

  - every concrete instance and example, with its detail intact
  - every observation id (the obs_... references) exactly as written
  - every quoted phrase, in the source's own words
  - every date, count and measurement, written exactly as the input writes it (2026-08-13 stays
    2026-08-13: a date that loses its year is a different fact)
  - every COST, limit, caveat or provenance note
  - every falsifier — the conditions that would raise, lower or close the claim

If an input says something only once, it still has to appear. Dropping a detail because it is
mentioned by only one facet is the single way this task fails.

LENGTH — AIM FOR THE UNION, NOT THE SUM.

  Ceiling: never longer than the facets you were given put together, and shorter wherever they
  restate each other. Floor: it must not be SHORTER than the longest one — that would mean you
  dropped content, and it will be rejected. There is no other target: do not pad, and do not
  cut evidence to reach one.

  State each distinct thing ONCE. Do not restate one mechanism in four phrasings because four
  facets did. Do not keep four separate "why this matters" passages that make the same point.
  Do not re-explain the trait before each instance.

  What you must NOT drop to make it shorter: any concrete instance, any observation id, any
  quoted phrase, any date or measurement, any cost or falsifier. Those are the irreducible
  content. Cut repeated DESCRIPTION, never evidence.

STRUCTURE. Organise a long value so it stays readable: name the disposition in the first
sentence, then number or label the distinct faces of it, then any cost, limit or falsifier the
inputs state. Do not write a falsifier, cost or recommendation (how-to-act advice included)
that no input makes: it would be an unsupported claim, so a section with nothing from the inputs
to put in it is left out. Keep the register of the inputs — specific and behavioural, citing
evidence — not abstract characterisation. Write it as a standalone facet for a reader who never
sees the facets you were given: the value and its headings never mention "the inputs", the
source facets or this merge.

STAY IN YOUR LANE. If you are shown other traits from the same dimension, they are being written
by other calls. Do not describe, generalise about or expand into their mechanisms: overlapping
with a sibling re-creates the redundancy this process exists to remove. But everything in the
facets YOU were given stays in your value, even where it also fits a sibling: those facets are
archived when you finish and no other call sees them.

OUTPUT FORMAT — two parts, separated by a line containing only three dashes:

CLAIM: <one or two sentences: what this trait IS, stated plainly>
---
<the full value, as described above>

The CLAIM is what gets sent to every future review call, so it has to stand alone: name the
disposition and its mechanism in a way that someone could judge new evidence against. No
instances, no observation ids, no falsifiers, no cross-references — those live in the full value
below. Think of it as the sentence you would use to tell someone what this trait is.

Everything after the dashes is the complete value: the instances, the evidence, and whatever
costs and falsifiers the inputs state, all of it, exactly as instructed above. Nothing is
dropped because the claim exists; the claim is an additional short handle, not a replacement.

Start your reply with "CLAIM:". No JSON, no code fence, no preamble, no commentary about what you did.`
