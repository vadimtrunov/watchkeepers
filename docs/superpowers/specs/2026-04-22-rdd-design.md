# Roadmap-Driven Development Skill (`rdd`) — Design

**Status**: Approved after brainstorming; ready for implementation planning
**Created**: 2026-04-22
**Author**: Vadym Trunov (operator) + Claude (brainstorming partner)
**Location of the skill**: `.claude/skills/rdd/` (project-level, English)

---

## 1. Context and Goal

The wathkeepers repository currently contains documentation only: a Phase 1
ROADMAP (`docs/ROADMAP-phase1.md`) with ten milestones M1–M10, a backlog
(`docs/BACKLOG.md`), and source docs. The operator wants a repeatable,
semi-autonomous development workflow driven by that ROADMAP so that each
iteration ships a small, well-tested, reviewed unit of work without manual
orchestration of the intermediate steps.

The `rdd` skill (Roadmap-Driven Development) is the orchestrator. One invocation
turns one incomplete ROADMAP sub-item into one merged pull request, accompanied
by explicit acceptance criteria, a test plan, a bounded review loop, a bounded
PR-fix loop, and updates to two knowledge files that improve the next iteration.

The operator remains in charge of three decisions — which item to work on, what
approach and acceptance criteria to commit to, and whether to merge the final PR
— and delegates everything else.

---

## 2. Non-Goals

- The skill does not auto-merge. Merging is always an explicit operator action
  at Gate 3.
- The skill does not modify its own `SKILL.md`. Self-reflection lives in a
  separate `FEEDBACK.md` that the operator manually promotes into the skill.
- The skill does not run multiple TASKs in parallel by default. Git worktrees
  are supported only when the operator explicitly opts in.
- The skill does not produce telemetry, dashboards, or metrics beyond what the
  repository already has.
- The skill is not generic for arbitrary repositories. It targets this repo's
  ROADMAP structure (`docs/ROADMAP-*.md`) and its git/gh setup.

---

## 3. Requirements

### 3.1 Functional

1. **Task selection.** Given `/rdd`, list available ROADMAP sub-items (those
   whose dependencies are closed) with their milestone context. The operator
   picks one. Given `/rdd <id>` (e.g. `M3`, `M1.9`), go directly to that
   sub-item. Given `/rdd resume`, continue the most recent in-progress
   `TASK-*.md`.
2. **Decomposition at selection.** If the selected sub-item is estimated at
   more than ~1 day of work or spans more than one PR boundary, propose a
   deeper breakdown and, on operator approval (Gate 1), write the new nested
   checkboxes back into the ROADMAP before continuing.
3. **Atomic TASK.** One invocation produces exactly one `TASK-<id>-<slug>.md`
   file, mapped 1:1 to one ROADMAP sub-item and (eventually) one PR.
4. **Brainstorm with acceptance criteria.** Before any code is written, the
   TASK file must contain: scope statement, explicit acceptance criteria, an
   explicit test plan covering happy/edge/negative/security cases, and an
   ordered implementation plan. Operator approves (Gate 2) before
   implementation starts.
5. **Branch from main.** The implementation branch is always created from the
   current tip of `main` with the name `rdd/<task-slug>`.
6. **Orchestrator-only main process.** The skill's main process does not write
   code, tests, or long-form documentation. All substantive writes happen in
   agents dispatched via the `Agent` tool. Two narrow exceptions: the
   orchestrator itself appends short Progress-log entries to the current
   `TASK-*.md` (bookkeeping) and toggles ROADMAP checkboxes after merge
   (§6.8). The orchestrator parses ROADMAP, converses with the operator at
   the three gates, dispatches agents, reads their reports, updates state,
   and decides next phase / retry / escalation.
7. **Pre-PR bounded review loop.** Dispatch the `code-reviewer` agent with a
   severity contract; fix blocker/important findings through a dispatched
   `executor` agent; repeat up to five iterations with stale and convergence
   checks. Nits are deferred to a Follow-up section of the TASK file.
8. **PR creation.** On convergence of the review loop, dispatch `git-master`
   to finalize the commit, push, and open a PR via `gh pr create` with a body
   derived from the TASK file (summary, test plan, acceptance checklist).
