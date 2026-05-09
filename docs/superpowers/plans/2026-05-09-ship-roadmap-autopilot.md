# `ship-roadmap-autopilot` Skill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Materialize the approved `ship-roadmap-autopilot` skill at `.claude/skills/ship-roadmap-autopilot/` per the design in `docs/superpowers/specs/2026-05-09-ship-roadmap-autopilot-design.md`.

**Architecture:** Project-level skill. The skill is a thin orchestrator: it loads `.omc/state/ship-autopilot.json`, runs pre-flight guards, optionally cascade-marks parents, picks the next leaf `[ ]` from `docs/ROADMAP-phase*.md`, dispatches one `Agent` per tick to ship the item end-to-end, and persists the result. Each iteration runs in a fresh subagent context (the orchestrator session retains only ~200 tokens of JSON summary per tick). Operator drives the loop with `/loop /oh-my-claudecode:ship-roadmap-autopilot`.

**Tech Stack:** Markdown only. No code is executed by this plan — we are authoring skill content. The skill's runtime artifacts (state JSON, log) live under `.omc/state/` (already gitignored).

**Commit discipline:** one commit per file, message template shown in each task. Atomic commits make review and revert trivial. The whole plan ships as a single squash PR per ship-roadmap-item conventions, so commits in this plan are intermediate — they remain on a feature branch (`feature/ship-roadmap-autopilot`) until the final task.

**How to treat code blocks:** every `Step: Write the file` shows the **complete final content** in a fenced block. Copy verbatim into the file; do not paraphrase. If anything seems off, stop and ask — do not ad-lib.

---

## File Structure

```
.claude/skills/ship-roadmap-autopilot/
├── SKILL.md                            (Task 5)
└── references/
    ├── state-schema.md                 (Task 2)
    ├── auto-rules.md                   (Task 3)
    └── agent-prompt.md                 (Task 4)
```

Root-level changes: none. `.omc/state/` is already gitignored.

---

## Task 1: Scaffold — directories + feature branch

**Files:**
- Create: `.claude/skills/ship-roadmap-autopilot/` (directory)
- Create: `.claude/skills/ship-roadmap-autopilot/references/` (directory)

- [ ] **Step 1: Create feature branch**

Run:
```bash
git checkout main && git pull --ff-only origin main
git checkout -b feature/ship-roadmap-autopilot
```

Expected: working tree clean, on `feature/ship-roadmap-autopilot`.

- [ ] **Step 2: Create directories**

Run:
```bash
mkdir -p .claude/skills/ship-roadmap-autopilot/references
```

- [ ] **Step 3: Verify directories**

Run:
```bash
ls -la .claude/skills/ship-roadmap-autopilot/
```
Expected: shows `references/` subdirectory.

(No commit yet — empty directories are not tracked by git. The first commit lands in Task 2.)

---

## Task 2: `references/state-schema.md`

**Files:**
- Create: `.claude/skills/ship-roadmap-autopilot/references/state-schema.md`

- [ ] **Step 1: Write the file**

Write the following content verbatim into `.claude/skills/ship-roadmap-autopilot/references/state-schema.md`:

````markdown
# State schema (`.omc/state/ship-autopilot.json`)

Single source of truth between ticks. Read at tick start, written at
tick end. `.omc/state/` is gitignored at repo root, so this file is
local to the operator's checkout.

## Shape

```json
{
  "version": 1,
  "halted": false,
  "halt_reason": null,
  "halt_detail": null,
  "started_at": "2026-05-09T22:30:00Z",
  "iterations_total": 0,
  "iterations_shipped": 0,
  "iterations_halted": 0,
  "last_item": null,
  "history": []
}
```

After at least one shipped iteration:

