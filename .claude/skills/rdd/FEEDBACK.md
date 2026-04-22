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

## 2026-04-22 — M2.6: Migration tool chosen and wired

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/4
**Phases with incidents**: 4, 6

### What worked
Gate 1/2/3 discipline caught the blocker (AC1 strict — goose pin location)
and all important items (NAME injection, Prerequisites docs drift, missing
Makefile-target test coverage) in iteration 1 review. Scope held tight:
Phase 4 iteration 2 converged with zero regressions. Fixer correctly
identified the iteration-1 injection fix as incomplete and switched to the
`export` pattern.

### What wasted effort
Three silent-turn-exits after `Agent` tool calls required operator nudge
("потерял?", "забило?", "подвисло?"). Not a skill process issue; a runtime-
level one (skill dispatch missing commit 6924aee's "Agent follow-up"
hard rule, which prevents this for future runs). commitlint `#<num>` footer-parser trap cost one
commit retry. CI timeout-minutes 5 on Migrate job was tight; apt install
postgresql-client flaked once; rerun passed (not code).

### Suggested skill changes
- Add note to `references/agent-briefs/git-master.md` about commitlint
  `#<num>` footer parser trap — if referencing PR in body, use `PR 4` or
  squash-merge commit link, never `#4`.
- Expand `references/agent-briefs/code-reviewer.md` with "important vs nit"
  examples: flag "Make variable → shell context = injection risk" as blocker
  (iter-1 regex fix was incomplete); struct field ordering as nit.
- Consider explicit rule in `references/bounded-loop.md` §Severity
  (Phase 6 source column): CodeRabbit `🔴 Critical` severity markers →
  classify as blocker regardless of review state (currently defaults to
  nit because review state is COMMENTED not CHANGES_REQUESTED).

### Metrics
- Review iterations: 2
- PR-fix iterations: 2
- Operator interventions outside of gates: 3 (silent-exit nudges)
- Total wall time from /rdd to merge: 01:45

---