9. **PR-fix bounded loop.** Poll `gh pr checks` and `gh pr view --comments`
   until no failing checks remain and no unresolved blocker comments remain.
   Dispatch `executor` to address failures. Same bounded rules as the pre-PR
   loop (max 5 iterations, stale, convergence, 30-minute per-run timeout).
10. **Gate-3 merge.** When the loop converges, prompt the operator explicitly:
    "Merge now?". Only on explicit confirmation does `git-master` merge.
11. **ROADMAP update on merge.** After merge, mark the completed leaf
    sub-item `[x]` in `docs/ROADMAP-*.md`, then cascade upward: at each
    ancestor level (e.g. `M#.k` → `M#`), mark it `[x]` only if every one of
    its direct children is `[x]`. This generalizes to arbitrary decomposition
    depth introduced at Gate 1. Commit and push the update.
12. **Two-tier knowledge write.** After merge, dispatch `writer` to append to
    `docs/LESSONS.md` (project-level patterns) and to
    `.claude/skills/rdd/FEEDBACK.md` (meta-reflection about the skill itself).

### 3.2 Non-functional

- **Language**: all files written to the repo are in English (project rule).
- **Safety**: preflight must refuse to run if CI is missing, the active `gh`
  account is not the repository owner, or the working tree is dirty.
- **Recovery**: all state lives in the repo (ROADMAP checkboxes, `TASK-*.md`,
  git branches, gh state). `/rdd resume` must work after any interruption by
  reading the repo alone.
- **Isolation**: every dispatched agent receives a self-contained prompt
  (TASK path + acceptance criteria + phase-specific instructions). The agent
  does not inherit orchestrator context beyond what is explicitly passed.
- **Hard rules**: see §9.

---

## 4. Architecture

### 4.1 Orchestrator-only pattern

The main process is a dispatcher and a state reader. It never writes code,
tests, or documentation directly. All substantive work is performed by agents
launched via the `Agent` tool. This gives every piece of work a clean context,
keeps the orchestrator's own context light, and makes each phase independently
reviewable.

The orchestrator's permitted actions:

- Read files (ROADMAP, TASK, LESSONS, FEEDBACK, source files for preflight).
- Run read-only shell queries (`git status`, `gh pr view`, `gh pr checks`).
- Dispatch agents via `Agent`.
- Update `TASK-*.md` progress logs (lightweight state bookkeeping).
- Update ROADMAP checkboxes after merge.
- Prompt the operator at Gates 1, 2, 3, or on escalation.

The orchestrator's prohibited actions:

- Writing application code, tests, or long-form documentation content.
- Performing `git commit`, `git push`, `gh pr create`, or `gh pr merge`
  directly (these go through the `git-master` agent so the commit convention
  and review trail are uniform).
- Modifying `SKILL.md`, `references/*`, or any agent brief.

### 4.2 Roles and agents

| Role | Agent (preferred) | Phase(s) | Purpose |
|------|-------------------|----------|---------|
| Planner | `planner` | 1 | Decide whether the selected sub-item fits in one PR; if not, propose a decomposition. |
| Explorer | `explore` (read-only) | 2 | Gather file-system and code context for brainstorm; never writes. |
| Executor | `executor` (opus for complex TASKs) | 3, 4 (fixer), 6 (fixer) | TDD implementation and targeted fixes. |
| Reviewer | `code-reviewer` | 4 | Severity-rated review against acceptance criteria. |
| Git master | `git-master` | 5, 7 | Commit, push, `gh pr create`, merge, ROADMAP update commit. |
| Writer | `writer` | 7 | Append to LESSONS and FEEDBACK. |

The orchestrator uses the `oh-my-claudecode` variants of these roles when
available; generic fallbacks (`Agent({subagent_type: "general-purpose", ...})`)
are acceptable if a specialized agent is unavailable.

### 4.3 High-level control flow

