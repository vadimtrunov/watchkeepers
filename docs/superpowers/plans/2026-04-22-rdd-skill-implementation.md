# `rdd` Skill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Materialize the approved `rdd` skill by creating `.claude/skills/rdd/` with `SKILL.md`, `FEEDBACK.md` seed, and the full `references/` tree — per the design in `docs/superpowers/specs/2026-04-22-rdd-design.md` (commit `a06f546`).

**Architecture:** Project-level skill. Orchestrator-only main process; work is dispatched to agents via the `Agent` tool. `SKILL.md` is a light entrypoint; details (pseudocode, templates, agent briefs) live in `references/*` and are read on demand.

**Tech Stack:** Markdown only. No code is executed by this plan — we are authoring skill content. Each task creates exactly one file and commits it atomically.

**Commit discipline:** every task ends with a commit of the single file it created, using the message template shown in the task. This gives one commit per file, so review and revert are trivial.

**How to treat code blocks:** every `Step: Write the file` contains the **complete final content** of the file inside a fenced block. Copy the content verbatim into the file; do not paraphrase or "improve" it. If anything seems off, stop and raise a question — do not ad-lib.

---

## File Structure

```
.claude/skills/rdd/
├── SKILL.md                         (Task 2)
├── FEEDBACK.md                      (Task 1, seed)
└── references/
    ├── preflight.md                 (Task 3)
    ├── roadmap-migration.md         (Task 4)
    ├── task-template.md             (Task 5)
    ├── gates.md                     (Task 6)
    ├── review-loop.md               (Task 7)
    ├── pr-fix-loop.md               (Task 8)
    ├── lessons-template.md          (Task 9)
    ├── feedback-template.md         (Task 10)
    └── agent-briefs/
        ├── planner.md               (Task 11)
        ├── explore.md               (Task 12)
        ├── executor.md              (Task 13)
        ├── code-reviewer.md         (Task 14)
        ├── git-master.md            (Task 15)
        └── writer.md                (Task 16)
```

Root-level changes:
- `.gitignore` gains one line (Task 1): `TASK-*.md`.

---

## Task 1: Scaffold — directories, `.gitignore`, `FEEDBACK.md` seed

**Files:**
- Create: `.claude/skills/rdd/` (directory)
- Create: `.claude/skills/rdd/references/` (directory)
- Create: `.claude/skills/rdd/references/agent-briefs/` (directory)
- Create: `.claude/skills/rdd/FEEDBACK.md`
- Modify: `.gitignore` (append one line)

- [ ] **Step 1: Create directories**

Run:
```bash
mkdir -p .claude/skills/rdd/references/agent-briefs
```

- [ ] **Step 2: Verify directories exist**

Run:
```bash
ls -la .claude/skills/rdd/ && ls -la .claude/skills/rdd/references/
```
Expected: both listings show directories; `agent-briefs/` present in the second.

- [ ] **Step 3: Append `TASK-*.md` to `.gitignore`**

Check current content of `.gitignore`. If it does not already contain a `TASK-*.md` entry, append one. Also ensure there is a trailing newline.

After editing, `.gitignore` must contain this line on its own (anywhere in the file, but keep it grouped under a header like the one below if the file already has headers):

```
# rdd skill: TASK files are ephemeral, never committed to main
TASK-*.md
```

- [ ] **Step 4: Verify `.gitignore` entry**

Run:
```bash
grep -n 'TASK-\*\.md' .gitignore
```
Expected: one matching line printed.

- [ ] **Step 5: Write `FEEDBACK.md` seed**

Write `.claude/skills/rdd/FEEDBACK.md` with exactly this content:

````markdown
# rdd Skill — Feedback Log

This file collects the `rdd` skill's self-reflection about its own behavior,
appended automatically at Phase 7 of each successful run by the `writer` agent
using the template in `references/feedback-template.md`.

**How this file is used:**
- The skill itself never reads this file during a run — entries here do not
  influence behavior automatically.
- The operator periodically reviews the accumulated entries and manually
  promotes useful changes into `SKILL.md` or the appropriate `references/*`
  file. This manual promotion is the only supported way the skill evolves.
- The skill never modifies its own `SKILL.md`, `references/*`, or agent
  briefs. It only appends to this file.

**Initial state:** empty. The first entry will be appended by the first
successful `/rdd` run.

---
````

- [ ] **Step 6: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/FEEDBACK.md
```
Expected: between 14 and 20 lines.

- [ ] **Step 7: Commit**

```bash
git add .gitignore .claude/skills/rdd/FEEDBACK.md
git commit -m "feat(rdd): scaffold skill directory, gitignore, feedback seed"
```

---

## Task 2: `SKILL.md`

**Files:**
- Create: `.claude/skills/rdd/SKILL.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
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
1. **Select & decompose** — see `references/gates.md` §1; uses the
   `planner` agent per `references/agent-briefs/planner.md`. **GATE 1.**
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
   `references/agent-briefs/git-master.md` (Phase 5 mode).
6. **PR-fix loop** — bounded loop per `references/pr-fix-loop.md`.
7. **Merge & update ROADMAP & learn** — **GATE 3**; `git-master` (Phase 7
   mode) merges and commits the ROADMAP update; the `writer` agent per
   `references/agent-briefs/writer.md` appends `docs/LESSONS.md` and
   `FEEDBACK.md`.

## Hard rules

1. NEVER write code, tests, or long-form documentation from the orchestrator
   process. Always dispatch. Allowed exceptions: TASK progress log, ROADMAP
   checkbox toggles.
2. NEVER skip a gate. If the operator is unreachable, halt.
3. NEVER merge without explicit operator confirmation at Gate 3.
4. NEVER modify `SKILL.md`, `references/*`, or any file under `agent-briefs/`.
   `FEEDBACK.md` is the only self-referential file you may append to.
5. NEVER resolve review comments on behalf of the reviewer. The `executor`
   (fixer mode) may post a `fixed in <sha>` reply; the "Resolve" click stays
   with a human.
6. NEVER run when the active `gh` account is not the owner of `origin`. Stop
   with the exact instructions in `references/preflight.md`.
7. ALL files written to the repo are in English.
8. `TASK-*.md` is gitignored and must never be committed to `main`.
9. Every internal link inside this skill uses a path relative to the skill
   root (`.claude/skills/rdd/`) — i.e. starts with `references/`. When
   writing or editing any file under this skill, preserve that convention.

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
| executor | `oh-my-claudecode:executor` (`model: opus` for complex TASKs) | `general-purpose` |
| code-reviewer | `oh-my-claudecode:code-reviewer` | `superpowers:code-reviewer` |
| git-master | `oh-my-claudecode:git-master` | `general-purpose` |
| writer | `oh-my-claudecode:writer` | `general-purpose` |

## State recovery

All state is in the repo:

