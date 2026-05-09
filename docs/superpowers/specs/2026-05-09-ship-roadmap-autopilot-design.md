# ship-roadmap-autopilot — Design

**Date**: 2026-05-09
**Status**: Approved (brainstorming) → ready for writing-plans
**Owner**: Vadym Trunov

## 1. Goal

Drive `.claude/skills/ship-roadmap-item` end-to-end against `docs/ROADMAP-phase*.md`
in a loop without operator intervention. Each iteration produces a merged PR
on `main`. Stops on any blocker (halt-on-blocker policy). Each iteration runs
in a fresh subagent context so the orchestrator session does not accumulate
per-item state.

Non-goals:

- Not a replacement for the existing `rdd` skill. They coexist; see §8.
- Does not auto-decompose roadmap items. Aggregates → halt.
- Does not auto-resolve `phase2-uncertainty` (API design forks without
  prior art) — halts and surfaces the question to the operator.

## 2. Architecture

The wrapper is a project-scoped skill at
`.claude/skills/ship-roadmap-autopilot/SKILL.md`. The operator runs it as:

```
/loop /oh-my-claudecode:ship-roadmap-autopilot
```

`/loop` provides the turn boundary between iterations. The skill itself
spawns one `Agent` per tick — that subagent runs in its own fresh context
and does the heavy lifting (branch → impl → tests → parallel review iter-1
→ PR → merge → lesson finalize). The orchestrator session stays thin: it
only persists state (~200 tokens per iteration of JSON summary).

Per-tick procedure:

0. **Arg dispatch.** If invoked with `reset` → clear
   `halted/halt_reason/halt_detail`, keep `history`, exit. If invoked
   with `status` → render the last 5 entries of `history` and the
   current halt flag as markdown, exit. Otherwise fall through.
1. Load `.omc/state/ship-autopilot.json` (initialise empty if absent).
2. **Pre-flight guards** (each is a halt cause; see §9):
   - `git status --porcelain` empty.
   - Current branch is `main`.
   - `.omc/state/rdd-active` does not exist.
3. Halt-check: if `halted=true`, print reason and exit the tick.
4. **Cascade-pass**: walk the 6 ROADMAP files; for every `[ ]` parent
   whose children are all `[x]`, flip to `[x]`. If any flips, dispatch a
   small `general-purpose` writer-Agent (model=sonnet) — this is a
   *separate* Agent from the ship-Agent in step 6 — to apply the Edits
   and commit `docs(roadmap): cascade <list> after sub-items shipped` on
   `main`. Wait for return. A tick therefore performs 1–2 Agent
   dispatches: cascade (only when flips exist) + ship.
5. **Pick next leaf**: find the first `[ ]` with no `[ ]` children below
   it. If none → halt `roadmap-complete` (terminal success). If the first
   `[ ]` is an aggregate (has children but no AC bullets and projects to
   >1 PR) → halt `aggregate-needs-decomposition`.
6. **Dispatch ship-Agent**: build prompt from
   `references/agent-prompt.md` with `{id}`/`{family}` substituted. Call
   `Agent({subagent_type: "general-purpose", model: "opus", prompt})`.
   Wait foreground for the JSON return.
7. **Persist + decide**: parse JSON. Append to `history`, update
   `last_item`, append one line to `.omc/state/ship-autopilot.log`. If
   `status="halted"` → set `halted=true`/`halt_reason`/`halt_detail`,
   exit. Otherwise tick is done; `/loop` will produce another tick.

Why this pattern solves context cleanup:

- Each `Agent` dispatch creates an isolated subagent context. The full
  ship-roadmap-item flow (Phases 1–10) runs there and is discarded on
  return.
- The orchestrator only retains a small JSON summary per iteration.
- `/loop` provides a per-iteration turn boundary so silent-exit after
  Agent return does not lose progress.
- No external bash runner / `claude -p` headless is required.

Leaf vs aggregate detection:

- Leaf: a `[ ]` with no `M<x>.<y>` numbered children below it, OR a `[ ]`
  whose immediate body is a list of concrete acceptance-criteria bullets.
- Aggregate: a `[ ]` with `M<x>.<y>` numbered children below it. The
  picker dives into the children; aggregates are never targets.
- Aggregate-with-no-leaf-decomposition (a `M*` parent without `M*.<x>`
  children spelled out): halt and ask the operator to decompose.

## 3. State

### 3.1 `.omc/state/ship-autopilot.json`

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

`.omc/state/` is gitignored at the repo root, no commit needed.

### 3.2 Halt reasons (closed set)

Two origins. The Agent JSON return enum (§5) carries only the agent-side
reasons; the orchestrator may also halt before any Agent dispatch.

Agent-side (returned by ship-Agent):

