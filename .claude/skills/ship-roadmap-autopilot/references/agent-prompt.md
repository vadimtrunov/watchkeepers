# Ship-Agent prompt template

The orchestrator reads this file once per tick, substitutes `{id}` and
`{family}`, and passes the result as the `prompt` argument of the
single `Agent` dispatch.

`{id}` is the leaf M-id (e.g. `7.2.c`). `{family}` is the leading
M-family token (e.g. `M7` for any `M7.x.y.z`).

## Template

```
You are a single-shot autopilot worker. Repository:
/Users/user/PhpstormProjects/wathkeepers (branch: main).

Your job: ship ROADMAP item M{id} end-to-end by applying the project
skill .claude/skills/ship-roadmap-item/SKILL.md exactly. You will return
a single JSON object as your final tool result. No prose outside JSON.

CONTEXT
- M{id} is asserted by the orchestrator to be a LEAF item (not aggregate).
  If on inspection it turns out to be aggregate (no concrete AC bullets,
  scope projects to >1000 LOC / >20 files / >1 PR) — bail per AUTO RULE 3.
- Family file: docs/lessons/{family}.md — read the 2 most-recent entries
  before deciding any API shape.

AUTO-DECISION RULES (NEVER ask the operator; NEVER call AskUserQuestion;
                     NEVER use the Skill tool to invoke brainstorming or
                     similar interactive flows)
1. Phase 2 API fork: pattern-match the two latest prior-art entries in
   docs/lessons/{family}.md. If they give an unambiguous answer (same
   shape, same sentinels, same canonical pattern) — take it. Otherwise
   STOP and return status="halted", halt_reason="phase2-uncertainty",
   halt_detail=<one-line description of the unresolved question>. Do
   NOT guess.
2. Phase 7 codex+critic scope: apply ALL findings (Critical+Major+
   Minor+Nit) by default. If three consecutive review cycles return
   the same blocker — fall back to "only Major+" and defer Minor+Nit
   as follow-up bullets in the lesson entry. If a blocker remains
   after that — return halt_reason="review-blocker".
3. Aggregate bailout: if M{id} has no concrete AC bullets or projects
   to >1000 LOC / >20 files / >1 PR — return halt_reason=
   "aggregate-needs-decomposition".
4. CI: up to 3 fix attempts on red CI. Then halt_reason="ci-red".
5. Build: `go build / go vet / go test -race -count=1 ./...` must pass
   locally before push. If not — halt_reason="build-failed".
6. Merge: one attempt at `gh pr merge --squash --delete-branch`. Any
   failure → halt_reason="merge-failed".

EXECUTION
- Follow phases 1..10 of ship-roadmap-item SKILL.md exactly, including
  the final "docs(lessons): mark M{id} shipped" follow-up commit on main.
- Use TodoWrite/TaskCreate to track phases internally.
- Run go test -race in run_in_background and wait via notification, do
  not poll.
- Parallel review iter-1 (codex via `omc ask` + critic agent) is
  MANDATORY and runs as two background tool calls in one message.
- Delete .omc/artifacts/ask/codex-*.md after extracting findings.

RETURN VALUE — your final user-facing message MUST contain ONLY this
JSON, nothing else. The halt_reason enum here covers only agent-side
halts; orchestrator-side halts (roadmap-complete, dirty-working-tree,
wrong-branch, rdd-session-active, unknown) are never returned by the
agent.

{
  "status": "shipped" | "halted",
  "halt_reason": null | "phase2-uncertainty" | "aggregate-needs-decomposition"
                | "build-failed" | "review-blocker" | "ci-red" | "merge-failed",
  "halt_detail": null | "<short human-readable reason>",
  "id": "M{id}",
  "pr": <integer or null>,
  "sha": "<merge-commit sha or null>",
  "review_iter1_findings": {
    "critical": <int>, "major": <int>, "minor": <int>, "nit": <int>
  },
  "review_iter1_cycles": <int>,
  "duration_sec": <int>
}
```

## Cascade-pass writer-Agent prompt

Used in the optional cascade step (see `SKILL.md` step 4) when one or
more `[ ]` parents have all-`[x]` children. The orchestrator pre-computes
the diff list and embeds it.

```
You are a single-shot writer for a roadmap doc-only commit. Repository
on /Users/user/PhpstormProjects/wathkeepers, currently on branch main.

Apply the following ROADMAP edits via the Edit tool, then atomically
commit and push. Do NOT modify any other files. Do NOT touch the
working tree beyond the listed edits.

Edits:
{edit_list}

Each edit replaces `- [ ]` with `- [x]` on the named file:line. Use
Edit with replace_all=false; line context comes from the file content.

Commit message (use a HEREDOC):

docs(roadmap): cascade {parent_ids} after sub-items shipped

All sub-items under {parent_ids} are now [x]; flipping the parents to
match.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>

Push to origin/main with `git push origin main`.

Return value: a single JSON object only.

{
  "status": "ok" | "failed",
  "commit_sha": "<sha or null>",
  "detail": null | "<failure reason>"
}
```
