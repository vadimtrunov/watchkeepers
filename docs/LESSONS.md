# Project Lessons — Watchkeepers

This file is an **index**. Lessons live in `docs/lessons/<milestone>.md`,
one file per milestone family of `docs/ROADMAP-*.md`.

The `rdd` skill at Phase 7a writer pass appends to the appropriate
milestone file based on the closed TASK's roadmap id. At Phase 2 it reads
**only** the milestone file relevant to the next TASK plus
`lessons/cross-cutting.md` — not this index, not all milestone files.

## Index

| File                                                   | Scope                                                                                                                                                                                            |
| ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [`lessons/M2.md`](lessons/M2.md)                       | Keep service: schema, RLS, outbox, `keepclient`, audit log, manifest fields.                                                                                                                     |
| [`lessons/M2b.md`](lessons/M2b.md)                     | Notebook library: SQLite + sqlite-vec, ArchiveStore, archive lifecycle, audit emission.                                                                                                          |
| [`lessons/M3.md`](lessons/M3.md)                       | Go core services: eventbus, lifecycle manager, cron, secrets, capability broker, outbox consumer. Includes M3.5.a security follow-ups (`organization_id` plumbing, manifest tenant column, RLS). |
| [`lessons/M4.md`](lessons/M4.md)                       | Messenger adapter + Slack: Send/Subscribe/CreateApp/InstallApp, dev-workspace bootstrap, human identity mapping.                                                                                 |
| [`lessons/M5.md`](lessons/M5.md)                       | Runtime + LLM: `AgentRuntime`, `LLMProvider`, TS harness, JSON-RPC stdio framing, tool isolation.                                                                                                |
| [`lessons/M6.md`](lessons/M6.md)                       | Watchmaster meta-agent: manifest-seed migration pattern, privilege-boundary phrase contract, authority matrix enum reuse.                                                                        |
| [`lessons/M7.md`](lessons/M7.md)                       | Spawn flow end-to-end: saga skeleton, Slack interaction, App provisioning, Notebook provision, runtime launch.                                                                                   |
| [`lessons/cross-cutting.md`](lessons/cross-cutting.md) | Multi-milestone or process lessons: verification-batch discipline, autonomous-loop behaviour, PR-evidence rules.                                                                                 |

## How to use this index

1. Take the roadmap id of the next TASK (e.g. `M5.3.b`).
2. Pick the `lessons/<family>.md` whose family prefix matches (`M5` →
   `lessons/M5.md`).
3. Skim the most recent entries (newest at the bottom) before brainstorming.
4. Also skim `lessons/cross-cutting.md` once per session.

## Append protocol

The `rdd` Phase 7a writer agent uses
`.claude/skills/rdd/references/lessons-template.md` to compose a new
section and **appends** it to the matching milestone file. New milestone
families create a new `docs/lessons/<id>.md` file and a new index row
here.

## Why a per-milestone split

The previous flat `LESSONS.md` had grown to ~115 KB / ~30 K tokens. The
`rdd` skill read it at Phase 2 of every iteration, paying that cost on
every loop even though only one milestone family was relevant. Per-file
split lets Phase 2 read 5–25 KB instead of 115 KB. See
`lessons/cross-cutting.md` for the introduction lesson.