| Reason | When |
|---|---|
| `aggregate-needs-decomposition` | Agent inspected M-id and saw no concrete AC bullets / scope >1000 LOC / >20 files / >1 PR |
| `phase2-uncertainty` | Agent could not pattern-match an API fork |
| `build-failed` | `go build/vet/test -race` failed after 1 fix attempt |
| `review-blocker` | codex/critic blocker not resolved after fallback |
| `ci-red` | GitHub CI failed 3 times in a row |
| `merge-failed` | `gh pr merge --squash` failed |

Orchestrator-side (set without an Agent dispatch):

| Reason | When |
|---|---|
| `roadmap-complete` | All `[ ]` exhausted (terminal success) |
| `aggregate-needs-decomposition` | Picker hit a `[ ]` parent without leaf decomposition (also reachable from the agent side; same reason, set by either) |
| `dirty-working-tree` | `git status --porcelain` non-empty at tick start |
| `wrong-branch` | Orchestrator not on `main` at tick start |
| `rdd-session-active` | `.omc/state/rdd-active` marker present |
| `unknown` | Agent returned non-JSON / crashed (orchestrator records this after parse failure) |

### 3.3 `.omc/state/ship-autopilot.log`

Append-only, one human-readable line per tick:

```
2026-05-09T22:18Z  M7.2.b  shipped   pr=120  sha=f6aa80c  dur=740s
2026-05-09T22:30Z  M7.2.c  halted    reason=phase2-uncertainty  detail="resolver vs static for SpawnClaim"
```

## 4. Auto-decision rules

Embedded into the Agent prompt (see §5):

| Decision point | Rule |
|---|---|
| **Phase 2 — API fork** | Read 2 most recent entries in `docs/lessons/<family>.md`. If they unambiguously dictate the answer (same shape, same sentinels, same canonical pattern) → take it. Otherwise return `status="halted"`, `halt_reason="phase2-uncertainty"`, `halt_detail=<the question>`. No guessing. |
| **Phase 7 — codex+critic scope** | Default `all` (Critical+Major+Minor+Nit). If 3 consecutive review cycles return the same blocker → fall back to `only Major+`, defer Minor+Nit as follow-up bullets in the lesson entry. If a blocker remains after fallback → `halt_reason="review-blocker"`. |
| **Decomposition** | Never decompose. Aggregate → halt. |
| **Aggregate detection** | If on inspection M-id has no concrete AC bullets or projects to >1000 LOC / >20 files / >1 PR → bail with `aggregate-needs-decomposition`. |
| **CI fix-loop** | Up to 3 fix attempts on red CI. Then `ci-red`. |
| **Review fix-loop** | Up to 3 cycles. Then fallback (above) or `review-blocker`. |
| **Merge** | Single attempt at `gh pr merge --squash --delete-branch`. Any failure → `merge-failed`. |
| **Concurrency** | Strict serial: one Agent at a time. The orchestrator does not dispatch the next tick until the current Agent returns its JSON. |

## 5. Agent prompt + JSON contract

Stored in `references/agent-prompt.md`. Template (orchestrator substitutes
`{id}` and `{family}`):

```
You are a single-shot autopilot worker. Repository:
/Users/user/PhpstormProjects/wathkeepers (branch: main).

Your job: ship ROADMAP item M{id} end-to-end by applying the project skill
.claude/skills/ship-roadmap-item/SKILL.md exactly. You will return a single
JSON object as your final tool result. No prose outside JSON.

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

Orchestrator parses the last JSON object in the Agent's text return. Any
parse failure → halt with `unknown`, first 200 chars of the dirty answer
in the log.

## 6. File layout

```
.claude/skills/ship-roadmap-autopilot/
├── SKILL.md              orchestrator procedure (~150 lines)
└── references/
    ├── auto-rules.md     full text of §4
    ├── agent-prompt.md   template from §5
    └── state-schema.md   JSON schema + halt reasons (§3)
```

`SKILL.md` reads `references/agent-prompt.md` once per tick, just before
the dispatch. Other references are loaded only on operator-driven
`status` / `reset` paths.

### 6.1 Frontmatter

```yaml
---
name: ship-roadmap-autopilot
description: Autopilot wrapper around ship-roadmap-item. One Agent dispatch per
  ROADMAP leaf, halt-on-blocker policy, cascade-marks parents, fresh subagent
  context per iteration. Use as `/loop /oh-my-claudecode:ship-roadmap-autopilot`.
triggers:
  - "/ship-roadmap-autopilot"
  - "ship-autopilot"
