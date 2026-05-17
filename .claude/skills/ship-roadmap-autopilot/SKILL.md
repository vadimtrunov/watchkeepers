---
name: ship-roadmap-autopilot
description: Autopilot wrapper around ship-roadmap-item. Takes a `phase<N>`
  argument and ships ROADMAP-phase<N>.md leaves into branch `phase<N>` (not
  main). One Agent dispatch per leaf, halt-on-blocker policy, cascade-marks
  parents, fresh subagent context per iteration. Use as
  `/loop /oh-my-claudecode:ship-roadmap-autopilot phase<N>`.
triggers:
  - "/ship-roadmap-autopilot"
  - "ship-autopilot"
  - "автопилот по roadmap"
  - "autopilot roadmap"
---

# ship-roadmap-autopilot — autonomous ROADMAP shipper

Drive `ship-roadmap-item` end-to-end against a single
`docs/ROADMAP-phase<N>.md` file in a loop with no operator intervention.
Each iteration produces one merged PR on the integration branch
`phase<N>` named by the operator's argument. Halts on any blocker; the
operator fixes the blocker, runs `reset`, and continues.

The orchestrator session is thin. Each tick spawns one ship-Agent (and
optionally one writer-Agent for the cascade-pass). Both agents run in
fresh subagent contexts; the orchestrator only retains a small JSON
summary per iteration.

Reference docs (read on demand, not at tick start):

- `references/state-schema.md` — JSON shape and halt-reason enum
- `references/auto-rules.md` — phase-2 / phase-7 auto-defaults + gates
- `references/agent-prompt.md` — ship-Agent and writer-Agent prompts

## Operator interface

The required positional argument is a ROADMAP-phase token of the form
`phase<N>` (e.g. `phase3`). It selects the file `docs/ROADMAP-phase<N>.md`
as the source of leaves AND the branch `phase<N>` as the integration
target (PR base, cascade push, step-10 follow-up push, working-tree
restore — everywhere the underlying `ship-roadmap-item` skill mentions
`main`, this orchestrator substitutes `phase<N>`).

| Command | Behaviour |
|---|---|
| `/oh-my-claudecode:ship-roadmap-autopilot phase<N>` | One tick targeting phase `<N>`. Manual / debug / dry-run. |
| `/loop /oh-my-claudecode:ship-roadmap-autopilot phase<N>` | Self-paced loop on phase `<N>`. Recommended. |
| `/loop 30m /oh-my-claudecode:ship-roadmap-autopilot phase<N>` | Fixed-interval loop (rare; ticks already self-pace 5–20 min). |
| `/oh-my-claudecode:ship-roadmap-autopilot reset` | Clear `halted/halt_reason/halt_detail`; keep `phase` + `history`. After fixing a blocker. |
| `/oh-my-claudecode:ship-roadmap-autopilot reset phase<N>` | Clear halt fields AND switch the persisted `phase` to `<N>`. Use when retargeting after a phase completes. |
| `/oh-my-claudecode:ship-roadmap-autopilot status` | Print phase, last 5 iterations, halt flag in markdown. |

## Per-tick procedure

### Step 0 — Arg dispatch

Parse the slash-command arguments left-to-right. Recognised tokens:

- `reset` — clear-halt subcommand (may be followed by an optional
  `phase<N>` token to retarget).
- `status` — print-state subcommand (no further args).
- `phase<N>` matching the regex `^phase[1-9][0-9]*$` — the ROADMAP
  phase target for a normal tick.

Anything else → halt `invalid-arg` with `halt_detail="<raw arg>"`.

If invoked with `reset` (with or without a trailing `phase<N>`):

1. Read `.omc/state/ship-autopilot.json` (initialise empty if absent).
2. Set `halted=false`, `halt_reason=null`, `halt_detail=null`. Keep
   `history` and `last_item`.
3. If a `phase<N>` token followed `reset`, set `state.phase = "phase<N>"`
   (replacing any previously-persisted phase). Otherwise keep
   `state.phase` as-is.
4. Write back.
5. Print `✅ ship-autopilot halt cleared. Phase: <state.phase>. Last
   item: <last_item.id> (<last_item.status>).`
6. Exit tick.

If invoked with `status`:

1. Read `.omc/state/ship-autopilot.json` (initialise empty if absent).
2. Render `state.phase`, last 5 entries of `history`, and
   `halted/halt_reason/halt_detail` as a markdown table.
3. Exit tick.

Otherwise the arg list must contain exactly one `phase<N>` token. Save
it as `arg_phase` and fall through to Step 1.

### Step 1 — Load state