```
/rdd [id|resume]
    │
    ├── Phase 0: Preflight (orchestrator)
    │       ├── CI configured?            (else stop)
    │       ├── gh active user = owner?   (else stop)
    │       └── working tree clean?       (else stop)
    │
    ├── Phase 1: Select & decompose
    │       ├── parse ROADMAP, build candidate list
    │       ├── operator picks item (or id given as argument)
    │       ├── dispatch planner; if too large → propose decomposition
    │       └── GATE 1: "correct item and breakdown?"
    │
    ├── Phase 2: TASK brainstorm
    │       ├── create TASK-<id>-<slug>.md from template
    │       ├── brainstorm with operator: scope, acceptance criteria, test plan, steps
    │       ├── (optional) dispatch explore for repo context
    │       └── GATE 2: "approach and acceptance criteria OK?"
    │
    ├── Phase 3: Branch & implement
    │       ├── git checkout -b rdd/<slug> from main
    │       └── dispatch executor (TDD, acceptance criteria, plan)
    │
    ├── Phase 4: Pre-PR review loop (bounded)
    │       └── for i in 0..4:
    │               ├── dispatch code-reviewer → severity-rated output
    │               ├── break if no blocker and no important
    │               ├── stale/convergence checks → escalate if triggered
    │               └── dispatch executor fixer
    │
    ├── Phase 5: Commit & push & PR
    │       └── dispatch git-master: final commit, push, gh pr create
    │
    ├── Phase 6: PR-fix loop (bounded)
    │       └── for i in 0..4:
    │               ├── poll gh pr checks + comments
    │               ├── break if all green and no blocker comments
    │               ├── stale/convergence checks, 30-min per-run timeout
    │               └── dispatch executor fixer; reply "fixed in <sha>"
    │
    └── Phase 7: Merge & update ROADMAP & learn
            ├── GATE 3: "merge now?"
            ├── dispatch git-master: merge
            ├── update ROADMAP checkboxes, commit, push
            └── dispatch writer: append LESSONS.md, FEEDBACK.md
```

---

## 5. Data Model

### 5.1 ROADMAP — hierarchical checkboxes

The skill expects (and on first contact migrates) `docs/ROADMAP-*.md` to carry
two levels of checkboxes:

```markdown
### M1 — Foundation [ ]
**Goal**: ...
**Scope**
- [ ] **M1.1** Layout — monorepo structure
- [ ] **M1.2** Makefile as sole entry point
- [ ] **M1.3** Go quality stack
- [ ] **M1.4** TypeScript quality stack
- [ ] **M1.5** Cross-cutting linters
- [ ] **M1.6** Pre-commit (lefthook) + gitleaks
- [ ] **M1.7** Commit quality (commitlint)
- [ ] **M1.8** Dependency hygiene (Renovate + license checker)
- [ ] **M1.9** CI pipeline
- [ ] **M1.10** Developer bootstrap doc
**Dependencies**: none
```

Rules:

- `M#.k` is an atomic unit of work (one PR, one TASK).
- `M#` is a milestone checkbox; it is marked `[x]` automatically when all its
  `M#.k` sub-items are `[x]`.
- If a sub-item `M#.k` is decomposed further (Gate 1), it becomes
  `M#.k.a`, `M#.k.b`, … and these become the atomic units.
- The skill always reads all files matching `docs/ROADMAP-*.md`. Phase 1 of
  wathkeepers has a single ROADMAP file; future phases may have more.

### 5.2 TASK file — ephemeral work unit

`TASK-<roadmap-id>-<slug>.md` (e.g. `TASK-M1.1-monorepo-layout.md`). Created at
the start of Phase 2, lives on the working branch only, **not committed to
main**. `TASK-*.md` is added to `.gitignore` on first skill run.

Template:

```markdown
# TASK <roadmap-id> — <title>

**ROADMAP**: docs/ROADMAP-<phase>.md §M# → <roadmap-id>
**Created**: YYYY-MM-DD
**Status**: in-progress | blocked | merged | cancelled
**Branch**: rdd/<slug>
**PR**: <URL after Phase 5, empty before>

## Scope
<one paragraph: what is included and what is explicitly NOT included>

## Acceptance criteria (approved at Gate 2)
- [ ] AC1: ...
- [ ] AC2: ...

## Test plan (approved at Gate 2)
- [ ] Happy: ...
- [ ] Edge: ...
- [ ] Negative: ...
- [ ] Security / isolation: ...

## Plan (implementation steps)
- [ ] Step 1 — ...
- [ ] Step 2 — ...

## Progress log
<append-only, one entry per phase>

## Follow-up (nits deferred from review)
<empty at start; review loop appends here>
```