- `docs/ROADMAP-*.md` checkboxes — what's left to do.
- `TASK-*.md` with `Status: in-progress` — what's underway.
- Git branches — where the code is.
- `gh` PR state — where review/CI is.

On `/rdd resume`:
1. Find the most recent `TASK-*.md` in the working tree with
   `Status: in-progress`.
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
````

- [ ] **Step 2: Verify file presence and frontmatter**

Run:
```bash
head -10 .claude/skills/rdd/SKILL.md && wc -l .claude/skills/rdd/SKILL.md
```
Expected: first line is `---`, `name: rdd` on line 2, description starts on line 3; total between 100 and 160 lines.

- [ ] **Step 3: Verify every referenced file path is planned**

Run:
```bash
grep -oE 'references/[a-z/-]+\.md' .claude/skills/rdd/SKILL.md | sort -u
```
Expected output, exactly (one path per line, alphabetically sorted):
```
references/agent-briefs/code-reviewer.md
references/agent-briefs/executor.md
references/agent-briefs/explore.md
references/agent-briefs/git-master.md
references/agent-briefs/planner.md
references/agent-briefs/writer.md
references/feedback-template.md
references/gates.md
references/lessons-template.md
references/pr-fix-loop.md
references/preflight.md
references/review-loop.md
references/roadmap-migration.md
references/task-template.md
```

If the output differs, fix `SKILL.md` — every listed path must be present, no extras. If it matches, continue.

- [ ] **Step 4: Commit**

```bash
git add .claude/skills/rdd/SKILL.md
git commit -m "feat(rdd): add SKILL.md entrypoint"
```

---

## Task 3: `references/preflight.md`

