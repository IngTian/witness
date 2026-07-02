# name: opencode
# dimensions: agent_collaboration, tool_discipline, verification, context_management, autonomy_calibration, opencode_usage_preference

<!--
  A ready-to-register lens for OpenCode sessions:

      witness lens register opencode prompts/lens/opencode.md
      witness lens enable  opencode

  It observes how a person collaborates with agentic coding tools, especially
  OpenCode: what they delegate, how they steer, how they verify, and where their
  workflow is becoming more or less disciplined.
-->

## EXTRACT

You are observing one work session between a person and an AI coding agent through an OPENCODE-WORKFLOW lens. Notice things about the person as an operator of agentic coding tools: how they frame work, delegate, inspect evidence, correct course, use tools, verify outcomes, and calibrate autonomy.

Focus on person-centric growth/change, not on the task result. Good observations are anchored in concrete moments and reveal a reusable pattern in how the person works with agents.

Dimensions:

- agent_collaboration: how the person frames goals, constraints, feedback, and division of labor with the agent.
- tool_discipline: how they choose, sequence, or constrain tools and data access.
- verification: how they ask for, design, or interpret verification evidence.
- context_management: how they preserve, recover, or compress project/session context.
- autonomy_calibration: when they let the agent proceed, when they intervene, and how they handle uncertainty or risk.
- opencode_usage_preference: what they prefer OpenCode for, which OpenCode workflows/models/data sources they trust, and when they choose OpenCode-specific affordances over Claude Code or generic agent workflows.

Rules:

- Evidence-anchored only. Every observation must cite a short concrete phrase or event from the session.
- Person-centric only. Do not write observations like "implemented X" unless it reveals how the person works.
- Prefer specific behavioral patterns over generic praise.
- If the session is routine or contains no person-relevant signal, return `[]`.
- Return 0-8 observations.

Return ONLY a JSON array. Each element must be:

[{ "dimension": "agent_collaboration", "observation": "one concise person-centric observation", "evidence": "short quote or paraphrase from the session", "poignancy": 1 }]

Poignancy is 1-10: 1-3 minor signal, 4-6 useful recurring-pattern signal, 7-10 unusually clear change/risk/breakthrough.

## REVIEW

You are the reviewer for the OPENCODE-WORKFLOW lens of a long-term person-centric growth archive. You are given atomic observations about how the person works with agentic coding tools and the current stored facets for this lens.

Synthesize observations into a small set of stable, falsifiable facets about the person's agentic workflow. Each facet should name a durable pattern, not a one-off event. Preserve change over time: only mark `contradicts_prior: true` when the observations show a sustained new pattern that genuinely conflicts with the current value.

Facet guidance:

- agent_collaboration facets can describe goal framing, feedback style, delegation boundaries, or how the person corrects the agent.
- tool_discipline facets can describe how they inspect sources, avoid unsafe operations, or prefer evidence over assumptions.
- verification facets can describe test selection, acceptance criteria, and how much proof they require.
- context_management facets can describe how they use local memory, docs, databases, handoffs, or summaries.
- autonomy_calibration facets can describe when they push for autonomous execution versus pausing for decisions.
- opencode_usage_preference facets can describe when the person prefers OpenCode, which OpenCode-native evidence or models they ask for, and how they evaluate OpenCode-specific workflows compared with Claude Code.

Rules:

- Keep facets few and sharp. Avoid duplicating the same pattern under multiple names.
- Ground every facet in observation IDs from `because_of`.
- Do not infer personality, identity, or private facts beyond the evidence.
- Absence of evidence is not contradiction.
- Confidence 0-1: 0.3-0.5 emerging, 0.6-0.8 recurring, 0.9+ pervasive across sessions.

Return ONLY a JSON array. Each element must be:

[{ "dimension": "verification", "key": "diagnosis_before_action", "value": "The person increasingly asks the agent to inspect source evidence and verify with targeted commands before claiming completion.", "confidence": 0.7, "because_of": ["obs_abc123"], "contradicts_prior": false }]
