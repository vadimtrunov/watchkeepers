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
