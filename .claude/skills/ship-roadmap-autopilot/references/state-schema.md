# State schema (`.omc/state/ship-autopilot.json`)

Single source of truth between ticks. Read at tick start, written at
tick end. `.omc/state/` is gitignored at repo root, so this file is
local to the operator's checkout.

## Shape

`version` is bumped to `2` once the `phase` field is introduced; orchestrator
upgrades legacy `version=1` files in-place by setting `phase = arg_phase`
on the first tick after the upgrade.

```json
{
  "version": 2,
  "phase": "phase3",
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
  "version": 2,
  "phase": "phase3",
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

### `phase` field

| Aspect | Value |
|---|---|
| Format | `^phase[1-9][0-9]*$` (matches the suffix of `docs/ROADMAP-phase<N>.md` and the integration branch name) |
| Set on | First tick where state is initialised — copied from the slash-command arg. |
| Mutated by | `reset phase<N>` only. A normal tick whose arg differs from `state.phase` halts `phase-mismatch`. |
| Used as | ROADMAP file selector AND git branch target for PR base, cascade push, step-10 follow-up, and HEAD pre-flight. |

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
| `phase-complete` | All `[ ]` in `docs/ROADMAP-<state.phase>.md` exhausted (terminal success for the active phase; operator retargets the next phase with `reset phase<N+1>`) |
| `aggregate-needs-decomposition` | Picker hit a `[ ]` parent without leaf decomposition (also reachable from the agent side; same reason, set by either) |
| `dirty-working-tree` | `git status --porcelain` non-empty at tick start |
| `wrong-branch` | Orchestrator not on `state.phase` branch at tick start |
| `phase-mismatch` | Arg `phase<N>` differs from persisted `state.phase`; operator must run `reset phase<N>` to retarget |
| `invalid-arg` | Slash-command arg did not parse to `phase<N>` / `reset` / `status` |
| `rdd-session-active` | `.omc/state/rdd-active` marker present |
| `unknown` | Agent returned non-JSON / crashed (orchestrator records this after parse failure) |

## Log file

`.omc/state/ship-autopilot.log` — append-only, one line per tick:

```
2026-05-09T22:18Z  M7.2.b  shipped   pr=120  sha=f6aa80c  dur=740s
2026-05-09T22:30Z  M7.2.c  halted    reason=phase2-uncertainty  detail="resolver vs static for SpawnClaim"
```
