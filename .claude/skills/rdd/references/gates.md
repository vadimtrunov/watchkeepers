# Operator Gates

Three mandatory synchronous checkpoints. The orchestrator prints the prompt,
waits for the operator's reply, and acts only on explicit approval. No gate
may be skipped; if the operator is unreachable, halt.

## Gate 1 — Selection and decomposition

**When:** end of Phase 1, before any TASK file is created.

**Preconditions:**

- Preflight passed.
- Candidate list computed (all `[ ]` leaf sub-items whose dependencies are
  `[x]`).
- `planner` has returned a verdict on whether the chosen sub-item fits one
  PR, and a decomposition if it does not.

**Prompt format:**

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

**Accepted operator replies:**

- `yes` — apply ROADMAP edits (if any), commit+push them (see
  `references/roadmap-migration.md`), proceed to Phase 2.
- `revise` — operator provides specific edits (different item, different
  decomposition). Re-run Phase 1 with the new info.
- `cancel` — stop the skill, no side effects.

## Gate 2 — Approach and acceptance criteria

**When:** end of Phase 2, after TASK file is fully populated (Scope,
Acceptance criteria, Test plan, Plan), before any branch is created.

**Prompt format:**

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

**Accepted operator replies:**

- `yes` — proceed to Phase 3.
- `revise` — operator specifies which section to change; orchestrator edits
  the TASK file accordingly, then re-asks Gate 2.
- `cancel` — mark TASK `cancelled`, delete the TASK file, stop.

## Gate 3 — Merge

**When:** end of Phase 6, after the PR-fix loop has converged (all checks
green, no unresolved blocker/important comments).

**Prompt format:**

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

**Accepted operator replies:**

- `yes` — execute Phase 7 exactly as listed.
- `no` — mark TASK `cancelled`, close the PR (keep branches for inspection),
  stop.
- `defer` — stop the skill; TASK stays `in-progress`; branch and PR stay
  open. Next run with `/rdd resume` will re-enter Gate 3.

## Halting rules

- If operator reply is empty, ambiguous, or times out (operator-defined
  timeout, default none), treat as "no response" and halt without side
  effects.
- If reply is unrecognized (not in the accepted set), ask once to clarify,
  then halt on a second unrecognized reply.
- Never infer intent from similar-sounding answers. "looks good" is not
  `yes`.