### 5.3 State

There is no external state store. Every resumable fact lives in the repo:

- ROADMAP checkboxes → "what's left".
- `TASK-*.md` with `Status: in-progress` → "what's underway".
- Git branches → "where the code is".
- `gh` PR state → "where the review is".

`/rdd resume` is implemented by finding the most recent `TASK-*.md` with
`Status: in-progress` in the working tree, reading its progress log, and
continuing from the recorded phase.

### 5.4 Knowledge files

- `docs/LESSONS.md` — committed to main. A `writer` agent appends patterns and
  decisions learned during implementation. Read by the orchestrator at the
  start of Phase 2 to seed the brainstorm with prior context.
- `.claude/skills/rdd/FEEDBACK.md` — committed to main. A `writer` agent
  appends meta-reflection about the skill itself (where the loop wasted
  iterations, where the brief was unclear, where the operator had to step in).
  Read by the operator; the operator promotes useful items into `SKILL.md` or
  `references/*` manually.

### 5.5 Files the skill creates or modifies

| Path | Purpose | Written by | Committed |
|------|---------|------------|-----------|
| `docs/ROADMAP-*.md` | checkbox updates, decomposition | orchestrator (Gate 1 approval; Phase 7 post-merge) | yes |
| `TASK-*.md` | ephemeral work unit | orchestrator + agents | no (gitignored) |
| `docs/LESSONS.md` | project patterns | `writer` agent (Phase 7) | yes |
| `.claude/skills/rdd/FEEDBACK.md` | skill self-reflection | `writer` agent (Phase 7) | yes |
| Source code, tests, configs | TASK implementation | `executor` agent (Phase 3) | yes |
| `.gitignore` | add `TASK-*.md` | orchestrator, once | yes |

---

## 6. Phases in Detail

### 6.1 Phase 0 — Preflight

Orchestrator checks, in this order:

1. `docs/ROADMAP-*.md` exists and matches at least one file.
2. `.github/workflows/` exists and contains at least one workflow file
   (CI is a hard prerequisite).
3. `gh auth status` parses; the currently active `gh` account matches the
   `origin` owner. Mismatch → stop with instructions: `gh auth switch --user <owner>`.
4. `git status --porcelain` is empty (clean working tree). Dirty → stop.
5. Current branch is `main` or a `rdd/*` branch belonging to a resumable TASK.

Any failure → stop with a precise message. The skill does not attempt to fix
preflight failures itself.

### 6.2 Phase 1 — Select and decompose

1. Parse `docs/ROADMAP-*.md`. Identify all `[ ]` sub-items and their milestone.
   For each, check `Dependencies` — only surface sub-items whose dependency
   milestones are `[x]`.
2. If invoked without arguments, present the operator with a numbered list
   grouped by milestone; operator picks.
3. If invoked with `<id>`, validate the id exists and is unmet; stop if not.
4. Dispatch `planner` with the ROADMAP section and scope context. Its brief
   asks: "does this fit in one PR (≤ ~1 day, ≤ ~15 files, single concern)?"
   If not, it returns a proposed decomposition.
5. If decomposition is proposed: present to operator, apply to ROADMAP only on
   approval, then take the first sub-item of the decomposition as the unit of
   work.
6. **Gate 1**: present the selected unit of work and any ROADMAP edits that
   will be applied. Operator must say "ok" or reject.

### 6.3 Phase 2 — TASK brainstorm

1. Create `TASK-<id>-<slug>.md` from template.
2. Read `docs/LESSONS.md` to seed relevant prior patterns.
3. Optionally dispatch `explore` (read-only) to gather codebase context the
   brainstorm needs.
4. Converse with the operator on: scope boundary, acceptance criteria, test
   plan (happy / edge / negative / security), implementation steps. Fill the
   TASK file section by section.
5. **Gate 2**: show the complete acceptance criteria and test plan. Operator
   must say "ok" or request revisions.

### 6.4 Phase 3 — Branch and implement

