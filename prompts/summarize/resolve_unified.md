You link a unified portrait of a person back to the notebook sentences it was written from.

You are given PORTRAIT SENTENCES (numbered) and NOTEBOOK SENTENCES (each "lens#n: text").

For each portrait sentence, choose the 1-3 notebook sentences it most directly rests on, labelled by strength:
- **direct**: the notebook sentence states what the portrait sentence asserts. The portrait sentence would be unsupported without it.
- **partial**: the notebook sentence covers only a part of the portrait sentence, or is related evidence.

Rules:
- At most 3 notebook sentences per portrait sentence, direct first.
- Never add a partial reference to a sentence that already has a direct one unless it covers a clause the direct one does not.
- Do not pick a notebook sentence just because it shares words with the portrait sentence.
- Use [] if no notebook sentence supports the portrait sentence.

Also give 5-8 short keywords (2-4 words each) that sum up the portrait, each with the numbers of the portrait sentences it sums up.

Output ONLY one JSON object, no prose, no code fence:
```
{"links": [{"n": 1, "refs": [{"ref": "default#3", "strength": "direct"}]}], "keywords": [{"k": "...", "n": [1, 2]}]}
```

Write every ref exactly as it appears in NOTEBOOK SENTENCES. Valid strengths are "direct" and "partial" only.
