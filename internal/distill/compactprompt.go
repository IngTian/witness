package distill

// compactPrompt lives in Go, not in the lens registry, for the same reason the prior-facet
// block headers do: lens prompt files are copied into an archive once at seed time and never
// updated, so text authored there provably never reaches an existing install. Compaction is
// also structural rather than domain-specific — it asks "are these the same trait", which does
// not change between a coding lens and a trading lens — so there is nothing per-lens to author.
//
// ponytail: one shared prompt, no per-lens override. Add a lenses/<name>/compact.md seam the
// first time a real lens needs different grouping judgment, not before.
const compactPrompt = `You are consolidating a behavioural profile. You are given every facet in ONE dimension
of ONE lens. Each facet is a claim about one person, mined from their work sessions over
time.

The profile has a problem: it only ever grew. Nothing ever removed or combined a facet, and
nothing ever asked whether two facets were the same thing. So it has accumulated many facets
that restate each other in different words, plus some that were never really traits at all.

YOUR GOAL: the smallest set of facets that still says everything the current set says.
Not a shorter profile — an equally complete one with less redundancy. If merging two facets
would lose a real distinction, do not merge them. If a facet is the only thing carrying some
piece of the picture, keep it, however narrow it is.

Return a JSON array of actions. Only these two:

{"action": "merge", "dimension": "<the dimension>", "key": "<survivor key>",
 "value": "<rewritten value>", "absorb": ["<key>", "<key>", ...],
 "confidence": <0.0-1.0>, "why": "<one line>"}

{"action": "retire", "dimension": "<the dimension>", "key": "<key>", "why": "<one line>"}

MERGE — several facets that are ONE underlying trait.

  Use it when the facets would be described the same way by someone who knew the person:
  they are paraphrases, or they are specific instances of one general disposition.

  "key" is the survivor. It may be one of the absorbed keys, or a NEW snake_case key you
  invent when the right name for the combined trait is more general than any of the originals.
  A new key is the better choice when you are abstracting rather than deduplicating.

  "absorb" lists at least two keys to fold in. Do not include the survivor in it.

  "value" is the hard part. REWRITE it. It must carry every distinct thing the absorbed
  facets contributed, in the register of the originals — specific and behavioural, not a
  summary of a summary. Do not concatenate the old values. Do not drop a nuance because it
  only appeared in one of them. If you cannot write a single value that keeps everything,
  that is strong evidence these are not one trait: do not merge them.

RETIRE — a facet that should not be in a profile at all.

  Legitimate reasons: it restates something a single other facet already says completely
  (and you are not merging it); it describes one incident rather than a pattern; it is an
  artifact of how the session was recorded rather than a trait of the person; it is so
  generic it would be true of anyone.

  NOT legitimate: low confidence alone, a small number of supporting observations alone, or
  being old. A quiet trait is still a trait. Absence of recent evidence is not evidence of
  absence — that is why it has a confidence score rather than an expiry.

JUDGMENT

  Be conservative. Doing nothing to a facet is always safe; a wrong merge destroys a
  distinction and a wrong retirement drops knowledge. Returning an empty array is a valid and
  correct answer for a dimension that is already tight.

  The signals you are given: "versions" is how many times this facet's value has changed — a
  facet with several versions is carrying a real arc over time and is usually worth keeping
  as its own node. "confidence" and "last_seen" tell you how well-supported and how recent it
  is; weigh them, but see the NOT-legitimate list above.

  You may only act on the keys shown in this batch. Do not reference a facet that is not
  listed. Do not act on another dimension.

Output ONLY the JSON array. No prose, no code fence.`
