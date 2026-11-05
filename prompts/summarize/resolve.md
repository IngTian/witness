You link a written profile of a person back to the facets it was written from.

You are given PROFILE SENTENCES (numbered) and FACETS (each "dimension/key: text", given as full text, not summaries).

For each sentence, choose the 1-3 facets it most directly rests on, labelled by strength:
- **direct**: the facet fully states what the sentence asserts (or, for a compound sentence, fully states one of its clauses).
- **partial**: the facet covers only part of a claim, or is related/thematic evidence.

Rules:
- **Prefer ONE facet that states the whole sentence** over multiple facets that each cover parts.
- For a compound sentence (e.g., "X, Y and Z"), link one facet per clause ONLY if no single facet states the whole sentence. Each link is "direct" only if it fully states its own clause.
- **Check every claim of the sentence against the facets.** Leave a claim without a direct link only when no facet states it.
- When a durable facet (e.g., `goals/...`, `habits/...`) and a dated snapshot (e.g., `state/something_2026_...`) both state a claim, prefer the durable one. Use a `state/...` facet only for claims about a specific dated episode.
- At most 3 facets per sentence, direct first.
- Never add a partial facet to a sentence that already has a direct one unless it covers a clause the direct one does not.
- Do not pick a facet just because it shares words with the sentence.
- Use [] if no facet supports the sentence.

Also give 5-8 short keywords (2-4 words each) that sum up the profile, each with the numbers of the sentences it sums up.

Output ONLY one JSON object, no prose, no code fence:
{"links": [{"n": 1, "keys": [{"key": "dimension/key", "strength": "direct"}]}], "keywords": [{"k": "...", "n": [1, 2]}]}

Write every key exactly as it appears in FACETS. Valid strengths are "direct" and "partial" only.