```json
{
  "version": 1,
  "halted": false,
  "halt_reason": null,
  "halt_detail": null,
  "started_at": "2026-05-09T22:30:00Z",
  "iterations_total": 12,
  "iterations_shipped": 11,
  "iterations_halted": 1,
  "last_item": {
    "id": "M7.2.b",
    "status": "shipped",
    "pr": 120,
    "sha": "f6aa80c",
    "duration_sec": 740,
    "review_iter1_findings": {"critical": 0, "major": 1, "minor": 2, "nit": 1}
  },
  "history": [
    {"ts": "2026-05-09T22:18:00Z", "id": "M7.2.b", "status": "shipped", "pr": 120, "sha": "f6aa80c"},
    {"ts": "2026-05-09T21:55:00Z", "id": "M7.2.a", "status": "shipped", "pr": 119, "sha": "f98a96c"}
  ]
}
```

## Halt reasons

Two origins. The agent JSON return enum (see `agent-prompt.md`) carries
only the agent-side reasons; the orchestrator may also halt before any
Agent dispatch.

### Agent-side (returned by ship-Agent)

| Reason | When |
|---|---|
| `aggregate-needs-decomposition` | Agent inspected the M-id and saw no concrete AC bullets / scope >1000 LOC / >20 files / >1 PR |
| `phase2-uncertainty` | Agent could not pattern-match an API fork |
| `build-failed` | `go build/vet/test -race` failed after 1 fix attempt |
| `review-blocker` | codex/critic blocker not resolved after fallback |
| `ci-red` | GitHub CI failed 3 times in a row |
| `merge-failed` | `gh pr merge --squash` failed |

### Orchestrator-side (set without an Agent dispatch)

| Reason | When |
|---|---|
| `roadmap-complete` | All `[ ]` exhausted (terminal success) |
| `aggregate-needs-decomposition` | Picker hit a `[ ]` parent without leaf decomposition (also reachable from the agent side; same reason, set by either) |
| `dirty-working-tree` | `git status --porcelain` non-empty at tick start |
| `wrong-branch` | Orchestrator not on `main` at tick start |
| `rdd-session-active` | `.omc/state/rdd-active` marker present |
| `unknown` | Agent returned non-JSON / crashed (orchestrator records this after parse failure) |

## Log file

`.omc/state/ship-autopilot.log` — append-only, one line per tick:

```
2026-05-09T22:18Z  M7.2.b  shipped   pr=120  sha=f6aa80c  dur=740s
2026-05-09T22:30Z  M7.2.c  halted    reason=phase2-uncertainty  detail="resolver vs static for SpawnClaim"
```
````

- [ ] **Step 2: Verify**

Run:
```bash
cat .claude/skills/ship-roadmap-autopilot/references/state-schema.md | head -20
```
Expected: shows the `# State schema` heading and the opening `{` of the empty-state JSON.

- [ ] **Step 3: Commit**

Run:
```bash
git add .claude/skills/ship-roadmap-autopilot/references/state-schema.md
git commit -m "$(cat <<'EOF'
docs(ship-autopilot): add state-schema reference

State JSON shape, halt-reason enum split (agent-side vs
orchestrator-side), and log line format for ship-autopilot.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `references/auto-rules.md`

**Files:**
- Create: `.claude/skills/ship-roadmap-autopilot/references/auto-rules.md`

- [ ] **Step 1: Write the file**

Write the following content verbatim into `.claude/skills/ship-roadmap-autopilot/references/auto-rules.md`:

````markdown
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
````

- [ ] **Step 2: Verify**

Run:
```bash
grep -c "^## " .claude/skills/ship-roadmap-autopilot/references/auto-rules.md
```
Expected: `8` (eight section headings).

- [ ] **Step 3: Commit**

Run:
```bash
git add .claude/skills/ship-roadmap-autopilot/references/auto-rules.md
git commit -m "$(cat <<'EOF'
docs(ship-autopilot): add auto-decision rules reference

Auto-rules for the two operator-question points in ship-roadmap-item
(Phase 2 API fork, Phase 7 fix scope) plus CI/build/merge gates and
the no-interactive-tools mandate.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `references/agent-prompt.md`

**Files:**
- Create: `.claude/skills/ship-roadmap-autopilot/references/agent-prompt.md`

- [ ] **Step 1: Write the file**

Write the following content verbatim into `.claude/skills/ship-roadmap-autopilot/references/agent-prompt.md`:

