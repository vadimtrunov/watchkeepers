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

One narrow write exception: the orchestrator itself appends short
Progress-log entries to the current `TASK-*.md`. Everything else goes
through an agent — including ROADMAP checkbox toggles, which moved to the
`writer` agent in Phase 7a so they ride inside the squash commit (see
Phase map step 7).

> **Turn-closure invariant** — every `Agent` tool result MUST be
> followed, in the SAME reply, by at least one user-facing sentence
> (agent verdict + next step). Silent turn-exit after an `Agent`
> return is the #1 failure mode operators report (see `FEEDBACK.md`
> 2026-04-22). Formal form: Hard rule 5. Structural reinforcement:
> `## Dispatching agents` §Companion-todo.

## Invocation

- `/rdd` — interactive: list available sub-items, operator picks one.
- `/rdd <id>` — e.g. `/rdd M3` or `/rdd M1.9` — skip the selection prompt.
- `/rdd resume` — continue the most recent in-progress `TASK-*.md`.
- `/rdd --auto [<id>|resume]` — autonomous mode for `/loop`-driven runs.
  Auto-decides Gates 1/2/3 deterministically per `references/gates.md`
  §"Auto mode". Halts (no side effects) on any condition the auto rules
  cannot resolve unambiguously: planner verdict missing, bounded-loop
  escalation, ambiguous candidate selection, CI-not-green at Gate 3, or
  any preflight failure. The operator-facing gate prompts are still
  emitted to the transcript for audit, immediately followed by the
  auto-decision and its justification.

## Phase map (hard sequence)

0. **Preflight** — see `references/preflight.md`.
1. **Select & decompose** — see `references/gates.md` §1 and
   `references/roadmap-migration.md` for first-time checkbox migration and
   decomposition rules; uses the `planner` agent per
   `references/agent-briefs/planner.md`. **GATE 1.**
2. **TASK brainstorm** — see `references/gates.md` §2; may use the `explore`
   agent per `references/agent-briefs/explore.md`. TASK file is created from
   `references/task-template.md`. **GATE 2.**
   - Phase 2 reads only the milestone-family lessons file relevant to the
     candidate TASK (e.g. `docs/lessons/M5.md`) plus
     `docs/lessons/cross-cutting.md`. Never reads the full
     `docs/LESSONS.md` index — that file is not lessons content, only a
     pointer table.
3. **Branch & implement** — dispatch the `executor` agent per
   `references/agent-briefs/executor.md` (TDD discipline).
4. **Pre-PR review loop** — bounded loop per `references/bounded-loop.md`
   §Phase 4; uses the `code-reviewer` agent per
   `references/agent-briefs/code-reviewer.md` and the `executor` (fixer
   mode).
5. **Commit & push & PR** — dispatch the `git-master` agent per
   `references/agent-briefs/git-master.md` (`pr` mode). Returns the PR URL.
6. **PR-fix loop** — bounded loop per `references/bounded-loop.md` §Phase 6.
   Stops when CI is green and review is clean.
