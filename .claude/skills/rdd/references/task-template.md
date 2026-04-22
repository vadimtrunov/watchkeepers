# TASK File Template

Every `rdd` run creates exactly one `TASK-<roadmap-id>-<slug>.md` file in the
repo root at the start of Phase 2. The file is gitignored (`TASK-*.md` in
`.gitignore`) and deleted at the end of Phase 7.

## Filename

Pattern: `TASK-<roadmap-id>-<slug>.md`

- `<roadmap-id>` — e.g. `M1.1`, `M3`, `M1.9.a`. Dots kept.
- `<slug>` — kebab-case, from the sub-item title, max 40 chars.

Examples:

- `TASK-M1.1-monorepo-layout.md`
- `TASK-M1.9-ci-pipeline.md`
- `TASK-M2-keep-storage.md`

## Template (exact content to write)

```markdown
# TASK <roadmap-id> — <title>

**ROADMAP**: docs/ROADMAP-<phase>.md §M# → <roadmap-id>
**Created**: YYYY-MM-DD
**Status**: in-progress
**Branch**: rdd/<slug>
**PR**: <empty until Phase 5>

## Scope

<one paragraph: what is included and what is explicitly NOT included>

## Acceptance criteria (approved at Gate 2)

- [ ] AC1: <criterion>
- [ ] AC2: <criterion>

## Test plan (approved at Gate 2)

- [ ] Happy: <case>
- [ ] Edge: <case>
- [ ] Negative: <case>
- [ ] Security / isolation: <case>

## Plan (implementation steps)

- [ ] Step 1 — <action>
- [ ] Step 2 — <action>

## Progress log

<!-- append-only, one entry per phase transition -->

## Follow-up (nits deferred from review)

<!-- empty at start; review loop appends here -->
```

## Status transitions

- `in-progress` — created at Phase 2, active through Phase 6.
- `blocked` — set by the orchestrator when a phase escalates and the operator
  chooses `continue` to investigate without moving on.
- `cancelled` — set when the operator chooses `abort` on an escalation or
  rejects at Gate 3.
- `merged` — **transient**: set right before deletion at Phase 7 step 6.
  Because the file is then deleted, this status is observable only in the
  Progress log of the deleted file (which lives in git history of the branch
  if commits on the branch touched the file — typically they do not, since
  the file is gitignored).

For practical resume purposes, only `in-progress` and `blocked` matter.

## Progress log entries

Each entry is one Markdown heading + a short paragraph. Written by the
orchestrator, not by agents, at each phase transition.

Example:

```markdown
### 2026-04-22 14:32 — Phase 2 complete, Gate 2 passed

Acceptance criteria: 3. Test plan: 5 cases. Ready for Phase 3.

### 2026-04-22 15:10 — Phase 3 complete

executor dispatched: opus. Commits: 4 (c1b2d3e…ffeebb7). Files: 9.

### 2026-04-22 15:24 — Phase 4 iteration 1

code-reviewer: 0 blocker, 2 important, 3 nit. Dispatching fixer.

### 2026-04-22 15:31 — Phase 4 converged

2 important resolved in 1 fix iteration. 3 nits moved to Follow-up.
```

## Field precision

- `**ROADMAP**` line includes the `§` symbol and the full id path; if the
  item was decomposed, use the leaf id (e.g. `§M1 → M1.9 → M1.9.a`).
- `**Status**` must be exactly one of `in-progress`, `blocked`, `cancelled`,
  `merged`.
- `**Branch**` is set at Phase 3; empty between Phase 2 creation and Phase 3
  checkout.
- `**PR**` is set at Phase 5 (PR URL from `gh pr create`); empty before.