````markdown
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
````

- [ ] **Step 2: Verify**

Run:
```bash
grep -E "^## (Template|Cascade-pass)" .claude/skills/ship-roadmap-autopilot/references/agent-prompt.md
```
Expected: two matching lines.

- [ ] **Step 3: Commit**

Run:
```bash
git add .claude/skills/ship-roadmap-autopilot/references/agent-prompt.md
git commit -m "$(cat <<'EOF'
docs(ship-autopilot): add agent-prompt templates

Two prompts: ship-Agent (full ship-roadmap-item run) and cascade-pass
writer-Agent (doc-only ROADMAP parent flips). Both return strict JSON.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `SKILL.md`

**Files:**
- Create: `.claude/skills/ship-roadmap-autopilot/SKILL.md`

- [ ] **Step 1: Write the file**

Write the following content verbatim into `.claude/skills/ship-roadmap-autopilot/SKILL.md`:

````markdown
---
name: ship-roadmap-autopilot
description: Autopilot wrapper around ship-roadmap-item. One Agent dispatch per
  ROADMAP leaf, halt-on-blocker policy, cascade-marks parents, fresh subagent
  context per iteration. Use as `/loop /oh-my-claudecode:ship-roadmap-autopilot`.
triggers:
  - "/ship-roadmap-autopilot"
  - "ship-autopilot"
  - "автопилот по roadmap"
  - "autopilot roadmap"
---

# ship-roadmap-autopilot — autonomous ROADMAP shipper

Drive `ship-roadmap-item` end-to-end against `docs/ROADMAP-phase*.md` in
a loop with no operator intervention. Each iteration produces one merged
PR on `main`. Halts on any blocker; the operator fixes the blocker, runs
`reset`, and continues.

The orchestrator session is thin. Each tick spawns one ship-Agent (and
optionally one writer-Agent for the cascade-pass). Both agents run in
fresh subagent contexts; the orchestrator only retains a small JSON
summary per iteration.

Reference docs (read on demand, not at tick start):

- `references/state-schema.md` — JSON shape and halt-reason enum
- `references/auto-rules.md` — phase-2 / phase-7 auto-defaults + gates
- `references/agent-prompt.md` — ship-Agent and writer-Agent prompts

## Operator interface

| Command | Behaviour |
|---|---|
| `/oh-my-claudecode:ship-roadmap-autopilot` | One tick. Manual / debug / dry-run. |
| `/loop /oh-my-claudecode:ship-roadmap-autopilot` | Self-paced loop. Recommended. |
| `/loop 30m /oh-my-claudecode:ship-roadmap-autopilot` | Fixed-interval loop (rare; ticks already self-pace 5–20 min). |
| `/oh-my-claudecode:ship-roadmap-autopilot reset` | Clear `halted/halt_reason/halt_detail`; keep `history`. After fixing a blocker. |
| `/oh-my-claudecode:ship-roadmap-autopilot status` | Print last 5 iterations + halt flag in markdown. |

## Per-tick procedure

### Step 0 — Arg dispatch

If invoked with `reset`:
1. Read `.omc/state/ship-autopilot.json` (initialise empty if absent).
2. Set `halted=false`, `halt_reason=null`, `halt_detail=null`. Keep `history`.
3. Write back.
4. Print `✅ ship-autopilot halt cleared. Last item: <last_item.id> (<last_item.status>).`
5. Exit tick.

If invoked with `status`:
1. Read `.omc/state/ship-autopilot.json` (initialise empty if absent).
2. Render last 5 entries of `history` and `halted/halt_reason/halt_detail` as a markdown table.
3. Exit tick.

Otherwise fall through to Step 1.

### Step 1 — Load state

Read `.omc/state/ship-autopilot.json`. If the file is absent, create it
with the empty-state shape from `references/state-schema.md`. If the
file is malformed JSON, halt with `halt_reason="unknown"`,
`halt_detail="state file corrupt: <parse error>"`.

### Step 2 — Pre-flight guards

Check, in order. First failure halts the tick (write halt fields to state, exit):

1. `git status --porcelain` must produce empty output → else halt
   `dirty-working-tree`.
2. `git rev-parse --abbrev-ref HEAD` must return `main` → else halt
   `wrong-branch` with `halt_detail=<current-branch>`.
3. `.omc/state/rdd-active` must NOT exist → else halt
   `rdd-session-active`.

### Step 3 — Halt-check

If `state.halted == true`, print:

```
🛑 ship-autopilot HALTED: <halt_reason> — <halt_detail>
```

Exit the tick. (To unblock: operator runs `reset` after fixing the
underlying issue.)

### Step 4 — Cascade-pass

Read all of `docs/ROADMAP-phase1.md` … `docs/ROADMAP-phase6.md`.

For each `[ ]` line that is a parent (matches `^- \[ \] \*\*M[0-9.]+\*\*`
and has `M<x>.<y>` numbered children below it within the same section):

1. Find all numbered children directly under this parent.
2. If **all** children are `[x]` — record this parent for flip.

If the flip-list is non-empty:

1. Build the writer-Agent prompt by substituting `{edit_list}` and
   `{parent_ids}` into the cascade template from
   `references/agent-prompt.md`.
2. Dispatch the writer-Agent: `Agent({description: "cascade ROADMAP
   parents", subagent_type: "general-purpose", model: "sonnet", prompt:
   <built prompt>})`.
3. Wait foreground.
4. Parse JSON return. If `status != "ok"` — halt `unknown` with
   `halt_detail="cascade failed: <detail>"` and exit.
5. Continue to Step 5.

If the flip-list is empty — skip directly to Step 5.

### Step 5 — Pick next leaf

Re-read `docs/ROADMAP-phase1.md` … `docs/ROADMAP-phase6.md` (cascade
may have just changed them). Walk top-down (phase1 first, in-file order).

For each `[ ]` line:

- If it has no `M<x>.<y>` numbered children below it within its section
  *and* it has acceptance-criteria bullets directly under it — it's a
  **leaf**. Take its M-id as the target. Stop scanning.
- If it has `M<x>.<y>` numbered children — it's a parent. Skip it; the
  scan will reach the children.
- If it has `M<x>.<y>` numbered children but they are all `[x]` — the
  cascade-pass should have flipped this parent. If it didn't, treat this
  as a corrupt state and halt `unknown` with detail.

If the scan finishes without finding a leaf:

- If there is no `[ ]` left in any phase file → halt
  `roadmap-complete` (terminal success).
- Otherwise the only remaining `[ ]` items are parents without leaf
  decomposition (no AC bullets, no numbered children) → halt
  `aggregate-needs-decomposition` with `halt_detail="<first such M-id> at <file>:<line>"`.

### Step 6 — Dispatch ship-Agent

Compute `{family}` as the leading M-family token of the leaf id (e.g.
`M7` for `M7.2.c`).

Read `references/agent-prompt.md`. Substitute `{id}` (without the leading
`M`) and `{family}`. Dispatch:

```
Agent({
  description: "ship-roadmap-item autopilot iteration: M<id>",
  subagent_type: "general-purpose",
  model: "opus",
  prompt: <substituted prompt>
})
```

Wait foreground for the JSON return.

### Step 7 — Persist + decide

1. Parse the last JSON object in the Agent's text return. On parse
   failure → set `halted=true`, `halt_reason="unknown"`,
   `halt_detail="<first 200 chars of dirty answer>"`, append a log
   line, exit.
2. Append a record to `state.history`:
   `{ts: <now ISO8601>, id, status, pr, sha}`.
3. Update `state.last_item` from the JSON return.
4. Increment `state.iterations_total`. If `status="shipped"` increment
   `state.iterations_shipped`; if `status="halted"` increment
   `state.iterations_halted`.
5. Write `.omc/state/ship-autopilot.json` back.
6. Append one line to `.omc/state/ship-autopilot.log`:
   - On ship: `<ts>  <id>  shipped   pr=<pr>  sha=<sha>  dur=<duration_sec>s`
   - On halt: `<ts>  <id>  halted    reason=<halt_reason>  detail="<halt_detail>"`
7. If `status="halted"`:
   - Set `state.halted=true`, `halt_reason`, `halt_detail`.
   - Print `🛑 ship-autopilot HALTED: <halt_reason> — <halt_detail>`.
   - Exit tick.
8. Else print `✅ M<id> shipped — PR #<pr> merged as <sha>. Tick done.`
   and exit. `/loop` will produce another tick.