Read `.omc/state/ship-autopilot.json`. If the file is absent, create it
with the empty-state shape from `references/state-schema.md` and set
`state.phase = arg_phase`. If the file is malformed JSON, halt with
`halt_reason="unknown"`, `halt_detail="state file corrupt: <parse error>"`.

If the file exists and `state.phase` is set but differs from
`arg_phase`, halt `phase-mismatch` with
`halt_detail="state=<state.phase> arg=<arg_phase>; run 'reset <arg_phase>'
to switch"`.

If the file exists with `state.phase == null` (legacy state file from a
prior version), set `state.phase = arg_phase` and proceed.

### Step 2 — Pre-flight guards

Check, in order. First failure halts the tick (write halt fields to state, exit):

1. `git status --porcelain` must produce empty output → else halt
   `dirty-working-tree`.
2. `git rev-parse --abbrev-ref HEAD` must return `state.phase` → else
   halt `wrong-branch` with
   `halt_detail="expected=<state.phase> got=<current-branch>"`.
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

Read **only** `docs/ROADMAP-<state.phase>.md` (e.g.
`docs/ROADMAP-phase3.md`). Other phase files are out of scope for this
orchestrator and must not be read or written.

For each `[ ]` line that is a parent (matches `^- \[ \] \*\*M[0-9.]+\*\*`
and has `M<x>.<y>` numbered children below it within the same section):

1. Find all numbered children directly under this parent.
2. If **all** children are `[x]` — record this parent for flip.

If the flip-list is non-empty:

1. Build the writer-Agent prompt by substituting `{edit_list}`,
   `{parent_ids}`, and `{target_branch}` (= `state.phase`) into the
   cascade template from `references/agent-prompt.md`.
2. Dispatch the writer-Agent: `Agent({description: "cascade ROADMAP
   parents", subagent_type: "general-purpose", model: "sonnet", prompt:
   <built prompt>})`.
3. Wait foreground.
4. Parse JSON return. If `status != "ok"` — halt `unknown` with
   `halt_detail="cascade failed: <detail>"` and exit.
5. Continue to Step 5.

If the flip-list is empty — skip directly to Step 5.

### Step 5 — Pick next leaf

Re-read `docs/ROADMAP-<state.phase>.md` (cascade may have just changed
it). Walk top-down in file order.

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

- If there is no `[ ]` left in `docs/ROADMAP-<state.phase>.md` → halt
  `phase-complete` (terminal success for this phase; operator retargets
  the next phase via `reset phase<N+1>`).
- Otherwise the only remaining `[ ]` items are parents without leaf
  decomposition (no AC bullets, no numbered children) → jump to **Step 8
  (Auto-decompose)** with `aggregate_id=<first such M-id>` and
  `aggregate_detail="picker: <M-id> at docs/ROADMAP-<state.phase>.md:<line>"`.
  Do NOT halt directly — Step 8 decides whether the decompose succeeds
  (tick exits non-halting) or fails (tick halts `decompose-failed`).

### Step 6 — Dispatch ship-Agent

Compute `{family}` as the leading M-family token of the leaf id (e.g.
`M7` for `M7.2.c`).

Read `references/agent-prompt.md`. Substitute `{id}` (without the
leading `M`), `{family}`, and `{target_branch}` (= `state.phase`).
Dispatch:

```
Agent({
  description: "ship-roadmap-item autopilot iteration: M<id> → <state.phase>",
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
2. **Aggregate-needs-decomposition shortcut.** If
   `status="halted"` AND `halt_reason="aggregate-needs-decomposition"` →
   jump straight to **Step 8 (Auto-decompose)** with
   `aggregate_id=<M-id from agent JSON>` and `aggregate_detail=<halt_detail
   from agent JSON>`. Do NOT yet append history, do NOT yet increment
   counters, do NOT yet set `state.halted`. Step 8 owns persistence
   and exit for this branch.
3. Append a record to `state.history`:
   `{ts: <now ISO8601>, id, status, pr, sha}`.
4. Update `state.last_item` from the JSON return.
5. Increment `state.iterations_total`. If `status="shipped"` increment
   `state.iterations_shipped`; if `status="halted"` increment
   `state.iterations_halted`.
6. Write `.omc/state/ship-autopilot.json` back.
7. Append one line to `.omc/state/ship-autopilot.log`:
   - On ship: `<ts>  <id>  shipped   pr=<pr>  sha=<sha>  dur=<duration_sec>s`
   - On halt: `<ts>  <id>  halted    reason=<halt_reason>  detail="<halt_detail>"`
8. If `status="halted"`:
   - Set `state.halted=true`, `halt_reason`, `halt_detail`.
   - Print `🛑 ship-autopilot HALTED: <halt_reason> — <halt_detail>`.
   - Exit tick.
9. Else print `✅ M<id> shipped — PR #<pr> merged as <sha>. Tick done.`
   and exit. `/loop` will produce another tick.

