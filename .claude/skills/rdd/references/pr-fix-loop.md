# Phase 6 — PR-Fix Loop

Bounded loop that runs after the PR is opened (Phase 5) and before Gate 3.
Polls `gh` until all checks pass and no unresolved blocker/important comments
remain; dispatches the `executor` (fixer mode) when there is something to
fix.

## Loop signals

1. **Check status** — from `gh pr checks <pr-number> --json name,status,conclusion`.
   A check is failing if `status == "completed"` and
   `conclusion in {failure, cancelled, timed_out}`.

2. **Unresolved comments** — from
   `gh pr view <pr-number> --json comments,reviews --jq '...'`.
   A comment is treated as:
   - `blocker` if its body begins with `BLOCKER:` or
     `[blocker]`, or the reviewer left a `CHANGES_REQUESTED` review;
   - `important` if the body begins with `IMPORTANT:` or `[important]`;
   - otherwise `nit` (including bot comments from standard linters).

   `resolved` is true if the comment thread is marked resolved by any human
   or the commenter has since approved.

   Only `blocker` and `important` are loop-blocking.

## Pseudocode

```text
MAX_ITERATIONS = 5
CHECK_TIMEOUT_MINUTES = 30
previous_fingerprint = null

for i in 0..MAX_ITERATIONS-1:
    wait_until_checks_report_or_timeout(CHECK_TIMEOUT_MINUTES)
        # polls `gh pr checks` every 30s; timer starts from the last push sha
        # if a check is still "in_progress" after 30 min, escalate immediately
        # with reason="pr-fix CI timeout: <check-name>"

    checks   = gh pr checks <pr> --json name,status,conclusion
    comments = gh pr view <pr> --json comments,reviews
    failing  = [c for c in checks if failing(c)]
    unresolved_blockers = [c for c in comments
                           if severity(c) in {blocker, important}
                           and not resolved(c)]

    if failing == [] and unresolved_blockers == []:
        append progress log entry: "Phase 6 converged at iteration i"
        return CONVERGED

    current_fingerprint = fingerprint(failing, unresolved_blockers)
    if previous_fingerprint != null and current_fingerprint == previous_fingerprint:
        escalate(reason="pr-fix stuck: identical failures two rounds in a row",
                 failing=failing, unresolved=unresolved_blockers)

    if previous_fingerprint != null and count(current_fingerprint) > count(previous_fingerprint):
        escalate(reason="pr-fix diverging: failure count growing",
                 failing=failing, unresolved=unresolved_blockers)

    dispatch(executor, mode=pr-fixer, brief={
        task_path: TASK_PATH,
        pr_number: <n>,
        failing_checks: failing + <log snippets via `gh run view --log-failed`>,
        unresolved_blockers: unresolved_blockers,
        instruction: "apply minimal patches; push to rdd/<slug>; for each
                      comment you addressed, post `gh pr comment <pr> --body
                      \"fixed in <sha>\"`; do NOT resolve the comment
                      thread — the reviewer resolves."
    })

    previous_fingerprint = current_fingerprint
    append progress log entry: "Phase 6 iteration {i}: <summary>"

escalate(reason="pr-fix exhausted 5 iterations without converging",
         failing=failing, unresolved=unresolved_blockers)
```

## `fingerprint` definition

A stable hash over:
- sorted list of (check-name, conclusion) for failing checks;
- sorted list of (comment-id, severity) for unresolved blocker+important
  comments.

Rationale: reruns of the same CI failure on the same push should fingerprint
identically; new failures caused by a new push change the fingerprint.

## `count(fingerprint)` definition

Total number of items (failing checks + unresolved blocker+important
comments).

## Escalation handling

Identical to Phase 4 (`references/review-loop.md` §Escalation handling), with these
differences:

- The prompt reports failing checks **and** unresolved comments.
- `reframe` sends the skill back to Phase 2 (Gate 2) — because by this point
  the original acceptance criteria may have been wrong. The existing PR
  stays open while the operator edits the TASK; after Gate 2 re-approval,
  Phase 3 resumes and pushes new commits to the same branch.
- `abort` sets TASK `cancelled` and closes the PR (`gh pr close <n>`
  without merging). The branch is kept locally for inspection; the remote
  branch is left for the operator to delete.

## Progress log format (Phase 6 entries)

- `### YYYY-MM-DD HH:MM — Phase 6 iteration <i>`: failing counts, fixer
  dispatch outcome, latest push sha.
- `### YYYY-MM-DD HH:MM — Phase 6 converged`: time to convergence.
- `### YYYY-MM-DD HH:MM — Phase 6 escalated`: reason.