## Hard rules

1. **Strict serial.** Never dispatch two ship-Agents in parallel.
   Cascade-pass agent (when it runs) finishes before ship-Agent dispatch.
2. **No code authoring in the orchestrator.** Files in `core/`, `harness/`,
   `keep/`, `tools-builtin/`, `bin/`, `scripts/` are touched only inside
   the ship-Agent. The orchestrator only edits `.omc/state/*.json|.log`.
   The cascade-pass writer-Agent is the only place where the orchestrator's
   tick edits ROADMAP files — and it does so via a delegated Agent, not
   directly.
3. **No interactive tools.** Never call `AskUserQuestion`, never invoke
   `brainstorming`/`brainstorm`/`writing-plans` skills from inside the
   tick. Any unresolvable question becomes a halt with the appropriate
   `halt_reason`.
4. **No git push --force, no --no-verify, no --no-gpg-sign** anywhere in
   the orchestrator or the dispatched agents.
5. **Honour the rdd marker.** If `.omc/state/rdd-active` exists, halt
   immediately. Two orchestrators on one repo would conflict.

## Coexistence with `rdd`

- `rdd` is operator-attended; uses many specialised agents; has 3
  interactive gates (or `--auto` rules + `/loop`).
- `ship-roadmap-autopilot` is unattended; one agent per iteration; halt
  on any blocker.

