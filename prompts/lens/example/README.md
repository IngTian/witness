# Example lens — a "math learner" lens

A complete, ready-to-register example lens. A lens is a **directory** of three files:

    lens.json     structured settings: name, dimensions, optional per-lens models
    extract.md    the mining (L0→L1) prompt — the whole file is the prompt
    review.md     the review (L1→L2) prompt — the whole file is the prompt

Copy this directory, rewrite the dimensions and the two prompts for your domain, then:

    witness lens register math ./example/      # copies the directory into the registry
    witness lens enable   math                 # runs it on every session

The two prompts are passed VERBATIM as the system prompt for their pass — they fully
REPLACE the built-in `default` prompts, they don't extend them — so each must be
self-contained, INCLUDING its output JSON schema. The tool appends the transcript
(extract) or the accumulated observations (review) as the user message; it injects no
schema for you.

A lens is runner-agnostic: by default it rides the global model. To pin a stronger
model just for this lens (without paying for it on every session), add an optional
`"extract_model"` / `"review_model"` to `lens.json`, or run
`witness lens set math --extract-model <model>` (empty value clears it).
