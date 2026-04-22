# Phase 6 — PR-Fix Loop

Bounded loop that runs after the PR is opened (Phase 5) and before Gate 3.
Polls `gh` until all checks pass and no unresolved blocker/important comments
remain; dispatches the `executor` (fixer mode) when there is something to
fix.

## Loop signals

1. **Check status** — from `gh pr checks <pr-number> --json name,status,conclusion`.
   A check is failing if `status == "completed"` and
   `conclusion in {failure, cancelled, timed_out}`.

2. **Unresolved comments** — thread-level resolution state is not exposed
   by `gh pr view --json comments,reviews`. Query it via GraphQL:

   ```bash
   gh api graphql -F owner='<owner>' -F repo='<repo>' -F pr=<pr-number> -f query='
     query($owner:String!,$repo:String!,$pr:Int!){
       repository(owner:$owner,name:$repo){
         pullRequest(number:$pr){
           reviewThreads(first:100){
             pageInfo{hasNextPage,endCursor}
             nodes{
               isResolved
               comments(first:1){nodes{id,body,author{login}}}
             }
           }
         }
       }
     }'
   ```

   Paginate with `after:"<endCursor>"` while `hasNextPage == true`.

   Each thread is classified by the body of its first comment:
   - `blocker` if body begins with `BLOCKER:` or `[blocker]`, or the
     reviewer left a `CHANGES_REQUESTED` review;
   - `important` if body begins with `IMPORTANT:` or `[important]`;
   - otherwise `nit` (including bot comments from standard linters).

   `resolved` is `isResolved == true` on the thread node.

   Only `blocker` and `important` are loop-blocking.

## Pseudocode

```text
MAX_ITERATIONS = 5
CHECK_TIMEOUT_MINUTES = 30
previous_fingerprint = null

for i in 0..MAX_ITERATIONS-1:
    wait_until_checks_report_or_timeout(CHECK_TIMEOUT_MINUTES)
        # polls `gh pr checks` every 30s; timer starts from the last push sha
        # if any check is still in a non-terminal state (anything other than
        # `completed` — e.g. `queued`, `pending`, `in_progress`, `waiting`,
        # `requested`) after 30 min, escalate immediately with
        # reason="pr-fix CI timeout: <check-name> (<status>)"

    checks   = gh pr checks <pr> --json name,status,conclusion,link
        # `link` points to .../actions/runs/<run-id>/job/<job-id>; extract
        # <run-id> to feed `gh run view --log-failed` for each failing check
    threads  = gh api graphql ... reviewThreads(first:100){isResolved, comments}  # see §Loop signals
    failing  = [c for c in checks if failing(c)]
    unresolved_blockers = [t for t in threads
                           if severity(t) in {blocker, important}
                           and not t.isResolved]

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

    # For each failing check, parse the run id out of its `link` field
    # (https://github.com/.../actions/runs/<RUN_ID>/job/<JOB_ID>) and fetch
    # log snippets via `gh run view <RUN_ID> --log-failed`.
    dispatch(executor, mode=pr-fixer, brief={
        task_path: TASK_PATH,
        pr_number: <n>,
        failing_checks: failing + <log snippets via `gh run view <run-id> --log-failed` per check>,
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