Choose the one that matches the work mode. Do not run both at the same
time on one repo (Step 2 guard catches this).
````

- [ ] **Step 2: Verify**

Run:
```bash
head -10 .claude/skills/ship-roadmap-autopilot/SKILL.md
wc -l .claude/skills/ship-roadmap-autopilot/SKILL.md
```
Expected: head shows the YAML frontmatter; line count between 150 and 220.

- [ ] **Step 3: Commit**

Run:
```bash
git add .claude/skills/ship-roadmap-autopilot/SKILL.md
git commit -m "$(cat <<'EOF'
feat(ship-autopilot): add SKILL.md orchestrator

Per-tick procedure: arg-dispatch (reset/status), pre-flight guards,
halt-check, cascade-pass, leaf picker, ship-Agent dispatch, persist.
Halt-on-blocker policy; rdd-marker mutual exclusion.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Markdownlint + skill-discoverability check

**Files:**
- Read-only check.

- [ ] **Step 1: Run markdownlint on the new files**

Run (assumes lefthook / repo's standard markdownlint config):
```bash
pnpm markdownlint .claude/skills/ship-roadmap-autopilot/**/*.md docs/superpowers/specs/2026-05-09-ship-roadmap-autopilot-design.md docs/superpowers/plans/2026-05-09-ship-roadmap-autopilot.md 2>&1 | tee /tmp/ship-autopilot-mdlint.log
```
Expected: no errors. If there are errors (typically MD024 duplicate headings, MD040 fenced-code-language), fix them inline and re-run before the next step.

- [ ] **Step 2: Confirm the skill is discoverable**

In a fresh Claude Code session in this repo, run a probe:
```
/oh-my-claudecode:ship-roadmap-autopilot status
```
Expected: prints the empty-state status table (no halts, no history).

- [ ] **Step 3: Commit only if any markdownlint fixes were needed**

If Step 1 produced edits:
```bash
git add -p .claude/skills/ship-roadmap-autopilot/
git commit -m "docs(ship-autopilot): markdownlint fixes"
```

If Step 1 was clean — no commit, proceed.

---

## Task 7: Dry-run scenarios (test plan)

These are integration scenarios for the operator to walk through before
trusting the autopilot to run unattended. Each scenario is a manual
invocation; the expected outcome is documented inline.

The point of these scenarios is to verify the orchestrator's pre-flight
guards and halt logic without actually shipping anything.

- [ ] **Scenario A: arg=`status` on empty state**

Setup:
```bash
rm -f .omc/state/ship-autopilot.json .omc/state/ship-autopilot.log
```

Invoke: `/oh-my-claudecode:ship-roadmap-autopilot status`

Expected output: empty status table or "No iterations yet". No state file should be created (status is read-only).

- [ ] **Scenario B: arg=`reset` on a fresh state**

Invoke: `/oh-my-claudecode:ship-roadmap-autopilot reset`

Expected: prints `✅ ship-autopilot halt cleared. Last item: none.` and creates `.omc/state/ship-autopilot.json` with `halted=false`, empty `history`, `last_item=null`.

- [ ] **Scenario C: pre-flight halt — dirty working tree**

Setup:
```bash
echo "scratch" > .scratch-test
```

Invoke: `/oh-my-claudecode:ship-roadmap-autopilot`

Expected: tick halts with `🛑 ship-autopilot HALTED: dirty-working-tree`.

State file: `halted=true`, `halt_reason="dirty-working-tree"`.

Cleanup:
```bash
rm .scratch-test
/oh-my-claudecode:ship-roadmap-autopilot reset
```

- [ ] **Scenario D: pre-flight halt — wrong branch**

Setup:
```bash
git checkout -b scratch-not-main
```

Invoke: `/oh-my-claudecode:ship-roadmap-autopilot`

Expected: halt with `wrong-branch`, `halt_detail="scratch-not-main"`.

Cleanup:
```bash
git checkout main && git branch -D scratch-not-main
/oh-my-claudecode:ship-roadmap-autopilot reset
```

- [ ] **Scenario E: pre-flight halt — rdd-session-active**

Setup:
```bash
mkdir -p .omc/state && touch .omc/state/rdd-active
```

Invoke: `/oh-my-claudecode:ship-roadmap-autopilot`

Expected: halt with `rdd-session-active`.

Cleanup:
```bash
rm .omc/state/rdd-active
/oh-my-claudecode:ship-roadmap-autopilot reset
```

- [ ] **Scenario F: aggregate-only halt**

Verify the picker correctly halts on an aggregate. The current ROADMAP
state is suitable: `M7.1`, `M7.2`, `M7.3` are parents; if their leaf
children get exhausted, the picker should halt rather than try to ship
the parent.

For this scenario, do NOT actually ship. Use `/oh-my-claudecode:ship-roadmap-autopilot`
in **dry-mode**: read-only inspection of the picker's choice. Since the
skill does not yet support `--dry-run`, the operator runs:

```bash
grep -nE "^- \[ \]" docs/ROADMAP-phase*.md | head -5
```

and confirms by inspection that the autopilot's first choice (highest
file, lowest line number, leaf-not-aggregate) matches the operator's
expectation. This is a sanity check, not a tool-driven test.

- [ ] **Scenario G: real one-tick ship (final readiness check)**

Once Scenarios A–F pass, the operator may run a single tick against
the real ROADMAP:

```
/oh-my-claudecode:ship-roadmap-autopilot
```

Expected: dispatches the ship-Agent for the first leaf (e.g. `M7.2.c`).
The Agent runs the full ship-roadmap-item flow. On success, the tick
prints `✅ M<id> shipped — PR #<pr> merged as <sha>.`

