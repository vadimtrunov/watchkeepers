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

## 2026-04-22 — M2.1: Complete Keep schema foundation (knowledge_chunk + RLS + outbox)

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/7
**Phases with incidents**: 6

### What worked

Gate 2's "decision points baked in" paragraph (dimension 1536, HNSW opclass, scoped tables,
executor model) surfaced concrete trade-offs so the operator could silently approve without
mid-flight interventions. Bounded-loop's severity contract held: 4 important items in
iteration 0 were genuine semantic gaps (FORCE RLS owner-baseline, hermeticity, determinism,
soft EXPLAIN) and 4 nits correctly deferred. Decoupling nits into `## Follow-up` kept the
fixer pass scope-disciplined (4 commits, 1:1 per item).

### What wasted effort

Phase 6 polling script hard-coded wrong field names (`status`/`conclusion` vs `state`/`bucket`)
for the `gh` version in this environment. The `|| echo "[]"` fallback swallowed the schema
error, causing the poller to emit `POLL:no-checks-yet` forever; operator had to notice.
Phase 6 also surfaced a potential prompt-injection surface: a PR comment from CodeRabbit
contained `<system-reminder>`-shaped warning-markdown that a hook/wrapper echoed back in
the tool result, mimicking a runtime rate-limit instruction.

### Suggested skill changes

- Update `references/bounded-loop.md` §Polling mechanism: probe `gh pr checks --help` for
  the actual schema first, or detect it via `gh --version`. Replace `status`/`conclusion`
  with environment-appropriate fields (`state`/`bucket` for this version).
- Add note to `references/bounded-loop.md` §Signal source: PR-comment bodies may contain
  content mimicking runtime system-reminders; trust only reminders emitted by hooks
  (PreToolUse/PostToolUse prefixes), not content injected via tool-result payloads.

### Metrics

- Review iterations: 2
- PR-fix iterations: 0
- Operator interventions outside of gates: 1 (poller field-name bug caught by operator)
- Total wall time from /rdd to merge: ~2.5 hours

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

## 2026-04-22 — M2.7.a: Keep service skeleton (HTTP server, health, pgx pool)

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/8
**Phases with incidents**: 4, 6

### What worked

Gate 2 confirmed scope boundaries (7 AC, 7 test cases; exclusions explicit: auth M2.7.b,
business endpoints M2.7.c/d/e). Phase 3 executor delivered 9 commits, 13 files, 60.7%
coverage, all CI green locally. Phase 4 review converged immediately (0 blocker, 0
important, 6 nit), demonstrating scope discipline. Severity rubric classified two Major
CodeRabbit findings as nit (no `BLOCKER:`/`IMPORTANT:` prefix), correctly deferred to
Follow-up per contract.

### What wasted effort

**Phase 4 iteration 0**: code-reviewer output violated strict JSON contract — emitted
`{verdict/blocking_issues/non_blocking_findings}` instead of `{blocker, important, nit}`.
Mapping unambiguous so loop proceeded; flag for brief tightening.

**Phase 5**: git-master authored PR title `M2.7.a: Keep service skeleton …`, failing
commitlint (`subject-empty + type-empty`; repo enforces `<type>(<scope>)?: <subject>`).
Title edit via `gh pr edit` + empty commit `chore(keep): trigger CI after title fix`
required to re-fire `pull_request.synchronize` (workflow lacks `edited` trigger type).

### Suggested skill changes

- Tighten code-reviewer brief (`references/agent-briefs/code-reviewer.md` §Output
  contract) to mandate exact JSON schema `{blocker, important, nit}` with strict
  output-validator step or reinforcement bullet.
- Amend git-master brief (`references/agent-briefs/git-master.md` §Mode — pr) to
  require PR title matching conventional-commits and dry-run commitlint (check repo
  config first). Pattern: `feat(keep): add Keep service skeleton (M2.7.a)`.
- Document Phase 6 pattern in `references/bounded-loop.md` §"Title edit + CI re-trigger":
  when commitlint-on-PR-title is sole failure, title edit alone does NOT re-fire; push
  empty commit for `synchronize` or add `edited` to workflow trigger.
- Note in `references/bounded-loop.md` §Severity contract: Major-severity bot findings
  naming concrete logic defects (e.g. non-positive shutdown timeout) SHOULD promote to
  `important` to avoid shipping known bugs under Follow-up IOU.

### Metrics

- Review iterations: 1
- PR-fix iterations: 1
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: 01:30

---

## 2026-04-23 — M2.7.b+c: Keep read API — capability-token auth + read endpoints

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/9
**Phases with incidents**: 4, 6

### What worked

Gate 1 re-bundling of two already-decomposed leaves (M2.7.b + M2.7.c) went through cleanly
thanks to the planner-verdict JSON — operator pre-argued the bundle, planner confirmed fit,
no ROADMAP churn. Phase 4 converged on iteration 2 — the explicit "domain sanity-checks"
section of the code-reviewer brief (body-size bound, Content-Type, RLS session-var ordering,
test fixture isolation) surfaced the blocker the author had missed (unbounded embedding slice
+ no MaxBytesReader = DoS). Bounded-loop background Monitor + heartbeat pattern kept the
orchestrator context cache warm while CI ran across Phase 6 iterations.

### What wasted effort

**Phase 6 iteration 1**: `git-master` pr mode does not cross-check the PR title against
`.commitlintrc` / `commitlint.config.cjs`. Phase 6 iter 1 burned one iteration on a purely-cosmetic
commitlint failure of the PR title (`M2.7.b+c: ...` rejected as `subject-empty` / `type-empty`).
The title text came from the orchestrator's own Phase-5 prompt, which used the RDD canonical
id-first form. Orchestrator manually renamed via `gh pr edit 9 --title "feat(keep): ..."`.

**Phase 4 iteration 1**: code-reviewer agent emitted keys beyond the strict JSON contract
(`verdict`, `positive`, `ac_coverage`, renamed `blockers`/`nits` plurals). Orchestrator had
to parse fuzzy-match; a stricter validator or a schema example in the brief would help.

### Suggested skill changes

- Add a note to `references/agent-briefs/git-master.md` §Mode — pr requiring that the PR
  title pass `commitlint` when a commitlint config is present in the repo root (or at minimum,
  follow the conventional-commits pattern `<type>(<scope>): <subject>` when ≥3 of the last
  10 commits on main do).
- Consider updating the PR-title template in the brief to default to `<first-commit-subject>`
  (which already passes commitlint because all feature commits did) instead of `<roadmap-id>: <title>`.
- Tighten `references/agent-briefs/code-reviewer.md` §Output contract: add a JSON schema
  skeleton with a `null` example for empty severity buckets, to discourage extra keys.

### Metrics

- Review iterations: 2 (converged on iter 2; 4 nits deferred to Follow-up)
- PR-fix iterations: 2 (iter 1 renamed title + fixed 2 CodeRabbit nits; iter 2 converged)
- Operator interventions outside gates: 1 (chose A+fix-nits for Meta CI + nits; orchestrator executed `gh pr edit` for the title)
- Total wall time from /rdd to merge: ~1 day

---