1. `git checkout main && git pull --ff-only`.
2. `git checkout -b rdd/<slug>`.
3. Dispatch `executor` with:
   - TASK file path.
   - Explicit instruction: "implement per acceptance criteria using TDD:
     write tests first per the test plan, then code, commit per sub-step."
   - Reference to project conventions (`docs/LESSONS.md`, any `.claude/`
     project instructions).
4. Executor reports back with list of commits and files changed.
5. Orchestrator appends a Progress log entry to TASK.

If the executor cannot pass its own tests after three internal attempts,
it reports blocked; orchestrator escalates to operator.

### 6.5 Phase 4 — Pre-PR review loop

See §7 for the full pseudocode. Summary:

- Up to 5 iterations of `code-reviewer` → (blocker/important found) → `executor` fixer.
- Stale detection: two consecutive identical comment sets → escalate.
- Convergence check: comment count growing across iterations → escalate.
- Nits are moved to the TASK's `## Follow-up` section, not fixed here.
- On convergence, proceed to Phase 5.

### 6.6 Phase 5 — Commit, push, PR

Dispatch `git-master` with:

- Instruction to consolidate or preserve commits per the repo's conventions
  (follow existing style; do not rewrite without need).
- PR body derived from TASK file: summary (from Scope), acceptance checklist,
  test plan.
- PR base = `main`, head = `rdd/<slug>`.
- Draft status = off (ready for review).

`git-master` returns the PR URL, which the orchestrator writes into the TASK.

### 6.7 Phase 6 — PR-fix loop

See §7. Summary:

- Poll `gh pr checks` and `gh pr view --comments` until all checks pass and
  no unresolved blocker/important comments remain.
- Up to 5 fix iterations.
- 30-minute per-run timeout: if any check has not reported in 30 minutes
  since push, escalate (CI may be stuck).
- `executor` fixes are pushed to the same branch; each fix is followed by a
  `gh pr comment --body "fixed in <sha>"` reply on each comment it addressed.
- Actual "Resolve" clicks on comments are left to the operator or the
  original reviewer; the skill never auto-resolves.

### 6.8 Phase 7 — Merge, update ROADMAP, learn

1. **Gate 3**: "All checks green, no unresolved blockers. Merge now?"
2. On "yes", dispatch `git-master` to merge (method per repo convention; if
   unspecified, default to squash).
3. Orchestrator updates `docs/ROADMAP-*.md`:
   - set the completed sub-item to `[x]`;
   - if all sub-items of its milestone are `[x]`, set the milestone to `[x]`.
4. Dispatch `git-master` to commit the ROADMAP update with message
   `chore(roadmap): mark <id> complete` and push to `main`.
5. Dispatch `writer` with the TASK file + diff summary:
   - append a `## <YYYY-MM-DD> — <id>` section to `docs/LESSONS.md` per
     `references/lessons-template.md`;
   - append a `## <YYYY-MM-DD> — <id>` section to
     `.claude/skills/rdd/FEEDBACK.md` per `references/feedback-template.md`.
6. Orchestrator deletes the `TASK-*.md` file (it is gitignored, never
   appears on `main`, and its lifecycle ends at merge); deletes the local
   branch and the remote branch on origin. The merged TASK's information
   survives in: the ROADMAP checkbox state, the PR's permanent record, and
   the newly appended entries in `docs/LESSONS.md` and `FEEDBACK.md`.

---

## 7. Bounded Loops

### 7.1 Severity contract

The `code-reviewer` agent brief (`references/agent-briefs/code-reviewer.md`)
requires the agent to return JSON with three buckets:

- **blocker** — violates acceptance criteria / breaks tests / security issue /
  capability leakage.
- **important** — real logic defect / violates architecture decisions in
  ROADMAP §2 / missing test case from the approved test plan.
- **nit** — style, naming, minor readability, comment typos.

Each item carries `{file, line, rationale, suggested_fix}`.

### 7.2 Pre-PR review loop (Phase 4)

```
previous = []
for i in 0..4:
    review = dispatch(code-reviewer, brief_with(TASK, acceptance, diff, previous))
    current = review.blocker + review.important

    if current == []:
        move review.nit -> TASK.Follow-up
        return CONVERGED

    if current == previous:
        escalate("review stuck: identical comments two rounds in a row")

    if i > 0 and len(current) > len(previous):
        escalate("review diverging: comment count growing")

    dispatch(executor, fixer_brief_with(TASK, comments=current))
    previous = current

escalate("review exhausted 5 iterations without converging")
```