If anything halts, fix the underlying issue, run `reset`, and re-attempt.

After this scenario passes once, the operator may run
`/loop /oh-my-claudecode:ship-roadmap-autopilot` for unattended
operation.

(No commit for this task — it is operator validation, not a code change.)

---

## Task 8: Final spec-coverage check + PR

- [ ] **Step 1: Self-review against the spec**

Open `docs/superpowers/specs/2026-05-09-ship-roadmap-autopilot-design.md` side-by-side with the implementation. Tick off:

- [ ] §2 Architecture — covered by `SKILL.md` Per-tick procedure (Steps 0–7).
- [ ] §3.1 State JSON shape — covered by `references/state-schema.md`.
- [ ] §3.2 Halt reasons (split) — covered by `references/state-schema.md`.
- [ ] §3.3 Log file format — covered by `references/state-schema.md` and `SKILL.md` Step 7.
- [ ] §4 Auto-decision rules — covered by `references/auto-rules.md`.
- [ ] §5 Agent prompt + JSON contract — covered by `references/agent-prompt.md`.
- [ ] §6 File layout — exact paths match Task 1's mkdir.
- [ ] §6.1 Frontmatter — present in `SKILL.md`.
- [ ] §7 Operator interface — `SKILL.md` table + Step 0 arg-dispatch.
- [ ] §8 Coexistence with rdd — `SKILL.md` Hard rule 5 + Step 2 pre-flight.
- [ ] §9 Safety guards — `SKILL.md` Step 2 + Hard rules 1, 4.
- [ ] §11 Acceptance criteria — Task 7 scenarios cover items 2–10. Item 1 (skill files created) is Tasks 1–5.

