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

## 2026-04-22 — M2.1.b: keepers_log table DDL + append-only trigger

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/6
**Phases with incidents**: 6

### What worked

Phase 4 review converged on iteration 0 (3 nits, 0 blocker/important). Scope
discipline held tight: executor produced exactly 2 files (migration + test
extension), no feature creep. LESSONS.md from M2.1.a provided concrete patterns
(SQLSTATE, UUID PKs) that the executor brief could cite, reducing discovery
friction.

### What wasted effort

Phase 6 iteration 0: PR-title commitlint failure (same as M2.1.a). Title
generated as `M2.1.b: keepers_log append-only table` lacked conventional-commits
type. Fix required `gh pr edit --title "feat(migrations): add keepers_log
append-only table (M2.1.b)"` and empty commit `07bb7d5` (ci: re-trigger checks)
because the repo's `pull_request` workflow omits `edited` event type and `gh run
rerun --failed` replays the cached payload.

### Suggested skill changes

- Update `references/agent-briefs/git-master.md` §pr mode: detect commitlint
  enforcement (commitlint config present or conventional-commits history) and
  derive PR title from branch's single commit subject or a type-prefixed form
  (`feat(scope): <summary> (<roadmap-id>)`) instead of raw `<roadmap-id>:
  <title>`.
- Add Phase 5 pre-push sanity check: pipe candidate PR title through local
  `commitlint` when config exists, surfacing failure before PR open.
- Document in `references/bounded-loop.md` §"Signal source": `gh run rerun
  --failed` replays cached event payload — for title/payload failures, use
  empty-commit synchronize re-trigger instead.

### Metrics

- Review iterations: 1
- PR-fix iterations: 1
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: 01:15

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

## 2026-04-22 — M2.1.a: Core business-domain tables DDL

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/5
**Phases with incidents**: 5, 6

### What worked

Gate 2 discipline caught scope tight (six tables, no extras). Phase 3 executor
delivered all AC green in one pass. Phase 4 review loop converged in 2
iterations — fixer correctly identified the important items (nullable FK test,
DROP EXTENSION footgun) and closed both in one commit. Bounded-loop severity
contract worked: CodeRabbit's 2 Minor threads (TRUNCATE warning, SQLSTATE
locale-sensitivity) correctly classified as nit, deferred to Follow-up.

### What wasted effort

Phase 5: git-master generated PR title `M2.1.a: Keep schema foundation — …`
violated commitlint (not a conventional type). Had to retitle via `gh pr edit`
to `feat(migrations): add core business-domain tables (M2.1.a)`. Because default
`pull_request` event lacks `edited` type, close/reopen was required to
retrigger CI (not a code issue, but workflow awareness gap).

### Suggested skill changes

- Add commitlint pre-flight check to `references/agent-briefs/git-master.md` §pr
  mode: validate PR title against conventional-commits before `gh pr create`, or
  require the caller to pass a conforming title.
- Document in same brief the `gh pr close && gh pr reopen` workaround for repos
  whose `pull_request` trigger lacks `edited` type (alternatives: add `edited`
  to the workflow trigger, or use `gh run rerun --failed` + wait, though the
  latter reuses stale snapshot).

### Metrics

- Review iterations: 2
- PR-fix iterations: 0 (metadata-only fix + CI retrigger, not code)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: 04:30

---