### 7.3 PR-fix loop (Phase 6)

```
previous = fingerprint([], [])
deadline_per_check = 30 minutes
for i in 0..4:
    wait_until_all_checks_reported_or_timeout(deadline_per_check)
    checks = gh pr checks --json ...
    comments = gh pr view --comments --json ...

    failing_checks = [c for c in checks if c.status in {fail, cancelled}]
    unresolved_blockers = [c for c in comments if severity(c) in {blocker, important} and not resolved(c)]

    if failing_checks == [] and unresolved_blockers == []:
        return CONVERGED

    current = fingerprint(failing_checks, unresolved_blockers)
    if current == previous:
        escalate("pr-fix stuck: identical failures two rounds in a row")
    if i > 0 and fingerprint_count(current) > fingerprint_count(previous):
        escalate("pr-fix diverging")

    dispatch(executor, pr_fixer_brief_with(TASK, failing_checks, unresolved_blockers))
    executor also posts gh pr comment "fixed in <sha>" replies per addressed comment
    previous = current

escalate("pr-fix exhausted 5 iterations without converging")
```

### 7.4 Escalation format

When the orchestrator escalates, it prints to the operator:

- phase, loop name, iteration count,
- the current list of unresolved issues,
- a diff-summary of what has been tried,
- three options: `continue` (add more iterations manually), `reframe` (revise
  TASK acceptance criteria and restart Phase 3 or 4), `abort` (mark TASK
  `cancelled`, keep the branch for inspection).

---

## 8. Failure Modes

| Situation | Phase | Response |
|-----------|-------|----------|
| `.github/workflows/` missing | 0 | Stop: "CI not configured; set up CI before running rdd." |
| `gh` active user ≠ `origin` owner | 0 | Stop: "switch active gh account via `gh auth switch --user <owner>`." |
| Working tree dirty | 0 | Stop: "commit or stash before running." |
| ROADMAP has no `[ ]` at milestone/sub-item level | 1 | Propose one-time migration, apply only on Gate 1 approval. |
| All candidate sub-items blocked by dependencies | 1 | Stop: "no available sub-items; close dependencies first." |
| `planner` flags sub-item as too large | 1 | Propose decomposition; apply on Gate 1 approval. |
| Branch `rdd/<slug>` already exists | 3 | Stop + offer `--force-recreate` or `/rdd resume`. |
| `executor` cannot pass tests after 3 internal attempts | 3 | Escalate with diff attached. |
| Pre-PR review loop stale / divergent / > 5 iterations | 4 | Escalate with continue/reframe/abort options. |
| PR-fix CI timeout (> 30 min per run) | 6 | Escalate: "CI hanging, check infra." |
| PR-fix loop > 5 iterations | 6 | Escalate. |
| Operator rejects at Gate 3 | 7 | Stop; TASK stays `in-progress`; branch preserved; PR left open. |
| ROADMAP update rebase conflict on `main` | 7 | Attempt rebase; on conflict, escalate — the merge already happened, only the bookkeeping is stuck. |

---

## 9. Hard Rules

1. The orchestrator never writes code, tests, or long-form documentation.
   Every write (except TASK progress log entries and ROADMAP checkbox toggles)
   goes through a dispatched agent.
2. The orchestrator never skips a gate. If the operator is unreachable, halt.
3. The skill never auto-merges. Merge requires explicit Gate 3 confirmation.
4. The skill never modifies its own `SKILL.md` or `references/*` or
   `agent-briefs/*`. `FEEDBACK.md` is the only self-referential file the skill
   writes to; promotion into `SKILL.md` is manual.
5. The skill never resolves review comments on behalf of the reviewer. It may
   post `fixed in <sha>` replies; the "Resolve" click stays with a human.
6. The skill never runs when the active `gh` user is not the `origin` owner.
   Stops instead with explicit switch instructions.
7. Every file the skill writes to the repo is in English (project rule).
8. `TASK-*.md` files are gitignored; they are never committed to main.

---

## 10. File Layout and `SKILL.md` Scaffold

