# Phase 4 — Pre-PR Review Loop

Bounded loop that runs after the `executor` reports Phase 3 complete and
before the PR is opened. Alternates `code-reviewer` (per
`references/agent-briefs/code-reviewer.md`) and `executor` in fixer mode (per
`references/agent-briefs/executor.md`).

## Severity contract

The `code-reviewer` agent returns `{blocker, important, nit}` per its brief in
`references/agent-briefs/code-reviewer.md` (JSON shape + severity
definitions). The loop treats `blocker` and `important` as loop-blocking;
`nit` is moved to the TASK's `## Follow-up` section and never fixed in this
loop.

## Pseudocode

```text
MAX_ITERATIONS = 5
previous = null

for i in 0..MAX_ITERATIONS-1:
    review = dispatch(code-reviewer, brief={
        task_path: TASK_PATH,
        acceptance_criteria: <list>,
        test_plan: <list>,
        diff_vs_main: <`git diff main...HEAD`>,
        previous_comments: previous
    })

    current_blockers = review.blocker + review.important
    current_nits     = review.nit

    if current_blockers == []:
        append nit items to TASK.Follow-up
        append progress log entry: "Phase 4 converged at iteration i"
        return CONVERGED

    if previous != null and current_blockers == previous:
        escalate(reason="review stuck: identical comments two rounds in a row",
                 unresolved=current_blockers)

    if previous != null and len(current_blockers) > len(previous):
        escalate(reason="review diverging: comment count growing",
                 unresolved=current_blockers)

    dispatch(executor, mode=fixer, brief={
        task_path: TASK_PATH,
        comments_to_fix: current_blockers,
        instruction: "apply minimal patches per suggested_fix; keep tests green"
    })

    previous = current_blockers
    append progress log entry: "Phase 4 iteration {i}: <summary>"

escalate(reason="review exhausted 5 iterations without converging",
         unresolved=previous)
```

## Escalation handling

When `escalate(...)` is called, the orchestrator:

1. Stops the loop.
2. Prints to the operator:

   ```
   Phase 4 escalation — <reason>

   Iterations completed: <i>
   Current unresolved blocker+important: <count>
   Items:
     <list: file:line — rationale>

   Diff summary (files changed since Phase 3 start):
     <short list from `git diff --stat main...HEAD`>

   Options:
     - continue   I'll run the next iteration anyway
     - reframe    we edit acceptance criteria / test plan and restart from Phase 3
     - abort      mark TASK cancelled, keep branch for inspection

   Which? (continue / reframe / abort)
   ```

3. Acts on the operator's reply:
   - `continue` — run one more iteration, then re-evaluate under the same
     rules (stale/convergence/budget).
   - `reframe` — open Gate 2 again with current TASK file preloaded; operator
     edits acceptance criteria or test plan; orchestrator re-dispatches
     executor from Phase 3 with the updated TASK.
   - `abort` — set TASK `cancelled`, leave branch intact, stop.

## Progress log format (Phase 4 entries)

- `### YYYY-MM-DD HH:MM — Phase 4 iteration <i>`: one line per iteration with
  counts of blocker/important/nit and the fix dispatch outcome.
- `### YYYY-MM-DD HH:MM — Phase 4 converged`: final entry with count of nits
  deferred.
- `### YYYY-MM-DD HH:MM — Phase 4 escalated`: on escalation, with reason.