7. **Learn → merge → cleanup** — **GATE 3** before this phase begins.
   - **7a. Writer pass** — dispatch the `writer` agent per
     `references/agent-briefs/writer.md`. The writer commits to
     `rdd/<slug>` (NOT `main`) one combined commit containing: lesson
     append into `docs/lessons/<milestone>.md`, FEEDBACK append, and
     ROADMAP checkbox toggle (leaf + cascade per
     `references/roadmap-migration.md`).
   - **7b. Merge** — dispatch `git-master` in `merge` mode. Squash-merges
     the PR (the writer's commit is folded in). No follow-up commit on
     `main`.
   - **7c. Cleanup** — orchestrator deletes the `TASK-*.md` file.

The Phase 7 reordering removes the prior pattern of two follow-up commits
on `main` after merge (`chore(roadmap)` + `docs: lessons`). Both now ride
inside the squash commit. Toggle-only PRs are forbidden — see
`references/roadmap-migration.md` §"Verification batches".

## Hard rules

1. NEVER write code, tests, or long-form documentation from the orchestrator.
   Delegate via `Agent`. Exceptions: TASK progress log only. ROADMAP
   checkbox toggles moved to the `writer` agent in Phase 7a (so they ride
   inside the squash commit, not as a follow-up commit on `main`).
2. NEVER skip a gate, including Gate 3 (merge). In interactive mode, if
   the operator is unreachable, halt. In `--auto` mode, gates are
   auto-decided per `references/gates.md` §"Auto mode" — halting still
   applies whenever the auto rules cannot resolve the gate
   deterministically.
3. All repo content is English.
4. `TASK-*.md` is gitignored and must never be committed to `main`.
5. After every `Agent` call, end the same reply with an orchestrator-authored
   text block (verdict + next step). A Gate prompt counts. Never let the
   Agent tool result itself be the last thing the operator sees.
   Minimal shape:
   > `planner` returned verdict: fits one PR. Presenting Gate 1.

   Silent-exit is the #1 failure mode recorded in `FEEDBACK.md`
   2026-04-22; `## Dispatching agents` §Companion-todo gives the
   workflow-level reinforcement.
6. **PR size cap.** A single rdd PR aims for **≤ 500 LOC added and ≤ 5
   files changed**. Exceeding both is a Gate 1 reject — `planner` must
   return a decomposition before the TASK proceeds. Mechanical scaffolds
   (generated migrations, vendored fixtures) count toward LOC; the cap
   is on review surface, not novelty. The cap was set after the night
   that produced PRs of 1700–2400 LOC and induced multi-iteration review
   loops.
7. **No toggle-only PRs.** Any TASK whose only change is moving ROADMAP
   checkboxes (e.g. "verification covered by existing tests") rides on
   the same PR as the next feature TASK in the same milestone, or batches
   into a single PR per milestone close-out. Three consecutive
   toggle-only PRs in one night (#38–#40) was the trigger for this rule.
8. **Append-only writes for lessons / FEEDBACK.** The `writer` agent does
   not Read the whole lessons or FEEDBACK file before appending — both
   files are 10–80 KB and reading them per iteration costs ≥10K tokens.
   See `references/lessons-template.md` §"Append mechanics".

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

### Companion-todo (Hard rule 5 reinforcement)

**Before every `Agent` dispatch**, create a companion todo via
`TaskCreate` so the required follow-up stays visible in the UI:

```
TaskCreate([{
  content: "After <agent> returns: state verdict (≤2 lines) + next step (phase / gate / clarifying question)",
  activeForm: "Drafting follow-up after <agent>"
}])
```

Then dispatch the `Agent`. After the agent result lands, mark the todo
`in_progress`, write the follow-up text in the same reply, and mark it
`completed`. A lingering incomplete follow-up todo is a visible signal
that Hard rule 5 is about to be violated.

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

Full pseudocode: `references/bounded-loop.md`.

On escalation, stop the loop and present to the operator: phase name,
iteration count, current unresolved issues, diff summary, and three choices
— `continue` (add more iterations manually), `reframe` (revise TASK
acceptance criteria and restart Phase 3 or 4), `abort` (mark TASK
`cancelled`, keep branch for inspection).

## Knowledge loop (Phase 7a)

- `docs/lessons/<milestone>.md` — project patterns, one file per milestone
  family. Written by the `writer` agent using
  `references/lessons-template.md`. Read by the orchestrator at the start
  of Phase 2 — but **only the file matching the candidate TASK's
  milestone family**, plus `docs/lessons/cross-cutting.md`. Never the full
  index, never every milestone file.
- `docs/LESSONS.md` — index pointing to the milestone files. Edited by the
  writer agent only when a brand-new milestone family appears.
- `.claude/skills/rdd/FEEDBACK.md` — skill self-reflection. Written by the
  `writer` agent using `references/feedback-template.md`. Never read by the
  skill at runtime. Operator promotes useful items into `SKILL.md` or
  `references/*` manually.

The skill does not self-modify `SKILL.md` or anything under `references/`.

## Context hygiene between iterations

In autonomous-loop mode (`/loop /rdd resume`), the previous iteration's
state is in the repo (TASK file, branch, ROADMAP checkboxes, lessons).
The skill needs **no in-memory continuation** between iterations — every
phase loads what it needs from disk via the State recovery protocol.

Recommendation to the operator running the loop:

- **Run each iteration in a fresh Claude session** (`/clear` in
  Claude Code, or a new conversation in API harnesses). The 5–7 MB
  per-night session size observed pre-2026-05-05 was the cost of
  carrying old phase context into iterations that no longer needed it.
- Hot-path files for the rdd skill — `docs/ROADMAP-phase1.md` (~25K
  tokens), the milestone lessons file (~5–25K tokens),
  `docs/lessons/cross-cutting.md` (~3K tokens) — should sit in the
  prompt cache (5-min TTL). Putting them into the orchestrator's first
  user-block when the iteration starts maximizes cache hits across the
  rapid Phase-1-through-Phase-7 sequence within a single iteration.
- The orchestrator never needs to Read `docs/LESSONS.md` (the index) at
  runtime; it computes the milestone-family file path from the TASK id
  directly.
