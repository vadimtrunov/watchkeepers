# Ship-Agent prompt template

The orchestrator reads this file once per tick, substitutes `{id}`,
`{family}`, and `{target_branch}`, and passes the result as the
`prompt` argument of the single `Agent` dispatch.

`{id}` is the leaf M-id (e.g. `7.2.c`). `{family}` is the leading
M-family token (e.g. `M7` for any `M7.x.y.z`). `{target_branch}` is
the operator-chosen ROADMAP-phase integration branch (e.g. `phase3`);
it replaces every reference to `main` in the underlying
`ship-roadmap-item` skill.

## Template

```
You are a single-shot autopilot worker. Repository:
/Users/user/PhpstormProjects/wathkeepers (integration branch: {target_branch}).

Your job: ship ROADMAP item M{id} end-to-end by applying the project
skill .claude/skills/ship-roadmap-item/SKILL.md exactly, with one global
override: wherever ship-roadmap-item SKILL.md says `main`, you must use
`{target_branch}` instead. You will return a single JSON object as your
final tool result. No prose outside JSON.

BRANCH-TARGET OVERRIDE (applies to every phase of ship-roadmap-item)
- Phase 3 (branch creation): `git checkout -b rdd/<id>-<slug>` is cut
  from `{target_branch}`. Confirm HEAD is `{target_branch}` and fast-
  forwarded before branching.
- Phase 6 (roadmap mark): the `[ ]` you flip is in
  `docs/ROADMAP-{target_branch}.md` only — never touch any other
  `docs/ROADMAP-phase*.md` in this tick.
- Phase 8 (PR + merge): `gh pr create --base {target_branch} ...` and
  `gh pr merge <pr> --squash --delete-branch`. Squash-merge into
  `{target_branch}`, never into `main`.
- Phase 9 (local cleanup): `git checkout {target_branch}` then
  `git pull --ff-only origin {target_branch}`.
- Phase 10 (lesson-mark follow-up): commit on `{target_branch}` and
  `git push origin {target_branch}`. The doc-only follow-up must not
  land on `main`.

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
  the final "docs(lessons): mark M{id} shipped" follow-up commit — on
  `{target_branch}` per the override above, not on `main`.
- Use TodoWrite/TaskCreate to track phases internally.
- Run go test -race in run_in_background and wait via notification, do
  not poll.
- Parallel review iter-1 (`codex review` CLI + critic agent) is
  MANDATORY and runs as two background tool calls in one message. The
  legacy `omc ask` wrapper is retired — invoke `codex` directly per
  the updated ship-roadmap-item Phase 7.
- Delete /tmp/codex-review-M{id}.md after extracting findings.

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

## Auto-decompose writer-Agent prompt

Used when the orchestrator (Step 5) or the ship-Agent (Step 7) flags an
M-id as `aggregate-needs-decomposition`. The orchestrator substitutes
`{id}` (M-id without the leading `M`), `{family}` (leading M-family
token), `{target_branch}` (active integration branch), and
`{halt_detail}` (the free-text reason from the picker or the ship-Agent
that triggered the decompose).

The writer-Agent is the ONLY actor allowed to edit ROADMAP files in this
flow. It must NOT touch `core/`, `harness/`, `keep/`, `tools-builtin/`,
`bin/`, `scripts/`, or any file outside `docs/ROADMAP-{target_branch}.md`.

```
You are a single-shot decompose-writer for a roadmap doc-only commit.
Repository on /Users/user/PhpstormProjects/wathkeepers, currently on
branch {target_branch}. All edits and the resulting push target
{target_branch} — never `main`.

The ship-Agent (or the picker) flagged M{id} as
aggregate-needs-decomposition. Detail:
{halt_detail}

Your job: read docs/ROADMAP-{target_branch}.md, locate the `[ ]` line
for M{id}, and replace it with the parent line + 2–5 nested leaf
sub-items M{id}.a, M{id}.b, M{id}.c, … using alphabetic suffixes
(matches Phase-1 convention: M2.7.a/b/c/d/e, M2.8.d.a/b — see
docs/ROADMAP-phase1.md for prior art).

Each leaf must:
- Project to <1000 LOC and <20 files (ship-Agent's aggregate budget).
- Be independently shippable as a single squash-merged PR. If a strict
  ordering dependency exists, encode it by letter (M{id}.b depends on
  M{id}.a means b's bullet text says "depends on M{id}.a"); the autopilot
  ships leaves top-down in file order.
- Embed acceptance criteria inline in the bullet text (no nested AC
  sub-bullets needed; Phase-1 convention treats the bullet text itself
  as the AC for the leaf).
- Be concrete: name the package directories, table names, function
  signatures, or interfaces that will be touched. The ship-Agent
  reading this leaf must be able to start without further clarification.

Context to read BEFORE proposing the decomposition:
- docs/ROADMAP-{target_branch}.md — the full milestone section around
  M{id} (sibling sub-items, Verification block, Dependencies, etc.).
- docs/lessons/{family}.md if it exists — prior-art grain for this
  family; the decomposed leaves should match that grain (≈600–900 LOC,
  ≈6 files per leaf is a healthy target).
- docs/ROADMAP-phase1.md around M2.7 / M2.8 / M3.5.a — examples of how
  Phase-1 decomposed aggregates into letter-suffixed leaves.
- The relevant package directories named in M{id} (look at what already
  exists vs. what's net-new), so the proposed scope is anchored in
  reality.

Then apply the edits via the Edit tool. Use exact-match replacement of
the existing M{id} line; insert the new leaves indented two spaces under
the parent line so the picker treats M{id} as a parent and the new
M{id}.a/b/c… as children.

Commit message (use a HEREDOC):

docs(roadmap): decompose M{id} into <count> sub-leaves

Ship-Agent flagged M{id} as aggregate. Decomposed into <comma-separated
leaf-ids> per autopilot leaf-grain budget (<1000 LOC / <20 files per
leaf).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>

Push to origin/{target_branch} with `git push origin {target_branch}`.

NEVER touch files outside docs/ROADMAP-{target_branch}.md. NEVER push
to main. NEVER use --force, --no-verify, or --no-gpg-sign.

Return value: a single JSON object only.

{
  "status": "ok" | "failed",
  "commit_sha": "<sha or null>",
  "parts": <count of sub-leaves added, or 0 on failure>,
  "leaf_ids": ["M{id}.a", "M{id}.b", ...] | [],
  "detail": null | "<failure reason, short>"
}
```

## Cascade-pass writer-Agent prompt

Used in the optional cascade step (see `SKILL.md` step 4) when one or
more `[ ]` parents have all-`[x]` children. The orchestrator pre-computes
the diff list and embeds it, along with `{target_branch}` (the active
ROADMAP-phase integration branch, e.g. `phase3`).

```
You are a single-shot writer for a roadmap doc-only commit. Repository
on /Users/user/PhpstormProjects/wathkeepers, currently on branch
{target_branch}. All edits and the resulting push target
{target_branch} — never `main`.

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

Push to origin/{target_branch} with `git push origin {target_branch}`.

Return value: a single JSON object only.

{
  "status": "ok" | "failed",
  "commit_sha": "<sha or null>",
  "detail": null | "<failure reason>"
}
```
