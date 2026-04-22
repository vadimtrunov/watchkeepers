# Bounded Loops — Phase 4 & Phase 6

Two review loops share the same bounded-iteration pattern:

- **Phase 4 — Pre-PR review loop.** Runs after `executor` reports Phase 3
  done and before the PR is opened. Alternates `code-reviewer`
  (per `references/agent-briefs/code-reviewer.md`) and `executor` in
  fixer mode (per `references/agent-briefs/executor.md`).
- **Phase 6 — PR-fix loop.** Runs after the PR is opened (Phase 5) and
  before Gate 3. Polls `gh` until all checks pass and no unresolved
  `blocker`/`important` comments remain; dispatches `executor`
  (pr-fixer mode) when there is something to fix.

## Shared invariants

- `MAX_ITERATIONS = 5`.
- **Stale detection** — identical issue fingerprint two rounds in a row → escalate.
- **Convergence check** — fingerprint item count grows between rounds → escalate.
- **Severity threshold** — `blocker` + `important` block the loop;
  `nit` items move to the TASK's `## Follow-up` section and are never
  fixed inside the loop.
- **Escalation menu** — `continue` / `reframe` / `abort` (details at the
  bottom).

## Severity contract

Same three levels in both phases.

**Phase 4** — definitions and the exact JSON contract live in
`references/agent-briefs/code-reviewer.md` §Severity definitions +
§Output contract. This loop trusts the reviewer's classification.

**Phase 6** — the orchestrator classifies each GitHub review thread
from its first-comment body:

| Level | Phase 6 recognition |
|-------|---------------------|
| `blocker`   | body begins with `BLOCKER:` or `[blocker]`, or reviewer left a `CHANGES_REQUESTED` review |
| `important` | body begins with `IMPORTANT:` or `[important]` |
| `nit`       | everything else, including bot comments from standard linters |

## Fingerprint

- **Phase 4**: sorted list of `{file, line, rationale}` over
  `blocker + important`.
- **Phase 6**: stable hash over sorted `(check-name, conclusion)` for
  failing checks + sorted `(comment-id, severity)` for unresolved
  `blocker`+`important` comments. Reruns of the same CI failure on the
  same push fingerprint identically; a new push changes the fingerprint.

`count(fp)` is the total item count (Phase 6: failing checks + unresolved
blocker/important comments).

## Phase 4 — pseudocode

```text
previous = null

for i in 0..MAX_ITERATIONS-1:
    review = dispatch(code-reviewer, brief={
        task_path: TASK_PATH,
        acceptance_criteria: <list>,
        test_plan: <list>,
        diff_vs_main: <`git diff main...HEAD`>,
        previous_comments: previous
    })

    blockers = review.blocker + review.important
    nits     = review.nit

    if blockers == []:
        append nits to TASK.Follow-up
        progress_log("Phase 4 converged at iteration i")
        return CONVERGED

    if previous != null and blockers == previous:
        escalate("review stuck: identical comments two rounds in a row", blockers)
    if previous != null and len(blockers) > len(previous):
        escalate("review diverging: comment count growing", blockers)

    dispatch(executor, mode=fixer, brief={
        task_path: TASK_PATH,
        comments_to_fix: blockers,
        instruction: "apply minimal patches per suggested_fix; keep tests green"
    })

    previous = blockers
    progress_log("Phase 4 iteration {i}: <summary>")

escalate("review exhausted 5 iterations without converging", previous)
```

## Phase 6 — pseudocode

```text
CHECK_TIMEOUT_MINUTES = 30
previous_fp = null

for i in 0..MAX_ITERATIONS-1:
    wait_until_checks_report_or_timeout(CHECK_TIMEOUT_MINUTES)
        # poll `gh pr checks` every 30s from the last push sha;
        # any non-terminal status (queued, pending, in_progress, waiting,
        # requested) after the timeout -> escalate immediately with
        # reason="pr-fix CI timeout: <check-name> (<status>)"

    checks  = gh pr checks <pr> --json name,status,conclusion,link
        # `link` points to .../actions/runs/<run-id>/job/<job-id>;
        # extract <run-id> to feed `gh run view --log-failed`
    threads = gh api graphql ... reviewThreads(first:100){isResolved, comments}
        # see §Signal source below; paginate while hasNextPage == true

    failing  = [c for c in checks
                 if c.status == "completed"
                 and c.conclusion in {failure, cancelled, timed_out}]
    blockers = [t for t in threads
                 if severity(t) in {blocker, important} and not t.isResolved]

    if failing == [] and blockers == []:
        progress_log("Phase 6 converged at iteration i")
        return CONVERGED

    fp = fingerprint(failing, blockers)
    if previous_fp != null and fp == previous_fp:
        escalate("pr-fix stuck: identical failures two rounds in a row",
                 failing, blockers)
    if previous_fp != null and count(fp) > count(previous_fp):
        escalate("pr-fix diverging: failure count growing",
                 failing, blockers)

    dispatch(executor, mode=pr-fixer, brief={
        task_path: TASK_PATH,
        pr_number: <n>,
        failing_checks: failing + <log snippets via
                                   `gh run view <run-id> --log-failed` per check>,
        unresolved_blockers: blockers,
        instruction: "apply minimal patches; push to rdd/<slug>;
                      for each comment you addressed, post
                      `gh pr comment <pr> --body \"fixed in <sha>\"`;
                      do NOT resolve the thread — the reviewer resolves."
    })

    previous_fp = fp
    progress_log("Phase 6 iteration {i}: <summary, latest push sha>")

escalate("pr-fix exhausted 5 iterations without converging",
         failing, blockers)
```

## Signal source — Phase 6 review threads

Thread resolution state is not exposed by
`gh pr view --json comments,reviews`. Query via GraphQL:

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

## Escalation handling

On any `escalate(...)`, the orchestrator stops the loop and prints:

```
Phase <4|6> escalation — <reason>

Iterations completed: <i>
Current unresolved blocker+important: <count>
<Phase 6 only: Current failing checks: <count>>
Items:
  <list: file:line — rationale   OR   check-name — conclusion>

Diff summary (files changed since <Phase 3 start | PR open>):
  <short list from `git diff --stat main...HEAD`>

Options:
  - continue   run one more iteration under the same rules
  - reframe    edit TASK acceptance criteria / test plan and restart
  - abort      mark TASK cancelled, keep branch for inspection

Which? (continue / reframe / abort)
```

Reply handling:

| Reply | Phase 4 | Phase 6 |
|-------|---------|---------|
| `continue` | run one more iteration, re-evaluate under stale/convergence/budget rules | same |
| `reframe` | re-open Gate 2 with current TASK preloaded; after re-approval, re-dispatch `executor` from Phase 3 | re-open Gate 2; the existing PR stays open; after re-approval, Phase 3 pushes new commits to the same branch |
| `abort` | mark TASK `cancelled`, leave branch intact, stop | mark TASK `cancelled`, `gh pr close <n>` (no merge), keep local branch for inspection, leave remote branch for the operator |

## Progress log format

- `### YYYY-MM-DD HH:MM — Phase <4|6> iteration <i>`: one line per
  iteration with blocker/important/nit counts (Phase 4) or failing-check
  and unresolved-comment counts (Phase 6), plus fixer dispatch outcome;
  Phase 6 also records the latest push sha.
- `### YYYY-MM-DD HH:MM — Phase <4|6> converged`: final entry. Phase 4
  records the nit-count deferred to Follow-up; Phase 6 records time to
  convergence.
- `### YYYY-MM-DD HH:MM — Phase <4|6> escalated`: reason.