**Files:**
- Create: `.claude/skills/rdd/references/preflight.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Phase 0 — Preflight

Run these checks in order. Any failure stops the skill immediately with the
prescribed message. The skill does not attempt to auto-fix preflight failures.

## Check 1 — ROADMAP file exists

Command:
```bash
ls docs/ROADMAP-*.md 2>/dev/null
```
Pass: at least one matching file printed.
Fail: empty output. Stop with:
> `rdd` preflight failed: no `docs/ROADMAP-*.md` file found. Create a roadmap before running the skill.

## Check 2 — CI is configured

Command:
```bash
ls .github/workflows/*.yml .github/workflows/*.yaml 2>/dev/null | head -1
```
Pass: at least one workflow file printed.
Fail: empty output. Stop with:
> `rdd` preflight failed: no CI workflow in `.github/workflows/`. CI is a hard prerequisite — set up CI before running `rdd`.

## Check 3 — `gh` active account matches `origin` owner

Commands:
```bash
owner=$(git remote get-url origin | sed -E 's#.*[:/]([^/]+)/[^/]+(\.git)?$#\1#')
active=$(gh api user --jq .login 2>/dev/null)
echo "owner=$owner active=$active"
```
Pass: `$owner == $active`.
Fail (or `active` empty): stop with:
> `rdd` preflight failed: active `gh` account (`<active>`) is not the owner of `origin` (`<owner>`). Switch with: `gh auth switch --user <owner>`

## Check 4 — Working tree is clean

Command:
```bash
git status --porcelain
```
Pass: empty output.
Fail: non-empty output. Stop with:
> `rdd` preflight failed: working tree is dirty. Commit or stash changes before running.

## Check 5 — Current branch is `main` or a resumable `rdd/*`

Commands:
```bash
branch=$(git rev-parse --abbrev-ref HEAD)
echo "branch=$branch"
```
Pass:
- `$branch == main` — proceed to Phase 1 (fresh run).
- `$branch` matches `rdd/*` AND a corresponding `TASK-*.md` with `Status: in-progress` exists in the working tree — proceed in resume mode.

Fail otherwise. Stop with:
> `rdd` preflight failed: current branch `<branch>` is neither `main` nor a resumable `rdd/*` branch. Checkout `main` (or run `/rdd resume` from the correct `rdd/*` branch).

## Stop format

When stopping on any failure, print exactly one message using the format
above. Do not attempt the next check. Do not run any other part of the skill.
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/preflight.md && grep -c '^## Check' .claude/skills/rdd/references/preflight.md
```
Expected: line count between 60 and 90; `## Check` count equals `5`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/preflight.md
git commit -m "feat(rdd): add preflight check reference"
```

---

## Task 4: `references/roadmap-migration.md`

**Files:**
- Create: `.claude/skills/rdd/references/roadmap-migration.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# ROADMAP Migration

The skill expects `docs/ROADMAP-*.md` to carry **hierarchical checkboxes**:

- A `[ ]` / `[x]` marker on every milestone heading (e.g. `### M1 — Foundation [ ]`).
- A `[ ]` / `[x]` list item under **Scope** for each atomic sub-item,
  identified by a stable id like `**M1.1**`.

If the file does not yet have these markers, the skill proposes a one-time
migration at Gate 1 and applies it only after the operator approves.

## Trigger

Before Phase 1 can produce a candidate list, the orchestrator checks whether
the ROADMAP already has markers.

Detection heuristic:

```bash
# count milestone headings with checkbox markers
marked=$(grep -cE '^### M[0-9]+.*\[( |x)\]\s*$' docs/ROADMAP-*.md)
total=$(grep -cE '^### M[0-9]+' docs/ROADMAP-*.md)
```

- If `marked > 0 and marked == total` — migration is not needed.
- Otherwise — propose migration.

## Transformation rules

For each milestone heading `### M# — <title>`:
1. Append ` [ ]` at the end (or ` [x]` if the operator marks it complete during
   the migration dialog).

For each bullet under the milestone's `**Scope**` section that represents an
atomic unit of work (engineering judgement + operator confirmation):
1. Convert the bullet to `- [ ] **M#.k** <rest of the bullet>` with a
   sequential `k`.

Lines that are not atomic sub-items (prose paragraphs, nested explanations,
external dependency notes) stay unchanged.

## Gate 1 dialog

Show the operator the proposed diff (unified `diff` format). Ask:

> `docs/ROADMAP-phase1.md` has no progress markers. I propose the migration
> above: `[ ]` on each milestone, and numbered `- [ ] **M#.k**` bullets for
> each atomic sub-item. This will be committed to `main` as one commit
> titled `docs(roadmap): add hierarchical progress checkboxes`. Apply?

Only apply on explicit "yes". On "no", stop the skill and tell the operator
the migration is required before any TASK can be picked.

## Application

After approval:
1. Rewrite the ROADMAP file(s).
2. Stage and commit on `main` directly (no PR):

   ```bash
   git add docs/ROADMAP-*.md
   git commit -m "docs(roadmap): add hierarchical progress checkboxes"
   git push origin main
   ```
3. Proceed to Phase 1 with the migrated file.

## Decomposition at Gate 1 (in-flight ROADMAP edit)

If the `planner` agent (see `references/agent-briefs/planner.md`) finds that an atomic
sub-item is still too large, it returns a proposed decomposition. On Gate 1
approval:

1. Replace the original bullet `- [ ] **M#.k** <title>` with a nested block:

   ```markdown
   - [ ] **M#.k** <title>
     - [ ] **M#.k.a** <sub-title-1>
     - [ ] **M#.k.b** <sub-title-2>
   ```

2. Commit on `main`:

   ```bash
   git add docs/ROADMAP-*.md
   git commit -m "docs(roadmap): decompose <M#.k> into sub-items"
   git push origin main
   ```

3. Take `M#.k.a` as the unit of work.

## Cascade at Phase 7

When marking the completed leaf `[x]`:

1. Flip the leaf bullet to `- [x] **M#.k.a.b** ...`.
2. Walk up the hierarchy: if every direct child of `M#.k.a` is `[x]`, flip
   `M#.k.a` to `[x]`. Continue up to `M#.k`, then to `M#`.
3. Stop at the first ancestor that still has an unchecked child.

This generalizes to arbitrary decomposition depth introduced at Gate 1.

## Rollback

If migration or decomposition was committed but the operator wants to revert
before any further work:

```bash
git revert <sha> --no-edit
git push origin main
```

The skill itself never rolls back — this is a manual operator action.
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/roadmap-migration.md
```
Expected: between 80 and 130 lines.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/roadmap-migration.md
git commit -m "feat(rdd): add roadmap-migration reference"
```

---

## Task 5: `references/task-template.md`

**Files:**
- Create: `.claude/skills/rdd/references/task-template.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# TASK File Template

Every `rdd` run creates exactly one `TASK-<roadmap-id>-<slug>.md` file in the
repo root at the start of Phase 2. The file is gitignored (`TASK-*.md` in
`.gitignore`) and deleted at the end of Phase 7.

## Filename

Pattern: `TASK-<roadmap-id>-<slug>.md`

- `<roadmap-id>` — e.g. `M1.1`, `M3`, `M1.9.a`. Dots kept.
- `<slug>` — kebab-case, from the sub-item title, max 40 chars.

Examples:
- `TASK-M1.1-monorepo-layout.md`
- `TASK-M1.9-ci-pipeline.md`
- `TASK-M2-keep-storage.md`

## Template (exact content to write)

```markdown
# TASK <roadmap-id> — <title>

**ROADMAP**: docs/ROADMAP-<phase>.md §M# → <roadmap-id>
**Created**: YYYY-MM-DD
**Status**: in-progress
**Branch**: rdd/<slug>
**PR**: <empty until Phase 5>

## Scope

<one paragraph: what is included and what is explicitly NOT included>

## Acceptance criteria (approved at Gate 2)

- [ ] AC1: <criterion>
- [ ] AC2: <criterion>

## Test plan (approved at Gate 2)

- [ ] Happy: <case>
- [ ] Edge: <case>
- [ ] Negative: <case>
- [ ] Security / isolation: <case>

## Plan (implementation steps)

- [ ] Step 1 — <action>
- [ ] Step 2 — <action>

## Progress log

<!-- append-only, one entry per phase transition -->

## Follow-up (nits deferred from review)

<!-- empty at start; review loop appends here -->
```

## Status transitions

- `in-progress` — created at Phase 2, active through Phase 6.
- `blocked` — set by the orchestrator when a phase escalates and the operator
  chooses `continue` to investigate without moving on.
- `cancelled` — set when the operator chooses `abort` on an escalation or
  rejects at Gate 3.
- `merged` — **transient**: set right before deletion at Phase 7 step 6.
  Because the file is then deleted, this status is observable only in the
  Progress log of the deleted file (which lives in git history of the branch
  if commits on the branch touched the file — typically they do not, since
  the file is gitignored).

For practical resume purposes, only `in-progress` and `blocked` matter.

## Progress log entries

Each entry is one Markdown heading + a short paragraph. Written by the
orchestrator, not by agents, at each phase transition.

Example:

```markdown
### 2026-04-22 14:32 — Phase 2 complete, Gate 2 passed
Acceptance criteria: 3. Test plan: 5 cases. Ready for Phase 3.

### 2026-04-22 15:10 — Phase 3 complete
executor dispatched: opus. Commits: 4 (c1b2d3e…ffeebb7). Files: 9.

### 2026-04-22 15:24 — Phase 4 iteration 1
code-reviewer: 0 blocker, 2 important, 3 nit. Dispatching fixer.

### 2026-04-22 15:31 — Phase 4 converged
2 important resolved in 1 fix iteration. 3 nits moved to Follow-up.
```

## Field precision

- `**ROADMAP**` line includes the `§` symbol and the full id path; if the
  item was decomposed, use the leaf id (e.g. `§M1 → M1.9 → M1.9.a`).
- `**Status**` must be exactly one of `in-progress`, `blocked`, `cancelled`,
  `merged`.
- `**Branch**` is set at Phase 3; empty between Phase 2 creation and Phase 3
  checkout.
- `**PR**` is set at Phase 5 (PR URL from `gh pr create`); empty before.
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/task-template.md && grep -c '^## ' .claude/skills/rdd/references/task-template.md
```
Expected: between 70 and 110 lines; `## ` heading count between 5 and 9.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/task-template.md
git commit -m "feat(rdd): add task-template reference"
```

---

## Task 6: `references/gates.md`

**Files:**
- Create: `.claude/skills/rdd/references/gates.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Operator Gates

Three mandatory synchronous checkpoints. The orchestrator prints the prompt,
waits for the operator's reply, and acts only on explicit approval. No gate
may be skipped; if the operator is unreachable, halt.

## Gate 1 — Selection and decomposition

**When:** end of Phase 1, before any TASK file is created.

**Preconditions:**
- Preflight passed.
- Candidate list computed (all `[ ]` leaf sub-items whose dependencies are
  `[x]`).
- `planner` has returned a verdict on whether the chosen sub-item fits one
  PR, and a decomposition if it does not.

**Prompt format:**

```
Gate 1 — Selection and decomposition

I will work on: <id> — <title>
  (From <roadmap-path> §<milestone>.)

Planner verdict: <fits | too large>
<if too large, list the proposed decomposition, numbered>

ROADMAP edits I will apply before continuing:
<unified diff, or "none">

Does this match what you want? (yes / revise / cancel)
```

**Accepted operator replies:**
- `yes` — apply ROADMAP edits (if any), commit+push them (see
  `references/roadmap-migration.md`), proceed to Phase 2.
- `revise` — operator provides specific edits (different item, different
  decomposition). Re-run Phase 1 with the new info.
- `cancel` — stop the skill, no side effects.

## Gate 2 — Approach and acceptance criteria

**When:** end of Phase 2, after TASK file is fully populated (Scope,
Acceptance criteria, Test plan, Plan), before any branch is created.

**Prompt format:**

```
Gate 2 — Approach and acceptance criteria for <id>

TASK file: <path>

Scope:
<Scope paragraph>

Acceptance criteria:
<list>

Test plan:
<list of cases>

Implementation plan:
<numbered steps>

Branch to be created: rdd/<slug>

Approve? (yes / revise / cancel)
```

**Accepted operator replies:**
- `yes` — proceed to Phase 3.
- `revise` — operator specifies which section to change; orchestrator edits
  the TASK file accordingly, then re-asks Gate 2.
- `cancel` — mark TASK `cancelled`, delete the TASK file, stop.

## Gate 3 — Merge

**When:** end of Phase 6, after the PR-fix loop has converged (all checks
green, no unresolved blocker/important comments).

**Prompt format:**

```
Gate 3 — Merge <PR URL>?

CI: all green
Unresolved blocker/important comments: 0
Nits / open discussions: <count, or "none">

Merging will:
1. Merge PR #<n> into main via <squash|merge|rebase> (repo default).
2. Mark <id> as [x] in <roadmap-path>, cascade parents if applicable.
3. Commit "chore(roadmap): mark <id> complete" and push to main.
4. Append Phase-7 entries to docs/LESSONS.md and .claude/skills/rdd/FEEDBACK.md.
5. Delete local branch rdd/<slug> and the remote branch.
6. Delete TASK-<id>-<slug>.md.

Proceed? (yes / no / defer)
```

**Accepted operator replies:**
- `yes` — execute Phase 7 exactly as listed.
- `no` — mark TASK `cancelled`, close the PR (keep branches for inspection),
  stop.
- `defer` — stop the skill; TASK stays `in-progress`; branch and PR stay
  open. Next run with `/rdd resume` will re-enter Gate 3.

## Halting rules

- If operator reply is empty, ambiguous, or times out (operator-defined
  timeout, default none), treat as "no response" and halt without side
  effects.
- If reply is unrecognized (not in the accepted set), ask once to clarify,
  then halt on a second unrecognized reply.
- Never infer intent from similar-sounding answers. "looks good" is not
  `yes`.
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c '^## Gate' .claude/skills/rdd/references/gates.md
```
Expected: `3`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/gates.md
git commit -m "feat(rdd): add gates reference"
```

---

## Task 7: `references/review-loop.md`

**Files:**
- Create: `.claude/skills/rdd/references/review-loop.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Phase 4 — Pre-PR Review Loop

Bounded loop that runs after the `executor` reports Phase 3 complete and
before the PR is opened. Alternates `code-reviewer` (per
`references/agent-briefs/code-reviewer.md`) and `executor` in fixer mode (per
`references/agent-briefs/executor.md`).

## Severity contract (duplicated here for the reviewer brief)

The `code-reviewer` agent must return a JSON object with three arrays:

```json
{
  "blocker":   [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ],
  "important": [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ],
  "nit":       [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ]
}
```

Definitions:
- **blocker** — violates an acceptance criterion, breaks tests, contains a
  security issue, or leaks a declared capability.
- **important** — real logic defect, violates an architecture decision
  recorded in ROADMAP §2, or is missing a test case from the approved test
  plan.
- **nit** — style, naming, minor readability, comment typos.

The orchestrator treats `blocker` and `important` as loop-blocking; `nit` is
moved to the TASK's `## Follow-up` section and never fixed in this loop.

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
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c '^## ' .claude/skills/rdd/references/review-loop.md && wc -l .claude/skills/rdd/references/review-loop.md
```
Expected: `## ` count between 4 and 7; line count between 100 and 150.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/review-loop.md
git commit -m "feat(rdd): add review-loop reference"
```

---

## Task 8: `references/pr-fix-loop.md`

**Files:**
- Create: `.claude/skills/rdd/references/pr-fix-loop.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
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
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c 'fingerprint' .claude/skills/rdd/references/pr-fix-loop.md && grep -c 'CHECK_TIMEOUT_MINUTES' .claude/skills/rdd/references/pr-fix-loop.md
```
Expected: `fingerprint` count ≥ 4; `CHECK_TIMEOUT_MINUTES` count ≥ 1.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/pr-fix-loop.md
git commit -m "feat(rdd): add pr-fix-loop reference"
```

---

## Task 9: `references/lessons-template.md`

**Files:**
- Create: `.claude/skills/rdd/references/lessons-template.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# `docs/LESSONS.md` Entry Template

Used by the `writer` agent at Phase 7 to append a new section to
`docs/LESSONS.md`.

## File structure (first time only)

If `docs/LESSONS.md` does not exist, create it with this header:

```markdown
# Project Lessons — Watchkeepers

Patterns, decisions, and lessons accumulated during implementation.
Appended by the `rdd` skill after each merged TASK (one section per TASK).

Read by the `rdd` skill at the start of Phase 2 to seed brainstorming with
prior context. Read by humans whenever.

---
```

## Per-TASK section (append this at the bottom)

Exact template:

```markdown
## <YYYY-MM-DD> — <roadmap-id>: <TASK title>

**PR**: <PR URL>
**Merged**: <YYYY-MM-DD HH:MM>

### Context
<one paragraph: what we were solving and why (from the TASK Scope)>

### Pattern
<one-to-three paragraphs: the reusable pattern or decision that emerged from
this TASK — the kind of thing the next brainstorm should consider. Concrete
names, file paths, library versions welcome>

### Anti-pattern
<optional: one paragraph if something was tried and rejected and future work
should avoid repeating it>

### References
- Files: <list of key files introduced or meaningfully modified>
- Docs: <ROADMAP section, ADR if any>

---
```

## Brevity rules

- Keep the whole section under 50 lines. If more, shrink Context and lean on
  References.
- Never copy the TASK file contents verbatim. Summarize.
- No marketing language ("robust", "elegant"). State what was done.
- If nothing new was learned (e.g. pure bugfix), still append a short entry
  so the running record is continuous.

## Example

```markdown
## 2026-04-22 — M1.1: monorepo layout

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/1
**Merged**: 2026-04-22 17:14

### Context
Bootstrapped the Go + TS monorepo layout per ROADMAP §M1 (Foundation). First
PR; establishes directory conventions the rest of Phase 1 builds on.

### Pattern
Go modules under `/core` and `/cli`, pnpm workspace under `/harness` and
`/tools-builtin`, shared CI matrix in `.github/workflows/ci.yml`. All
build targets routed through `make <target>` — no direct `go build` or
`pnpm build` calls from anywhere (including CI). When adding a new
package, mirror the structure: `/<service>/cmd/`, `/<service>/internal/`,
`/<service>/README.md`.

### References
- Files: `Makefile`, `go.work`, `pnpm-workspace.yaml`, `.github/workflows/ci.yml`
- Docs: `docs/ROADMAP-phase1.md` §M1

---
```
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/lessons-template.md && grep -c '^## ' .claude/skills/rdd/references/lessons-template.md
```
Expected: line count between 60 and 110; `## ` headings between 3 and 6.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/lessons-template.md
git commit -m "feat(rdd): add lessons-template reference"
```

---

## Task 10: `references/feedback-template.md`

**Files:**
- Create: `.claude/skills/rdd/references/feedback-template.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# `FEEDBACK.md` Entry Template

Used by the `writer` agent at Phase 7 to append a new section to
`.claude/skills/rdd/FEEDBACK.md`.

This file is the skill's self-reflection. **Nothing in it is read by the
skill at runtime.** The operator reviews it periodically and manually
promotes useful changes into `SKILL.md` or `references/*`.

## Per-TASK section (append this at the bottom)

Exact template:

```markdown
## <YYYY-MM-DD> — <roadmap-id>: <TASK title>

**PR**: <PR URL>
**Phases with incidents**: <list, or "none">

### What worked
<one-to-two paragraphs: places where the skill's process (gates, bounded
loops, agent briefs, templates) made work easier or faster>

### What wasted effort
<one-to-two paragraphs: where iterations were lost, where the operator had
to step in to unblock, where a brief was ambiguous. Be specific: "the
code-reviewer agent flagged three nits as important because the brief did
not give it examples" is useful; "reviewer was too strict" is not>

### Suggested skill changes
- <file-level suggestion: "tighten severity contract in
  references/review-loop.md §'important' to exclude X">
- <may be empty>

### Metrics
- Review iterations: <n>
- PR-fix iterations: <n>
- Operator interventions outside of gates: <n>
- Total wall time from /rdd to merge: <HH:MM>

---
```

## Tone rules

- Concrete and blunt. This is an internal log; flattery and hedging are
  noise.
- No suggestions that touch the business code — use `LESSONS.md` for that.
  This file is strictly about the skill itself.
- No "we should refactor the whole X" entries. Suggest one tight change at
  a time; a later operator review promotes or rejects it.

## Example

```markdown
## 2026-04-22 — M1.1: monorepo layout

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/1
**Phases with incidents**: 4

### What worked
Gate 2 flushed out a scope ambiguity that would have burned a review
iteration — operator caught that "Go quality stack" was implicit in "Layout".

### What wasted effort
Phase 4 iteration 1 produced 6 `important` items; 4 of them were really
nits (field ordering in struct literals). The reviewer brief does not
list examples of "important vs nit" so the classification drifted.

### Suggested skill changes
- Add an "important vs nit" example table to
  `references/agent-briefs/code-reviewer.md`.
- Consider raising the iteration budget for the first TASK per milestone
  from 5 to 7 (foundational PRs attract more comments).

### Metrics
- Review iterations: 3
- PR-fix iterations: 1
- Operator interventions outside of gates: 1 (stuck CI cache, manual flush)
- Total wall time from /rdd to merge: 04:37

---
```
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/feedback-template.md
```
Expected: between 60 and 110 lines.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/feedback-template.md
git commit -m "feat(rdd): add feedback-template reference"
```

---

## Task 11: `references/agent-briefs/planner.md`

**Files:**
- Create: `.claude/skills/rdd/references/agent-briefs/planner.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Agent Brief — planner (Phase 1)

The orchestrator dispatches this agent once per `rdd` run, during Phase 1,
after the operator has selected a ROADMAP sub-item (or one was passed via
argument). The agent decides whether that sub-item fits a single PR and, if
not, proposes a decomposition.

## Preferred `subagent_type`

`oh-my-claudecode:planner` (fallback: `general-purpose`).

## Input (pass in the agent prompt)

- **Sub-item id and title.**
- **Relevant ROADMAP section** (the `### M#` block containing the chosen
  sub-item, verbatim).
- **Repository snapshot**: first level of the repo tree and paths of
  relevant existing files (e.g. `Makefile`, `.github/workflows/*`,
  `docs/*`). This is metadata so the planner can compare the ask against
  the current state.
- **Prior LESSONS excerpt** (`docs/LESSONS.md` entries for the same
  milestone, if any) — so the planner does not propose decomposition that
  the project has already rejected.

## Heuristic: "fits one PR"

A sub-item fits one PR if, in the planner's judgement, **all** of the
following hold:

1. Estimated implementation ≤ ~1 engineer-day of focused work.
2. Touches ≤ ~15 files (code + tests + configs + docs combined).
3. Represents a single coherent concern (no "and also" in the title).
4. No ambiguous dependency on another unmerged sub-item in the same
   milestone.

If any condition fails, the sub-item is too large.

## Output contract (exact JSON)

```json
{
  "fits": true,
  "reason": "single Makefile + CI matrix entry; ≤ 6 files; ≤ 0.5 day",
  "decomposition": null
}
```

or, when decomposition is needed:

```json
{
  "fits": false,
  "reason": "Mixed: Makefile + Go toolchain + TS toolchain are three coherent concerns, each deserving its own PR",
  "decomposition": [
    { "id": "M1.1.a", "title": "Go module layout and top-level Makefile" },
    { "id": "M1.1.b", "title": "Go quality stack (golangci-lint + govulncheck)" },
    { "id": "M1.1.c", "title": "TypeScript quality stack (tsc + eslint + vitest)" }
  ]
}
```

Rules for decomposition:
- Ids follow the `<parent>.a`, `<parent>.b`, ... convention. Use lowercase
  letters.
- Each sub-item must itself pass the "fits one PR" heuristic.
- Decomposition must be **partitional**: union covers the parent, intersections are empty.
- Titles are imperative, ≤ 80 chars.

## Hard rules

- Do not speculate about future milestones; only assess the one sub-item
  passed in.
- Do not write files. Output JSON and nothing else.
- Do not propose decomposition if the sub-item fits — return `fits: true`
  even if you see ways the work "could" be split.
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c '```json' .claude/skills/rdd/references/agent-briefs/planner.md
```
Expected: `2`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/agent-briefs/planner.md
git commit -m "feat(rdd): add planner agent brief"
```

---

## Task 12: `references/agent-briefs/explore.md`

**Files:**
- Create: `.claude/skills/rdd/references/agent-briefs/explore.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Agent Brief — explore (Phase 2, optional)

The orchestrator optionally dispatches this agent during Phase 2 to gather
repository context for the brainstorm. **Read-only** — the agent must not
write any files or run any mutating commands.

## Preferred `subagent_type`

`oh-my-claudecode:explore` (fallback: `Explore`).

## Input (pass in the agent prompt)

- **TASK scope paragraph** (just the Scope section from the in-progress
  TASK file).
- **Explicit question(s)** the orchestrator wants answered, e.g.:
  - "Where are Go module boundaries currently defined?"
  - "Does any existing file already configure golangci-lint?"
  - "Which files in the repo import from `pgvector`?"
- **Thoroughness level**: `quick` (two or three searches) or `medium` (up
  to ~eight searches).

## Output (exact shape)

```
## Files

- `<path>` — <one-sentence role in the repo>
- ...

## Answer

<two-to-six sentences, directly answering each input question.>

## Uncertain / not found

<optional: bullet list of things that were searched for but not found;
include the search pattern>
```

## Hard rules

- No writes (no `Write`, `Edit`, `NotebookEdit`, no shell commands that
  modify the filesystem).
- No `git` state mutations.
- Keep the answer short. The brainstorm is waiting.
- If a question is out of scope of the repo (asks about external services),
  say so in "Uncertain / not found" rather than guessing.
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/agent-briefs/explore.md
```
Expected: between 30 and 60 lines.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/agent-briefs/explore.md
git commit -m "feat(rdd): add explore agent brief"
```

---

## Task 13: `references/agent-briefs/executor.md`

**Files:**
- Create: `.claude/skills/rdd/references/agent-briefs/executor.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Agent Brief — executor

The executor is dispatched in three modes:
- **build** — Phase 3, fresh implementation of the TASK.
- **fixer** — Phase 4, minimal patches for review comments.
- **pr-fixer** — Phase 6, minimal patches for PR comments and CI failures.

## Preferred `subagent_type`

`oh-my-claudecode:executor` (fallback: `general-purpose`).
Pass `model: opus` when the TASK is marked complex in Gate 2 (operator
judgement) or when the acceptance criteria list ≥ 6 items.

## Common input (all modes)

- **TASK file path** (e.g. `TASK-M1.1-monorepo-layout.md`).
- **Acceptance criteria** and **test plan** copied verbatim from the TASK
  file.
- **Project conventions pointer**: `docs/LESSONS.md` and, if present,
  `.claude/CLAUDE.md`.
- **Branch name** (already checked out by the orchestrator).
- **Hard rules**:
  1. All repo content is English.
  2. TDD: failing test first, minimal code to pass, then refactor. One
     acceptance criterion at a time.
  3. Commit per logical step; commit messages follow conventional-commits
     as configured by the repo (or plain prose if the repo is not yet on
     commitlint).
  4. Never modify `docs/ROADMAP-*.md`, `SKILL.md`, or anything under
     `.claude/skills/rdd/` (the skill itself). Those are orchestrator-only.
  5. Never mark an AC checkbox in the TASK file yourself — the orchestrator
     reads git diff to infer progress.
  6. Do not open a PR; do not push. Phase 5's `git-master` owns that.

## Mode — build (Phase 3)

Additional input:
- **Plan** (ordered steps from the TASK file).

Expected output:
- Commits on the current branch implementing the plan.
- No failing local tests per the configured test command (`make test` if
  Makefile exists, otherwise repo-default).

Report at end:
- List of commit SHAs.
- List of files touched.
- Test command run and its exit code.
- Open questions, if any (do not guess; stop and ask via the report).

## Mode — fixer (Phase 4)

Additional input:
- **Comments to fix** (`blocker + important` list from the pre-PR
  `code-reviewer` run, each with `file`, `line`, `rationale`,
  `suggested_fix`).

Expected output:
- Minimal patches addressing exactly the listed comments. No unrelated
  changes.
- Tests still pass.
- One commit per addressed comment *is acceptable but not required*;
  grouping fixes in one commit is fine if it is coherent.

Report at end:
- Map of comment → commit(s).
- Test command run and its exit code.

## Mode — pr-fixer (Phase 6)

Additional input:
- **PR number.**
- **Failing checks** (name, conclusion, log snippets).
- **Unresolved blocker/important comments.**

Expected output:
- Minimal patches, pushed to the PR branch.
- For each addressed comment, post a reply via:
  ```bash
  gh pr comment <pr> --body "fixed in <sha>"
  ```
  Do **not** resolve the comment thread.
- Tests still pass locally before push.

Report at end:
- Map of comment/check → commit(s) + push sha.
- For each comment you did NOT address, a one-sentence reason (if any
  were consciously deferred).

## Anti-patterns (all modes)

- "While I was there I also cleaned up …" — refuse. Scope discipline.
- Adding new dependencies without a justification line in the commit body.
- Renaming files or symbols unless the TASK plan explicitly calls for it.
- Silent changes to public API surface outside the diff the comments
  addressed.
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c '^## Mode' .claude/skills/rdd/references/agent-briefs/executor.md
```
Expected: `3`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/agent-briefs/executor.md
git commit -m "feat(rdd): add executor agent brief"
```

---

## Task 14: `references/agent-briefs/code-reviewer.md`

**Files:**
- Create: `.claude/skills/rdd/references/agent-briefs/code-reviewer.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Agent Brief — code-reviewer (Phase 4)

Dispatched once per Phase 4 iteration. Reviews the diff against the TASK's
acceptance criteria and returns a severity-rated list.

## Preferred `subagent_type`

`oh-my-claudecode:code-reviewer` (fallback: `superpowers:code-reviewer`).

## Input (pass in the agent prompt)

- **TASK file path** (for Scope, Acceptance criteria, Test plan).
- **Diff vs main**: full output of `git diff main...HEAD` (or an equivalent
  line-numbered representation).
- **Previous iteration's blocker+important list** (if not the first
  iteration). The reviewer should check whether these are now fixed
  (expected) and whether fixes introduced new issues.
- **Stop conditions**: the reviewer returns after its first complete pass —
  no back-and-forth.

## Output contract (exact JSON, no prose around it)

```json
{
  "blocker":   [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ],
  "important": [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ],
  "nit":       [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ]
}
```

Each item is `{file, line, rationale, suggested_fix}`:
- `file` — path relative to repo root.
- `line` — line number in the changed file at HEAD. For changes that span
  multiple lines, the first affected line.
- `rationale` — one sentence. Must cite either an acceptance criterion, a
  test case from the test plan, a ROADMAP §2 architecture decision, or a
  specific project convention from `docs/LESSONS.md` / `.claude/CLAUDE.md`.
- `suggested_fix` — one-to-three sentences describing the minimal patch.
  Code snippet acceptable if small.

## Severity definitions

- **blocker** — any ONE of:
  - violates an acceptance criterion from the TASK;
  - breaks a test listed in the test plan (including missing tests marked
    mandatory);
  - introduces a security issue (secret leak, unsafe deserialization,
    unbounded input, TOCTOU, etc.);
  - leaks a capability beyond what the TASK declared;
  - breaks the build / typecheck / lint gate configured for this repo.

- **important** — any ONE of:
  - real logic defect (wrong branch, off-by-one, swallowed error) that is
    not caught by an existing test;
  - violates an architecture decision recorded in `docs/ROADMAP-phase1.md §2`;
  - missing a test case from the approved test plan (happy / edge /
    negative / security);
  - inconsistent with a pattern established in `docs/LESSONS.md` when
    deviation has no stated reason.

- **nit** — everything else, including:
  - naming preferences;
  - comment style, typos;
  - field ordering;
  - local readability improvements that do not change behavior.

## Hard rules

- The JSON is the entire response. No preamble, no closing commentary.
- Do not propose architectural rewrites as part of `important`. Suggest
  only changes that are reachable inside the current diff.
- If the diff is clean, return `{"blocker": [], "important": [], "nit": []}`.
- Do not downgrade real defects to `nit` because they are "minor". Severity
  is about impact, not tone.
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c '```json' .claude/skills/rdd/references/agent-briefs/code-reviewer.md
```
Expected: `1`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/agent-briefs/code-reviewer.md
git commit -m "feat(rdd): add code-reviewer agent brief"
```

---

## Task 15: `references/agent-briefs/git-master.md`

**Files:**
- Create: `.claude/skills/rdd/references/agent-briefs/git-master.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Agent Brief — git-master

Two modes:
- **pr** — Phase 5: finalize commit style, push, `gh pr create`.
- **merge** — Phase 7: merge the PR, update ROADMAP checkboxes, push, clean
  up branches.

## Preferred `subagent_type`

`oh-my-claudecode:git-master` (fallback: `general-purpose`).

## Mode — pr (Phase 5)

Input:
- **TASK file path** (for Scope, Acceptance criteria, Test plan — used to
  compose the PR body).
- **Branch name** (`rdd/<slug>`).
- **Base branch** (`main`).
- **Commit-style policy**: follow the style already present in `git log` on
  `main` (heuristic: if ≥ 3 of the last 10 commits use conventional-commits,
  use conventional-commits; otherwise use plain imperative prose).

Actions:
1. Review the commit series on the branch. If commits are coherent, leave
   them. If there is obvious noise (typo fixups, "wip" messages), offer to
   squash — but do not squash without explicit confirmation (report back to
   the orchestrator; orchestrator asks the operator).
2. Push the branch: `git push --set-upstream origin rdd/<slug>`.
3. Open the PR:
   ```bash
   gh pr create --base main --head rdd/<slug> \
     --title "<title>" \
     --body "$(cat <<'EOF'
   ## Summary
   <Scope paragraph from TASK>

   ## Acceptance criteria
   <checklist — use the TASK list as-is>

   ## Test plan
   <checklist — use the TASK list as-is>

   ## Linked ROADMAP item
   docs/ROADMAP-<phase>.md §M# → <roadmap-id>

   🤖 Generated with /rdd
   EOF
   )"
   ```
4. Return the PR URL.

Hard rules:
- Do NOT merge in this mode.
- Do NOT force-push. If the remote branch exists and diverges, stop and
  report.
- PR title ≤ 70 chars. Derived from TASK title (prepend `<roadmap-id>: `).
- `--draft` is off (ready for review).

## Mode — merge (Phase 7)

Input:
- **PR number**.
- **Merge method**: default `squash`; use the repo default if configurable
  via `.github/settings.yml` or `gh api repos/:owner/:repo`.
- **Leaf roadmap id** (e.g. `M1.9.a`) — the one to mark `[x]`.
- **ROADMAP path** (e.g. `docs/ROADMAP-phase1.md`).

Actions:
1. Merge the PR:
   ```bash
   gh pr merge <pr> --<method> --delete-branch
   ```
   (`--delete-branch` removes the remote branch.)
2. Pull main locally:
   ```bash
   git checkout main
   git pull --ff-only origin main
   ```
3. Delete the local branch:
   ```bash
   git branch -D rdd/<slug>
   ```
4. Update the ROADMAP:
   - Mark the leaf `- [ ] **<leaf-id>** ...` → `- [x] **<leaf-id>** ...`.
   - Walk ancestors upward (see `references/roadmap-migration.md` §Cascade).
     For each ancestor whose direct children are all `[x]`, flip the
     ancestor to `[x]`.
5. Commit and push:
   ```bash
   git add docs/ROADMAP-*.md
   git commit -m "chore(roadmap): mark <leaf-id> complete"
   git push origin main
   ```

Hard rules:
- Never merge without an explicit instruction from the orchestrator (which
  only sends this brief after Gate 3).
- If the ROADMAP update commit fails to push (non-fast-forward), do not
  force. Pull, attempt to reapply the checkbox edits on top of the new
  HEAD, commit, push. If conflicts persist, return failure — the merge
  already happened, only the bookkeeping is stuck, and the orchestrator
  will escalate.
- Report: merge sha, new `main` sha, list of ROADMAP ids flipped (leaf plus
  any cascaded ancestors).
````

- [ ] **Step 2: Verify file**

Run:
```bash
grep -c '^## Mode' .claude/skills/rdd/references/agent-briefs/git-master.md
```
Expected: `2`.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/agent-briefs/git-master.md
git commit -m "feat(rdd): add git-master agent brief"
```

---

## Task 16: `references/agent-briefs/writer.md`

**Files:**
- Create: `.claude/skills/rdd/references/agent-briefs/writer.md`

- [ ] **Step 1: Write the file with exact content below**

````markdown
# Agent Brief — writer (Phase 7)

Dispatched once at the end of Phase 7, after the merge and the ROADMAP
commit. Appends one section to each of:
- `docs/LESSONS.md` — project-level patterns.
- `.claude/skills/rdd/FEEDBACK.md` — skill self-reflection.

## Preferred `subagent_type`

`oh-my-claudecode:writer` (fallback: `general-purpose`).

## Input (pass in the agent prompt)

- **Roadmap id, title, PR URL, merge timestamp.**
- **Branch name** (for history look-ups if needed).
- **TASK file path** — still present at this point; deleted by the
  orchestrator *after* this agent returns.
- **Progress log excerpts** from the TASK file covering each phase —
  especially Phase 4 and Phase 6 iteration entries, for the FEEDBACK
  section.
- **Templates**:
  - `references/lessons-template.md`
  - `references/feedback-template.md`

## Actions

1. If `docs/LESSONS.md` does not exist, create it with the header from
   `references/lessons-template.md` §"File structure (first time only)".
2. Append a new section to `docs/LESSONS.md` following the exact template
   in `references/lessons-template.md` §"Per-TASK section".
3. Append a new section to `.claude/skills/rdd/FEEDBACK.md` following the
   exact template in `references/feedback-template.md` §"Per-TASK section".
4. Stage and commit both files in one commit:
   ```bash
   git add docs/LESSONS.md .claude/skills/rdd/FEEDBACK.md
   git commit -m "docs: record lessons and feedback from <roadmap-id>"
   git push origin main
   ```

## Hard rules

- Use the templates verbatim for structure. Only the content inside each
  section varies.
- Do not edit existing sections of either file; append only.
- Keep each section under 50 lines. Trim if needed.
- Never suggest changes to business code in `FEEDBACK.md` (that belongs in
  `LESSONS.md`). Never suggest changes to the skill in `LESSONS.md` (that
  belongs in `FEEDBACK.md`).
- If you find that you have nothing new to add in either file, still append
  a minimal entry — the running record must be continuous, and the pattern
  of minimal entries is useful signal.

## Report

Back to the orchestrator:
- New commit sha.
- Character count of each appended section.
````

- [ ] **Step 2: Verify file**

Run:
```bash
wc -l .claude/skills/rdd/references/agent-briefs/writer.md
```
Expected: between 40 and 70 lines.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/rdd/references/agent-briefs/writer.md
git commit -m "feat(rdd): add writer agent brief"
```

---

## Task 17: Final cross-reference and smoke validation

This task does not create new files. It verifies the skill is internally
consistent: every relative reference from `SKILL.md` and
`references/*.md` points to a file that actually exists, and the skill has
the full file set called for by the spec.

**Files:** none created; read-only verification.

- [ ] **Step 1: List the complete skill tree**

Run:
```bash
find .claude/skills/rdd -type f | sort
```
Expected exact output (17 lines):
```
.claude/skills/rdd/FEEDBACK.md
.claude/skills/rdd/SKILL.md
.claude/skills/rdd/references/agent-briefs/code-reviewer.md
.claude/skills/rdd/references/agent-briefs/executor.md
.claude/skills/rdd/references/agent-briefs/explore.md
.claude/skills/rdd/references/agent-briefs/git-master.md
.claude/skills/rdd/references/agent-briefs/planner.md
.claude/skills/rdd/references/agent-briefs/writer.md
.claude/skills/rdd/references/feedback-template.md
.claude/skills/rdd/references/gates.md
.claude/skills/rdd/references/lessons-template.md
.claude/skills/rdd/references/pr-fix-loop.md
.claude/skills/rdd/references/preflight.md
.claude/skills/rdd/references/review-loop.md
.claude/skills/rdd/references/roadmap-migration.md
.claude/skills/rdd/references/task-template.md
```
(16 files, sorted). If any line is missing or extra, go back to the
corresponding Task and fix.

- [ ] **Step 2: Resolve every `references/...md` link from every skill file**

Convention: every internal link in the skill uses a path relative to the
**skill root** (`.claude/skills/rdd/`), i.e. it starts with `references/`.
With this convention, link-checking is a simple existence test from the
skill root.

Run:
```bash
cd .claude/skills/rdd
found_broken=0
for f in SKILL.md references/*.md references/agent-briefs/*.md; do
  for path in $(grep -oE 'references/[a-zA-Z0-9_/-]+\.md' "$f" | sort -u); do
    if [ ! -f "$path" ]; then
      echo "BROKEN: $f -> $path"
      found_broken=1
    fi
  done
done
cd - >/dev/null
exit $found_broken
```
Expected: no `BROKEN:` lines printed; exit code 0.

If any line is printed, fix the referencing file (rewrite the link using
the skill-root-relative form `references/...md`). The target file should
exist already — it was created by an earlier Task.

- [ ] **Step 3: Smoke-check `SKILL.md` frontmatter**

Run:
```bash
head -3 .claude/skills/rdd/SKILL.md
```
Expected (exactly these three lines):
```
---
name: rdd
description: Roadmap-Driven Development orchestrator. Use when the operator runs
```

- [ ] **Step 4: Smoke-check no placeholders remain**

Run:
```bash
grep -nE 'TBD|TODO|FIXME|XXX' .claude/skills/rdd/ -r
```
Expected: no matches.

- [ ] **Step 5: Verify `.gitignore` excludes `TASK-*.md` and the skill is staged**

Run:
```bash
grep -n 'TASK-\*\.md' .gitignore && git status --short .claude/skills/rdd
```
Expected: `.gitignore` line printed; `git status` shows no unstaged or
untracked files under `.claude/skills/rdd` (everything committed).

- [ ] **Step 6: Commit verification note (optional, if any doc-only fix was made in Step 2)**

If Step 2 surfaced and you repaired any broken reference, commit the fix:
```bash
git add .claude/skills/rdd
git commit -m "fix(rdd): repair broken internal references"
```
Otherwise skip.

- [ ] **Step 7: Summary to operator**

Print to the operator:
```
rdd skill implementation complete.
Files: 16
Commits: 16 (one per file) + 1 scaffolding + optional fixups.
Next: run `/rdd` on a real ROADMAP item (not part of this plan).
```

---

## Spec coverage self-check (plan author note)

| Spec requirement | Task(s) |
|------------------|---------|
| §3.1 #1 Task selection (`/rdd`, `/rdd <id>`, `/rdd resume`) | 2 (SKILL.md invocation section) |
| §3.1 #2 Decomposition at selection | 4 (roadmap-migration), 11 (planner) |
| §3.1 #3 Atomic TASK (1:1 with sub-item and PR) | 5 (task-template) |
| §3.1 #4 Brainstorm with acceptance criteria | 5, 6 (gates §Gate 2) |
| §3.1 #5 Branch from main as `rdd/<slug>` | 3 (preflight §Check 5), 13 (executor) |
| §3.1 #6 Orchestrator-only main process | 2 (SKILL.md Hard rules), 11–16 (every brief) |
| §3.1 #7 Pre-PR bounded review loop | 7 (review-loop), 14 (code-reviewer) |
| §3.1 #8 PR creation | 15 (git-master §Mode pr) |
| §3.1 #9 PR-fix bounded loop | 8 (pr-fix-loop), 13 (executor §Mode pr-fixer) |
| §3.1 #10 Gate-3 merge | 6 (gates §Gate 3), 15 (git-master §Mode merge) |
| §3.1 #11 ROADMAP update on merge (cascade) | 4 (roadmap-migration §Cascade), 15 (git-master §Mode merge) |
| §3.1 #12 Two-tier knowledge write | 9 (lessons), 10 (feedback), 16 (writer) |
| §3.2 Language = English | 1 (FEEDBACK seed), 2 (Hard rule #7) |
| §3.2 Safety preflight | 3 (preflight) |
| §3.2 Recovery | 2 (SKILL.md State recovery) |
| §3.2 Isolation | 2, 11–16 (every brief is self-contained) |
| §8 Failure modes table | 3, 7, 8 (preflight + both loops cover every row) |
| §9 Hard rules 1–8 | 2 (SKILL.md Hard rules) |
| §10.1 File layout | all tasks |
| §10.2 SKILL.md scaffold | 2 |

No spec item is left without a task. No task introduces content outside the
spec.
