# Auto-decision rules

These rules are embedded into the ship-Agent prompt (see
`agent-prompt.md`) and into the orchestrator's pre-dispatch logic in
`SKILL.md`. They replace the two operator questions that the underlying
`ship-roadmap-item` skill normally asks.

## Phase 2 — API design fork

Read the **two most recent** entries in `docs/lessons/<family>.md` (the
family file matching the M-id, e.g. `docs/lessons/M7.md` for any
`M7.x.y.z`). If they unambiguously dictate the answer (same shape,
same sentinels, same canonical pattern across both entries) — take it.

Otherwise STOP. Return:

```json
{
  "status": "halted",
  "halt_reason": "phase2-uncertainty",
  "halt_detail": "<one-line description of the unresolved question>",
  ...
}
```

Do **not** guess. Two prior-art entries agreeing is the threshold;
one entry alone is not enough.

## Phase 7 — codex+critic fix scope

Default: apply **all** findings (Critical+Major+Minor+Nit) across both
reviewers' merged list.

Fallback: if three consecutive review cycles return the same blocker
(stale / convergence-failure detection from `ship-roadmap-item` Phase
7), fall back to **only Major+** (Critical+Major). Defer Minor+Nit
findings as follow-up bullets in the lesson entry under a `## Follow-up`
section.

If a blocker (Critical or Major) remains after the fallback — return
`halt_reason="review-blocker"`. Do not merge with a known blocker.

## Decomposition

Never decompose a roadmap item from inside the autopilot. If the
chosen leaf turns out to be aggregate (no concrete AC bullets, or
scope projects to >1000 LOC / >20 files / >1 PR) — return
`halt_reason="aggregate-needs-decomposition"` with `halt_detail`
pointing to the file and line of the M-id.

## CI fix loop

Up to three fix attempts on red CI. Each attempt:

1. Read `gh pr checks <pr>` failure output.
2. Apply minimal fix.
3. Push.
4. Wait for `gh pr checks <pr> --watch`.

After the third failure → `halt_reason="ci-red"`,
`halt_detail="<failed-check-name>: <last-error-line>"`.

## Build gate

Before push: `cd core && go build ./... && go vet ./... && go test
-race ./... -count=1` must succeed. If any step fails after the local
fix attempt → `halt_reason="build-failed"`.

## Merge

A single attempt at:

```bash
gh pr merge <pr> --squash --delete-branch
```

Any failure (conflict, branch protection, missing required check) →
`halt_reason="merge-failed"`. Do not retry; the operator must
investigate.

## Concurrency

Strict serial. The orchestrator dispatches at most one ship-Agent at
a time. The next tick does not run until the current Agent returns
its JSON. The cascade-pass writer-Agent (when it runs) executes
*before* the ship-Agent within the same tick, never in parallel.

## Operator interaction (forbidden)

The Agent must NEVER call `AskUserQuestion`, `Skill` (especially
`brainstorming`/`brainstorm`), or any other interactive tool. Any
question that cannot be auto-resolved per the rules above must surface
as `status="halted"` with the appropriate `halt_reason`.