---
```

## 7. Operator interface

| Command | Behaviour |
|---|---|
| `/oh-my-claudecode:ship-roadmap-autopilot` | One tick. Manual / debug / dry-run. |
| `/loop /oh-my-claudecode:ship-roadmap-autopilot` | Self-paced loop. Recommended form. |
| `/loop 30m /oh-my-claudecode:ship-roadmap-autopilot` | Fixed-interval loop. Use only if wall-clock cap matters. |
| `/oh-my-claudecode:ship-roadmap-autopilot reset` | Clear `halted/halt_reason/halt_detail`; keep history. Use after fixing the blocker. |
| `/oh-my-claudecode:ship-roadmap-autopilot status` | Print last 5 iterations + halted flag in markdown. |

Halt is signalled to the operator three ways:

1. Tick prints a bold `🛑 ship-autopilot HALTED: <reason> — <detail>` to
   chat, visible in `/loop` notifications.
2. State file `.omc/state/ship-autopilot.json` has `halted=true`.
3. `.omc/state/ship-autopilot.log` records the halt line.

No Slack / Telegram / e-mail integration in v1. If needed later, route
through `oh-my-claudecode:configure-notifications`.

## 8. Coexistence with `rdd`

The two skills do not compete; they are different stylistics:

| | `rdd` | `ship-roadmap-autopilot` |
|---|---|---|
| Code authoring | role-specific agents (`executor`, `writer`, `git-master`, …) | one `general-purpose` Agent per iteration |
| Gates | 3 interactive (or auto-rules in `--auto`) | none; halt-on-blocker |
| PR cap | 1000 LOC / 20 files; planner decomposes | autopilot never decomposes; halt on aggregate |
| Review | `code-reviewer` agent, max 5 iter | parallel codex+critic, max 3 iter |
| Lessons | `writer` agent in Phase 5a | inside ship-roadmap-item Phase 6+10 |
| TASK file | `TASK-*.md` on branch (gitignored) | none — state in `.omc/state/` |
| Loop form | `/loop /rdd --auto resume` | `/loop /oh-my-claudecode:ship-roadmap-autopilot` |

Conflict prevention:

- Tick step 2.5 (after halt-check, before cascade-pass) checks
  `.omc/state/rdd-active`. If present → halt `rdd-session-active`.
  Two orchestrators on the same repo simultaneously would create
  conflicting branches and chaos.

When to choose which:

- `rdd` — operator-attended work; ACs need brainstorm; multi-person
  collaboration via TASK file.
- `ship-roadmap-autopilot` — overnight unattended work; ACs already
  pinned; patterns settled (e.g. saga-step family); ship-roadmap-item
  pipeline predictability matters.

## 9. Safety guards (orchestrator pre-flight)

Before any Agent dispatch in a tick:

- `git status --porcelain` must be empty → else halt `dirty-working-tree`.
- Current branch must be `main` → else halt `wrong-branch`.
- `.omc/state/rdd-active` must not exist → else halt `rdd-session-active`.
- Agent prompt forbids: `--no-verify`, `--no-gpg-sign`, `git push --force`.
- lefthook hooks (gofumpt, golangci-lint, markdownlint, commitlint,
  prettier, license-scan, gitleaks) must run on every commit Agent
  produces.

## 10. Open questions / out of scope for v1

- Notification integration (Slack/Telegram). Deferred.
- Resume mid-iteration after operator interrupt — out of scope; if
  Agent dies mid-flow the orchestrator records `unknown` and halts;
  operator cleans the half-finished branch by hand.
- Concurrent multi-repo autopilot. Out of scope.
- Cost / token budget tracking per iteration. Out of scope; can be
  layered on top of `state.history` later if needed.
- Phase 2 uncertainty escalation via codex (have codex propose a
  default and proceed). Out of scope; manual halt is safer for v1.

## 11. Acceptance criteria

- [ ] Skill files created at `.claude/skills/ship-roadmap-autopilot/`
      with frontmatter, references, and tick procedure as in §2 / §6.
- [ ] One-tick mode works: `/oh-my-claudecode:ship-roadmap-autopilot`
      ships exactly one leaf and exits cleanly.
- [ ] Loop mode works: `/loop /oh-my-claudecode:ship-roadmap-autopilot`
      ships ≥2 consecutive leaves without operator intervention.
- [ ] Halt-on-aggregate: when picker hits a `[ ]` parent without leaf
      children, autopilot halts with `aggregate-needs-decomposition`.
- [ ] Cascade-pass: after a tick that ships M7.2.c, parent M7.2 is
      flipped to `[x]` on `main` via the cascade commit before the
      next tick picks the next item.
- [ ] `reset` and `status` operator commands work.
- [ ] State file `.omc/state/ship-autopilot.json` accurately reflects
      the last shipped item, halt flag, and history.
- [ ] Pre-flight guards halt the tick on dirty tree / wrong branch /
      `rdd-active` marker.
- [ ] Phase 2 uncertainty halts (no guessing) when prior art is
      ambiguous; halt detail names the unresolved question.
- [ ] Phase 7 default `all` falls back to `only Major+` after 3
      non-converging cycles; remaining findings are listed as
      follow-ups in the lesson entry.
- [ ] Agent JSON return is parsed; non-JSON → halt `unknown` with
      first 200 chars logged.
