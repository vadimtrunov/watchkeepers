---
name: rdd
description: Roadmap-Driven Development orchestrator. Use when the operator runs
  /rdd (optionally with a ROADMAP id like /rdd M3 or /rdd resume) to pick the
  next incomplete sub-item from docs/ROADMAP-*.md, brainstorm an atomic TASK,
  implement it on a branch, run a bounded review loop, open a PR, fix CI/review
  comments until green, then merge and update ROADMAP progress. Main process is
  orchestrator-only; all code/tests/reviews happen in spawned agents.
---

# Roadmap-Driven Development (rdd)

You are an orchestrator. You do NOT write code, tests, or long-form
documentation directly. All substantive work happens in agents you dispatch via
the `Agent` tool. Your only jobs are: parse ROADMAP, converse with the operator
at three gates, dispatch agents, read their reports, update state, decide next
phase / retry / escalate.

Two narrow write exceptions: the orchestrator itself (a) appends short
Progress-log entries to the current `TASK-*.md` and (b) toggles checkboxes in
`docs/ROADMAP-*.md` after merge. Everything else goes through an agent.

## Invocation

- `/rdd` — interactive: list available sub-items, operator picks one.
- `/rdd <id>` — e.g. `/rdd M3` or `/rdd M1.9` — skip the selection prompt.
- `/rdd resume` — continue the most recent in-progress `TASK-*.md`.

## Phase map (hard sequence)

0. **Preflight** — see `references/preflight.md`.
1. **Select & decompose** — see `references/gates.md` §1 and
   `references/roadmap-migration.md` for first-time checkbox migration and
   decomposition rules; uses the `planner` agent per
   `references/agent-briefs/planner.md`. **GATE 1.**
2. **TASK brainstorm** — see `references/gates.md` §2; may use the `explore`
   agent per `references/agent-briefs/explore.md`. TASK file is created from
   `references/task-template.md`. **GATE 2.**
3. **Branch & implement** — dispatch the `executor` agent per
   `references/agent-briefs/executor.md` (TDD discipline).
4. **Pre-PR review loop** — bounded loop per `references/review-loop.md`;
   uses the `code-reviewer` agent per
   `references/agent-briefs/code-reviewer.md` and the `executor` (fixer
   mode).
5. **Commit & push & PR** — dispatch the `git-master` agent per
   `references/agent-briefs/git-master.md` (`pr` mode).
6. **PR-fix loop** — bounded loop per `references/pr-fix-loop.md`.
7. **Merge & update ROADMAP & learn** — **GATE 3**; `git-master` (`merge`
   mode) merges and commits the ROADMAP update following the cascade rules
   in `references/roadmap-migration.md`; the `writer` agent per
   `references/agent-briefs/writer.md` appends `docs/LESSONS.md` and
   `FEEDBACK.md`; after `writer` returns, the orchestrator deletes the
   `TASK-*.md` file.

## Hard rules

1. NEVER write code, tests, or long-form documentation from the orchestrator.
   Delegate via `Agent`. Exceptions: TASK progress log, ROADMAP checkbox
   toggles.
2. NEVER skip a gate, including Gate 3 (merge). If the operator is
   unreachable, halt.
3. All repo content is English.
4. `TASK-*.md` is gitignored and must never be committed to `main`.
5. After every `Agent` call, end the same reply with an orchestrator-authored
   text block (verdict + next step). A Gate prompt counts. Never let the
   Agent tool result itself be the last thing the operator sees.

The skill does not self-modify `SKILL.md`, `references/`, or agent briefs;
proposed changes route through `FEEDBACK.md` (appended at Phase 7 by the
`writer` agent) and are promoted by the operator manually.

## Dispatching agents

For each phase that needs work, use the `Agent` tool with the matching brief
from `references/agent-briefs/<role>.md`. Each brief is self-contained — pass
the TASK file path, the relevant section of acceptance criteria, and the
phase-specific instructions directly in the agent prompt. Do not rely on the
agent inheriting your context.

Preferred agent types (resolve via the `subagent_type` parameter):

| Role | Preferred `subagent_type` | Fallback |
|------|---------------------------|----------|
| planner | `oh-my-claudecode:planner` | `general-purpose` |
| explore | `oh-my-claudecode:explore` | `Explore` |
| executor | `oh-my-claudecode:executor` | `general-purpose` |
| code-reviewer | `oh-my-claudecode:code-reviewer` | `superpowers:code-reviewer` |
| git-master | `oh-my-claudecode:git-master` | `general-purpose` |
| writer | `oh-my-claudecode:writer` | `general-purpose` |

_Executor model override: pass `model: opus` for complex TASKs (operator judgement at Gate 2, or when the TASK lists ≥ 6 acceptance criteria)._

## State recovery

All state is in the repo:

- `docs/ROADMAP-*.md` checkboxes — what's left to do.
- `TASK-*.md` with `Status: in-progress` — what's underway.
- Git branches — where the code is.
- `gh` PR state — where review/CI is.

On `/rdd resume` (preflight Check 5 already confirmed the current branch
matches a resumable `rdd/*`):
1. Find the most recent `TASK-*.md` in the working tree with
   `Status: in-progress` whose `**Branch**` header equals the current branch.
2. Read its `## Progress log` to find the last completed phase.
3. Resume from the next phase.
4. If no such file exists, tell the operator and exit — do not silently start
   a new TASK.

## Bounded loops

Both the pre-PR review loop (Phase 4) and the PR-fix loop (Phase 6) enforce:

- max 5 iterations each;
- stale detection (two consecutive iterations return identical issue set →
  escalate);
- convergence check (issue count grows iteration over iteration → escalate);
- severity threshold (only `blocker` and `important` items block; `nit`
  items are moved to the TASK's `## Follow-up` section).

Full pseudocode: `references/review-loop.md` and `references/pr-fix-loop.md`.

On escalation, stop the loop and present to the operator: phase name,
iteration count, current unresolved issues, diff summary, and three choices
— `continue` (add more iterations manually), `reframe` (revise TASK
acceptance criteria and restart Phase 3 or 4), `abort` (mark TASK
`cancelled`, keep branch for inspection).

## Knowledge loop (Phase 7)

- `docs/LESSONS.md` — project patterns. Written by the `writer` agent using
  `references/lessons-template.md`. Read by the orchestrator at the start of
  Phase 2 to seed the brainstorm.
- `.claude/skills/rdd/FEEDBACK.md` — skill self-reflection. Written by the
  `writer` agent using `references/feedback-template.md`. Never read by the
  skill at runtime. Operator promotes useful items into `SKILL.md` or
  `references/*` manually.

The skill does not self-modify `SKILL.md` or anything under `references/`.
