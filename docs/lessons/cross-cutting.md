# Project Lessons — Cross-cutting

Patterns and decisions that span multiple milestones: verification-batch
discipline, autonomous-loop behaviour, PR-evidence rules, etc.

Appended by the `rdd` skill at Phase 7 when the closed TASK is not tied to
a single milestone (or is a verification-batch / process TASK).
Read by the `rdd` skill at the start of Phase 2 alongside the relevant
milestone file.

See `docs/LESSONS.md` for the index across all milestones.

---

## 2026-05-04 — Autonomous loop: blocked bullets don't kill the loop, just the iteration

**PR**: [#40](https://github.com/vadimtrunov/watchkeepers/pull/40)
**Merged**: 2026-05-04

### Pattern

**When an autonomous ROADMAP loop hits a fundamental architectural block, the right discipline is HALT-WITH-RATIONALE — the executor stops, documents the gap, leaves the bullet `[ ]`, and reports back. The NEXT iteration's brief explicitly skips the blocked bullet and finds the next viable adjacent item.**: iteration 5 hit a real architectural block (sqlite-vec v0.1.6 brute-force at 10k×1536 makes bullet 216 unachievable on the current substrate) and halted cleanly. Iteration 6's brief explicitly skipped bullet 216 and picked the next viable adjacent item (bullet 249 capability TTL expiry, Outcome A toggle in 1 line). The loop survives architectural decisions that the operator hasn't yet made — it does NOT need to make those decisions on the operator's behalf. A halted iteration with a clear blocked-bullet rationale is not a failure; it is the correct output when the substrate can't satisfy the AC. The loop's health is preserved precisely because the executor did not overreach.

### References

- Docs: `docs/ROADMAP-phase1.md` §M2b bullet 216, §M3 bullet 249. Pattern applies to all future autonomous ROADMAP loop iterations.

---

## 2026-05-05 — rdd hot-path restructure: per-milestone lessons + Phase 7 reorder

**PR**: pending
**Merged**: pending

### Context

Night-of-2026-05-04 ran 16 PRs through `rdd`. Sessions ballooned to 5–7 MB
of jsonl per night. Two hot-path files were re-read on every iteration
(`docs/ROADMAP-phase1.md` ~25 K tokens, `docs/LESSONS.md` ~30 K tokens),
costing ~55 K tokens of preface per iteration regardless of which
milestone was active. Each TASK also generated three commits hitting
`main`: `feat(...)` (squash), then `chore(roadmap): mark X complete`,
then `docs: record lessons and feedback`. Three of that night's PRs
(#38–#40) were toggle-only — pure ROADMAP `[x]` flips with no code.

### Pattern

**Per-milestone lessons split**: Move from a single flat `docs/LESSONS.md`
to `docs/lessons/<milestone>.md` (`M2.md`, `M2b.md`, `M3.md`, `M4.md`,
`M5.md`, `cross-cutting.md`). Phase 2 now reads only the file matching
the candidate TASK plus `cross-cutting.md` — 5–25 K tokens instead of 30 K
flat. The root `docs/LESSONS.md` becomes an index of pointers, never
appended to per-TASK.

**Phase 7 reorder — writer before merge, on the feature branch**: The
prior sequence (merge → roadmap toggle on main → lessons append on main)
produced two follow-up commits on `main` per TASK. New sequence: Phase
7a writer agent commits `docs/lessons/<milestone>.md` + `FEEDBACK.md` +
ROADMAP toggle to `rdd/<slug>` (the feature branch). Phase 7b
git-master squash-merges; the writer's commit is folded in, no follow-up
on `main`. Cuts two `main` commits per TASK and removes the third PR
ceremony from the loop.

**Toggle-only PRs forbidden**: Verification-batch TASKs ride on the
next feature PR for the same milestone, or batch into one feature-shaped
PR per milestone close-out. Three consecutive toggle-only PRs in one
night was the trigger.

**Append-only writes for high-cost append-mostly files**: Lessons and
FEEDBACK files are append-mostly and grow to tens of KB. The writer
agent must NOT Read the whole file before appending; the cheap path is
`cat >> file <<'TAG' ... TAG`. This entry was written using exactly that
mechanic — explicit demonstration that the append is the writer's only
action on the file.

**PR size cap of ≤ 500 LOC / ≤ 5 files**: Pre-restructure PRs ranged 199–
2417 LOC. Large PRs induced 2–4 review iterations, each consuming
double-digit K tokens. The cap is on review surface, not novelty;
generated code counts. Gate 1 rejects oversized TASKs back to `planner`
for decomposition.

### Anti-pattern

Reading `docs/LESSONS.md` on every iteration "to seed brainstorming with
prior context". Even if the brainstorm only references one milestone's
lessons, the orchestrator paid the full 30 K token cost. Replaced by
milestone-scoped reads.

### References

- Files: `docs/LESSONS.md` (now an index), `docs/lessons/M{2,2b,3,4,5}.md`,
  `docs/lessons/cross-cutting.md`,
  `.claude/skills/rdd/SKILL.md`,
  `.claude/skills/rdd/references/lessons-template.md`,
  `.claude/skills/rdd/references/agent-briefs/writer.md`,
  `.claude/skills/rdd/references/agent-briefs/git-master.md`,
  `.claude/skills/rdd/references/roadmap-migration.md`
- Docs: `docs/LESSONS.md` (rationale paragraph at the bottom).

---
