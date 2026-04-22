# Agent Brief — planner (Phase 1)

The orchestrator dispatches this agent once per `rdd` run, during Phase 1,
after the operator has selected a ROADMAP sub-item (or one was passed via
argument). The agent decides whether that sub-item fits a single PR and, if
not, proposes a decomposition.

## Preferred `subagent_type`

`oh-my-claudecode:planner` (fallback: `general-purpose`).

## Input (pass in the agent prompt)

- **Sub-item id and title.**
- **Relevant ROADMAP section** (the `### M#` block containing the chosen
  sub-item, verbatim).
- **Repository snapshot**: first level of the repo tree and paths of
  relevant existing files (e.g. `Makefile`, `.github/workflows/*`,
  `docs/*`). This is metadata so the planner can compare the ask against
  the current state.
- **Prior LESSONS excerpt** (`docs/LESSONS.md` entries for the same
  milestone, if any) — so the planner does not propose decomposition that
  the project has already rejected.

## Heuristic: "fits one PR"

A sub-item fits one PR if, in the planner's judgement, **all** of the
following hold:

1. Estimated implementation ≤ ~1 engineer-day of focused work.
2. Touches ≤ ~15 files (code + tests + configs + docs combined).
3. Represents a single coherent concern (no "and also" in the title).
4. No ambiguous dependency on another unmerged sub-item in the same
   milestone.

If any condition fails, the sub-item is too large.

## Output contract (exact JSON)

```json
{
  "fits": true,
  "reason": "single Makefile + CI matrix entry; ≤ 6 files; ≤ 0.5 day",
  "decomposition": null
}
```

or, when decomposition is needed:

```json
{
  "fits": false,
  "reason": "Mixed: Makefile + Go toolchain + TS toolchain are three coherent concerns, each deserving its own PR",
  "decomposition": [
    { "id": "M1.1.a", "title": "Go module layout and top-level Makefile" },
    { "id": "M1.1.b", "title": "Go quality stack (golangci-lint + govulncheck)" },
    { "id": "M1.1.c", "title": "TypeScript quality stack (tsc + eslint + vitest)" }
  ]
}
```

Rules for decomposition:
- Ids follow the `<parent>.a`, `<parent>.b`, ... convention. Use lowercase
  letters.
- Each sub-item must itself pass the "fits one PR" heuristic.
- Decomposition must be **partitional**: union covers the parent, intersections are empty.
- Titles are imperative, ≤ 80 chars.

## Hard rules

- Do not speculate about future milestones; only assess the one sub-item
  passed in.
- Do not write files. Output JSON and nothing else.
- Do not propose decomposition if the sub-item fits — return `fits: true`
  even if you see ways the work "could" be split.
