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

## Per-lens models (optional)

By default a lens rides the global models (`witness config set triage_model / distill_model`).
A reasoning-heavy lens like this one can pin a stronger model for itself without paying
for it on every session:

    witness lens set math --extract-model <a-capable-model>
    witness lens set math --review-model  <a-capable-model>

That writes `extract_model` / `review_model` into `lens.json`. Pass an empty value
(`--extract-model ""`) to clear it and ride the global again. A below-floor model tends
to silently produce nothing rather than error — run `witness doctor` to see the drift
count and the resolved per-lens models.