If any row is unchecked, fix the gap before opening the PR.

- [ ] **Step 2: Push the feature branch**

Run:
```bash
git push -u origin feature/ship-roadmap-autopilot
```

- [ ] **Step 3: Open PR**

Run:
```bash
gh pr create --title "feat(skills): add ship-roadmap-autopilot wrapper" --body "$(cat <<'EOF'
## Summary

- New project-scoped skill `.claude/skills/ship-roadmap-autopilot/` that drives `ship-roadmap-item` end-to-end against `docs/ROADMAP-phase*.md` in a loop with no operator intervention.
- One Agent dispatch per tick (fresh subagent context per iteration); orchestrator session stays thin.
- Halt-on-blocker policy: any failure halts the loop with a closed-set reason; operator fixes, runs `reset`, continues.
- Cascade-pass: parent `[ ]` items whose children are all `[x]` get auto-flipped to `[x]` via a separate doc-only writer-Agent commit.
- Strict serial; mutually exclusive with `rdd` via the `.omc/state/rdd-active` marker.

Design: `docs/superpowers/specs/2026-05-09-ship-roadmap-autopilot-design.md`
Plan: `docs/superpowers/plans/2026-05-09-ship-roadmap-autopilot.md`

## Test plan

- [x] markdownlint clean on all new markdown.
- [x] Skill discoverable via `/oh-my-claudecode:ship-roadmap-autopilot status`.
- [x] Pre-flight halts verified: dirty tree, wrong branch, rdd-active marker.
- [ ] One-tick real ship validated (Scenario G in plan).
- [ ] Loop-mode validated for ≥2 consecutive successful iterations.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 4: Watch CI**

Run:
```bash
gh pr checks <pr-number> --watch
```

Fix any failures inline (typically markdownlint or commitlint). Re-push.

- [ ] **Step 5: Merge**

Once green and reviewed:
```bash
gh pr merge <pr-number> --squash --delete-branch
```

- [ ] **Step 6: Local cleanup**

```bash
git checkout main
git pull --ff-only origin main
git branch -D feature/ship-roadmap-autopilot
```

---

## Self-review

After writing the plan, the following sweeps were performed:

**Spec coverage:** every section of the design (§2 through §11) is mapped to a task in this plan. Task 8 Step 1 enforces this mechanically.

**Placeholder scan:** searched for TBD/TODO/FIXME — none. Each "Write the file" step contains the complete file content verbatim.

**Type consistency:** the JSON shape in `state-schema.md` (Task 2), the JSON contract in `agent-prompt.md` (Task 4), and the parse logic in `SKILL.md` Step 7 (Task 5) all use the same field names: `halted`, `halt_reason`, `halt_detail`, `last_item`, `history`, `pr`, `sha`, `id`, `status`, `review_iter1_findings`, `review_iter1_cycles`, `duration_sec`.

**Decomposition rationale:** four reference files + one orchestrator file = clear single-responsibility boundaries. Each file is small enough (<300 lines) to hold in context. Splitting `SKILL.md` further would fragment the per-tick procedure, harming readability for the operator.
