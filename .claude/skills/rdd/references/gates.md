# Operator Gates

Three mandatory synchronous checkpoints. The orchestrator prints the
gate prompt, waits for the operator's reply, and acts only on explicit
approval. No gate may be skipped; if the operator is unreachable, halt
without side effects.

## Summary

| Gate | When | Accepted replies |
|------|------|------------------|
| 1. Selection & decomposition | End of Phase 1, before any TASK file is created. Preconditions: preflight passed; candidate list computed (all `[ ]` leaf sub-items whose dependencies are `[x]`); `planner` has returned a verdict on whether the chosen sub-item fits one PR, and a decomposition if it does not. | `yes` → apply ROADMAP edits (if any), commit+push per `references/roadmap-migration.md`, proceed to Phase 2. `revise` → operator provides specific edits (different item, different decomposition); re-run Phase 1 with the new info. `cancel` → stop, no side effects. |
| 2. Approach & acceptance criteria | End of Phase 2, after TASK file is fully populated (Scope, Acceptance criteria, Test plan, Plan), before any branch is created. | `yes` → proceed to Phase 3. `revise` → operator specifies which section to change; orchestrator edits the TASK file, then re-asks Gate 2. `cancel` → mark TASK `cancelled`, delete the TASK file, stop. |
| 3. Merge | End of Phase 6, after the PR-fix loop has converged (all checks green, unresolved blocker+important = 0). | `yes` → execute Phase 7 exactly as listed in the prompt. `no` → mark TASK `cancelled`, close the PR (keep branches for inspection), stop. `defer` → stop the skill; TASK stays `in-progress`; branch and PR stay open; next `/rdd resume` re-enters Gate 3. |

## Gate 1

Literal prompt (angle brackets substitute at runtime):

```
Gate 1 — Selection and decomposition

I will work on: <id> — <title>
  (From <roadmap-path> §<milestone>.)

Planner verdict: <fits | too large>
<if too large, list the proposed decomposition, numbered>

ROADMAP edits I will apply before continuing:
<unified diff, or "none">

Does this match what you want? (yes / revise / cancel)
```

## Gate 2

```
Gate 2 — Approach and acceptance criteria for <id>

TASK file: <path>

Scope:
<Scope paragraph>

Acceptance criteria:
<list>

Test plan:
<list of cases>

Implementation plan:
<numbered steps>

Branch to be created: rdd/<slug>

Approve? (yes / revise / cancel)
```

## Gate 3

```
Gate 3 — Merge <PR URL>?

CI: all green
Unresolved blocker/important comments: 0
Nits / open discussions: <count, or "none">

Merging will:
1. Merge PR #<n> into main via <squash|merge|rebase> (repo default).
2. Mark <id> as [x] in <roadmap-path>, cascade parents if applicable.
3. Commit "chore(roadmap): mark <id> complete" and push to main.
4. Append Phase-7 entries to docs/LESSONS.md and .claude/skills/rdd/FEEDBACK.md.
5. Delete local branch rdd/<slug> and the remote branch.
6. Delete TASK-<id>-<slug>.md.

Proceed? (yes / no / defer)
```

## Halting rules

- Empty, ambiguous, or timed-out reply (operator-defined timeout,
  default none) → treat as "no response" and halt without side effects.
- Unrecognized reply (not in the gate's accepted set) → ask once to
  clarify; a second unrecognized reply halts.
- Never infer intent from similar-sounding answers. "looks good" is
  not `yes`.
