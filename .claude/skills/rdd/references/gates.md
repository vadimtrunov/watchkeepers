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

## Auto mode

Activated when the orchestrator is invoked as `/rdd --auto …`. The
gate prompts are still printed to the transcript for audit, but the
orchestrator emits a deterministic decision instead of waiting for
the operator. Each gate has exactly one auto-`yes` rule; everything
else halts without side effects, mirroring the conservative posture of
interactive halting.

### Loop continuity

`--auto` runs Phase 1 → Phase 7 back-to-back inside a single
orchestrator turn and re-enters Phase 1 with the next ROADMAP
candidate as soon as Phase 7c (`TASK-*.md` deletion) completes. **No
ScheduleWakeup, no `/loop` wrapping, no inter-iteration sleep** — those
add 60-second-minimum delays that are pure overhead. The only place
the orchestrator is allowed to wait is Phase 6's CI poll, and that
uses an inline bash loop (`while ! gh pr checks --watch …; do …;
done`, or equivalent), not ScheduleWakeup. The orchestrator stops
when the auto-rules cannot resolve a gate (halt — see per-gate rules
below) or when the ROADMAP candidate list is empty (success —
print "ROADMAP complete" and exit).

### Gate 1 auto-decision

- **Auto-`yes`** when the planner verdict is `fits one PR` AND exactly
  one candidate sub-item is selectable (the first `[ ]` leaf whose
  dependencies are all `[x]`, top-down in the ROADMAP file).
- If the planner verdict is `too large` AND it returned a numbered
  decomposition, apply the ROADMAP edits and auto-`yes` on the FIRST
  decomposed sub-item. The remaining decomposed items become future
  candidates.
- Halt when: planner returned no verdict; planner returned `too large`
  without a decomposition; the candidate list is empty (nothing to do
  — print "ROADMAP complete" and halt); ROADMAP edits would touch
  more than the candidate's own checkbox + parent cascade.

### Gate 2 auto-decision

- **Auto-`yes`** when the TASK file is fully populated (Scope,
  Acceptance criteria with ≥1 item, Test plan with ≥1 item, Plan with
  ≥1 numbered step) AND Hard rule 6 (PR size cap) is not predicted to
  fail (rough heuristic: plan touches ≤ 5 files; if unknown, do not
  block at this gate — let the bounded review loops catch it).
- Halt when: any of Scope / Acceptance criteria / Test plan / Plan is
  empty or placeholder text; the TASK is a toggle-only TASK that would
  violate Hard rule 7.

### Gate 3 auto-decision

- **Auto-`yes`** when CI is reported all-green by `gh pr checks` AND
  unresolved blocker+important review comments = 0 (the standard
  Phase 6 exit condition is the precondition of Gate 3). Proceed
  directly into Phase 7 (writer pass, merge, cleanup).
- Halt when: CI is not all-green; unresolved blocker/important > 0;
  the bounded Phase 6 loop escalated; the merge precondition was
  computed from stale data (re-fetch once, then halt if still stale).

### Audit format for an auto-decision

After printing the gate's literal prompt, print exactly:

```
Auto-decision: yes
Reason: <one sentence quoting the rule above that authorised this>
```

Or, on halt:

```
Auto-decision: halt
Reason: <one sentence naming the specific halt condition above>
Side effects rolled back: <list, or "none">
```