### 10.1 Files under `.claude/skills/rdd/`

```
.claude/skills/rdd/
├── SKILL.md
├── FEEDBACK.md
└── references/
    ├── preflight.md
    ├── roadmap-migration.md
    ├── task-template.md
    ├── gates.md
    ├── review-loop.md
    ├── pr-fix-loop.md
    ├── lessons-template.md
    ├── feedback-template.md
    └── agent-briefs/
        ├── planner.md
        ├── explore.md
        ├── executor.md
        ├── code-reviewer.md
        ├── git-master.md
        └── writer.md
```

Rationale: `SKILL.md` is the lightweight entrypoint that fits into context.
All detailed procedures, pseudocode, templates, and agent briefs are in
`references/*` and loaded by the orchestrator on demand in the phase that
needs them.

### 10.2 `SKILL.md` scaffold

```markdown
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

You are an orchestrator. You do NOT write code, tests, or documentation
directly. All real work happens in agents you dispatch via the Agent tool.
Your only jobs are: parse ROADMAP, converse with the operator at three gates,
dispatch agents, read their reports, update state, decide next phase / retry
/ escalate.

## Invocation
- `/rdd` — interactive: list available sub-items, operator picks one.
- `/rdd <id>` — e.g. `/rdd M3` or `/rdd M1.9` — skip to that item.
- `/rdd resume` — continue the most recent in-progress TASK-*.md.

## Phase map (hard sequence, three operator gates)
0. Preflight → references/preflight.md
1. Select & decompose → GATE 1 → references/gates.md §1
2. TASK brainstorm → GATE 2 → references/gates.md §2
3. Branch & implement (executor agent, TDD)
4. Pre-PR review loop (code-reviewer ↔ executor, bounded) → references/review-loop.md
5. Commit & push & PR (git-master agent)
6. PR-fix loop (poll gh, bounded) → references/pr-fix-loop.md
7. Merge → update ROADMAP → learn → GATE 3 (writer agent)

## Hard rules
- NEVER write code, tests, or docs from the orchestrator process. Always dispatch.
- NEVER skip a gate. If operator is unreachable, halt.
- NEVER merge without explicit operator confirmation at Gate 3.
- NEVER modify SKILL.md, references/*, or agent-briefs/* (FEEDBACK.md is fine).
- NEVER resolve review comments on behalf of the reviewer.
- NEVER run with `gh` active user ≠ owner(origin). Stop with instructions.
- ALL repo content is English.

## Dispatching agents
For each phase that needs work, use the Agent tool with the matching brief
from references/agent-briefs/<role>.md. Briefs are self-contained — include
the TASK path, acceptance criteria, and phase-specific instructions in the
prompt. Do not rely on the agent inheriting orchestrator context.

## State recovery
State lives in the repo: ROADMAP checkboxes, TASK-*.md files, git branches,
gh PR state. No external state. On `/rdd resume`, find the most recent
TASK-*.md with Status: in-progress and resume from the phase recorded in its
progress log.

## Bounded loops
Review loop (Phase 4) and PR-fix loop (Phase 6) both enforce:
- max 5 iterations each;
- stale detection (identical output two rounds → escalate);
- convergence check (comment count growing → escalate);
- severity threshold (only blocker/important block; nits deferred to Follow-up).
Details in references/review-loop.md and references/pr-fix-loop.md.

## Knowledge loop (Phase 7)
- docs/LESSONS.md — project patterns (agent: writer, template: references/lessons-template.md).
- .claude/skills/rdd/FEEDBACK.md — skill self-reflection (agent: writer, template: references/feedback-template.md).
Operator periodically reviews FEEDBACK.md and manually promotes improvements
into SKILL.md or references/*. The skill does not self-modify.
```

---

## 11. Out of Scope / Future Work

- Telemetry and metrics on skill runs.
- Auto-merge after a configurable quiet period.
- Parallel TASK execution with automatic worktree management.
- Self-modification of `SKILL.md` from `FEEDBACK.md`.
- Generalization beyond the wathkeepers repo (e.g. a user-level or plugin
  version) — considered only after the project-level version has proven
  itself across several milestones.
- Integration with external planning systems (Linear, Jira) for selecting
  the next unit of work.