### Step 8 — Auto-decompose

Entered from Step 5 (picker found only aggregate parents) or Step 7.2
(ship-Agent returned `aggregate-needs-decomposition`). Inputs:
`aggregate_id` (M-id WITH leading `M`) and `aggregate_detail` (free-text
reason).

1. Compute `{id}` (aggregate_id without the leading `M`, e.g. `1.1`),
   `{family}` (leading M-family token, e.g. `M1`), and
   `{target_branch}` (= `state.phase`).
2. Read `references/agent-prompt.md`. Substitute `{id}`, `{family}`,
   `{target_branch}`, and `{halt_detail}=<aggregate_detail>` into the
   **auto-decompose writer-Agent prompt** template.
3. Dispatch the writer-Agent:
   ```
   Agent({
     description: "decompose aggregate <aggregate_id> in ROADMAP-<state.phase>",
     subagent_type: "general-purpose",
     model: "opus",
     prompt: <substituted prompt>
   })
   ```
4. Wait foreground for the JSON return.
5. Parse JSON. On parse failure or `status="failed"`:
   - Set `state.halted=true`, `halt_reason="decompose-failed"`,
     `halt_detail="<decompose-Agent detail OR 'parse failure'>"`.
   - Increment `state.iterations_total` and `state.iterations_halted`.
   - Append history: `{ts, id: aggregate_id, status: "halted", pr: null, sha: null}`.
   - Log line: `<ts>  <aggregate_id>  halted    reason=decompose-failed  detail="<halt_detail>"`.
   - Write state.
   - Print `🛑 ship-autopilot HALTED: decompose-failed — <halt_detail>`.
   - Exit tick.
6. On `status="ok"`:
   - Append history: `{ts: <now ISO8601>, id: aggregate_id, status: "decomposed", parts: <parts>, sha: <commit_sha>, leaf_ids: <leaf_ids>}`.
   - Update `state.last_item`: `{id: aggregate_id, status: "decomposed", pr: null, sha: <commit_sha>, parts: <parts>, leaf_ids: <leaf_ids>}`.
   - Increment `state.iterations_total` and `state.iterations_decomposed`.
     Do NOT increment `iterations_halted` (decompose is a successful
     non-halting outcome).
   - Keep `state.halted=false`, `halt_reason=null`, `halt_detail=null`
     (no carry-over from Step 7.2's halted-status JSON; Step 8 success
     converts it to a decompose outcome).
   - Write `.omc/state/ship-autopilot.json` back.
   - Log line: `<ts>  <aggregate_id>  decomposed parts=<n>  sha=<sha>  leaves=<comma-sep leaf_ids>`.
   - Print `🔀 <aggregate_id> decomposed into <parts> sub-leaves (sha <sha>). Tick done — next tick picks up the new leaves.`
   - Exit tick.

## Hard rules

1. **Strict serial.** Never dispatch two ship-Agents in parallel.
   Cascade-pass agent (when it runs) finishes before ship-Agent dispatch.
2. **No code authoring in the orchestrator.** Files in `core/`, `harness/`,
   `keep/`, `tools-builtin/`, `bin/`, `scripts/` are touched only inside
   the ship-Agent. The orchestrator only edits `.omc/state/*.json|.log`.
   ROADMAP edits are delegated to writer-Agents: the cascade-pass
   writer-Agent (parent flip on all-children-`[x]`) and the auto-decompose
   writer-Agent (aggregate → letter-suffixed sub-leaves). The orchestrator
   never edits ROADMAP files directly.
3. **No interactive tools.** Never call `AskUserQuestion`, never invoke
   `brainstorming`/`brainstorm`/`writing-plans` skills from inside the
   tick. Any unresolvable question becomes a halt with the appropriate
   `halt_reason`.
4. **No git push --force, no --no-verify, no --no-gpg-sign** anywhere in
   the orchestrator or the dispatched agents.
5. **Honour the rdd marker.** If `.omc/state/rdd-active` exists, halt
   immediately. Two orchestrators on one repo would conflict.
6. **Never touch `main` or any phase file other than the active phase.**
   The integration target is `state.phase`. PR base, cascade push, and
   the step-10 follow-up commit all land on that branch. The orchestrator
   never reads/writes other `docs/ROADMAP-phase*.md` files within a tick.

## Coexistence with `rdd`

- `rdd` is operator-attended; uses many specialised agents; has 3
  interactive gates (or `--auto` rules + `/loop`).
- `ship-roadmap-autopilot` is unattended; one agent per iteration; halt
  on any blocker.

Choose the one that matches the work mode. Do not run both at the same
time on one repo (Step 2 guard catches this).
