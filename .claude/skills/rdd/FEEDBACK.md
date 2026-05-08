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

## 2026-04-23 — M2.7.d: Keep write API — store, log_append, put_manifest_version

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/10
**Phases with incidents**: 4, 5, 6

### What worked

The `handlers_read.go` → `handlers_write.go` mirror pattern (same shape for request parsing,
error handling, JSON output) made Phase 3 deliver 4 commits with 8 files cleanly. Phase 4
iteration 0 flagged only 1 `important` — a test-layer gap (unit happy-tests short-circuiting
on sentinel error). Phase 4 iteration 1 fixed it with a tiny `fakeTx`/`fakeRow` seam; converged
thereafter. Reusing `db.WithScope` for writes (RLS `WITH CHECK` on `knowledge_chunk` already
handled) avoided a second tx helper. The `actorFromScope` helper kept scope binding in one
place, which paid off when CodeRabbit audited for vector laundering (M2.7.d context: all
client UUID and vector inputs got pre-validated before DB cast).

### What wasted effort

**(1) PR-title commitlint failure redux** (Phase 5): git-master produced `M2.7.d: Keep write API — …`
rejected by Meta CI. The brief says "conventional-commits if ≥3 of last 10 commits" but doesn't
mandate the PR title follow the same. TASK title shape inherited the roadmap-id-first form.
Orchestrator manually fixed via `gh pr edit`. **(2) Phase 4 nit vs blocker classification**:
`correlation_id` UUID prevalidation was labeled `nit` and deferred. Phase 6 CodeRabbit surfaced
it as `Major` — a defect that returns 500 for a client-shape error is not a nit. **(3) Phase 6
severity rule leniency**: The bounded-loop rule ("body begins with `BLOCKER:`/`IMPORTANT:`")
gives CodeRabbit LLM findings default `nit` level even when CodeRabbit itself badges them
`🔴 Critical` (goroutine fatalf). Orchestrator upgraded by hand; the rule as written would have
let a real bug merge silently.

### Suggested skill changes

- `references/agent-briefs/git-master.md` §Mode — pr: add rule that PR title must also pass
  `commitlint` when repo uses it (or follow conventional-commits pattern when ≥3 of last 10
  commits on main do). Suggest deriving title from first feature commit subject.
- `references/bounded-loop.md` §Severity (Phase 6): when a third-party automated reviewer
  (CodeRabbit, Sourcery) attaches severity badge (`🔴 Critical`, `🟠 Major`), upgrade from
  `nit` to `important` / `blocker` respectively, even if body lacks the prefix.
- `references/agent-briefs/code-reviewer.md` §Severity: add caveat that a defect fix is
  cosmetic but impact is 500 for client-valid shape = `important`, not `nit`. Follow-up items
  must stay strictly stylistic.

### Metrics

- Review iterations: 2 (iter 0: 1 important + 5 nits; iter 1: converged)
- PR-fix iterations: 2 (iter 0: title + 3 CodeRabbit threads; iter 1: converged)
- Operator interventions outside gates: 0 (orchestrator handled title fix autonomously)
- Total wall time from /rdd to merge: ~1 hour

---

## 2026-04-24 — M2.7.e.a: Keep subscribe endpoint + in-process publish Registry

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/11
**Phases with incidents**: 4, 6

### What worked

Gate 1 decomposition (M2.7.e → {M2.7.e.a, M2.7.e.b}) proved correct: planner fit check passed, operator approved, and the bundle avoided conflating the SSE transport seam with outbox-worker semantics. Phase 3 executor (opus, build) delivered all 9 AC green in 6 commits with 88.9% coverage on publish and 80.1% on server. Phase 4 iteration 1 review surfaced 3 genuine important items (watchdog-goroutine leak, missing malformed-token integration test, overly-broad EOF assertion); fixer resolved all three in 2 commits with no scope creep. Iteration 2 converged cleanly.

### What wasted effort

**Git-master PR-title commitlint failure (Phase 5)**: PR title `M2.7.e.a: add subscribe endpoint with publish Registry` failed Meta CI (`type-empty`, `subject-empty`). The git-master brief's formula `<roadmap-id>: <title>` is not commitlint-aware. Orchestrator renamed to `feat(keep): add subscribe endpoint + in-process publish Registry` via `gh pr edit`, then `gh pr close` + `gh pr reopen` to retrigger (repo's `ci.yml` `pull_request` trigger lacks `edited` type). One full Phase 6 iteration spent on title-only bookkeeping.

**Code-reviewer iter 1 suggested_fix for malformed token lacked production context**: Review thread suggested `reason=invalid_token` as the sentinel, but production middleware emits `bad_token` (per `core/internal/keep/server/middleware.go`). Fixer caught it and asserted the production value, but reviewer citing an unreferenced string cost one round-trip of operator attention.

### Suggested skill changes

- `references/agent-briefs/git-master.md` §Mode — pr: when `commitlint.config.*` exists at repo root, derive PR title as `<type>(<scope>): <imperative subject>` from the TASK's recent commit style, and place the `<roadmap-id>` in the PR body instead of the title.
- `references/bounded-loop.md` §Phase-6 polling / Signal source: add note that `pull_request` workflows using default trigger types (opened|synchronize|reopened) do NOT re-run on `edited`, so title-only fixes must use `gh pr close && gh pr reopen` to retrigger CI (or repo must add `edited` to trigger types).
- `references/agent-briefs/code-reviewer.md` §Output contract: require `suggested_fix` to cite the exact production symbol/constant name when naming a wire-level string (reason code, status text, header name); prevents reviewer inventing values the fixer must correct.

### Metrics

- Review iterations: 2
- PR-fix iterations: 2
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~08:00

---

## 2026-04-27 — M2.7.e.b: outbox publisher worker consuming outbox table into subscribe publish API

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/12
**Phases with incidents**: 5, 6

### What worked

Bounded loop's severity contract held tight: both unresolved CodeRabbit comments (nit #1 docstring, nit #2 AC4 shutdown path) were correctly classified as `nit` per contract. Operator-decided exception (merging despite AC4 nit #2 touching AC directly) was a clean recovery path documented in Follow-up. Executor (opus) at Phase 6 iter 1 quickly diagnosed both integration-test failures as test-side bugs (read-after-commit race + substring match), using local pgvector to repro with `KEEP_INTEGRATION_DB_URL` pointed at port 55432. CI converged green on second push.

### What wasted effort

**Preflight Check 4 (clean working tree) failed on untracked build artifact**: An ~15MB Mach-O `keep` binary in the repo root was untracked and not in `.gitignore`. Cost a clarification round before Phase 1 could even begin. Worth considering: should preflight surface untracked-but-gitignorable build artifacts with an explicit "add to .gitignore?" suggestion rather than blocking with the generic dirty-tree message?

**Phase 5 PR title still using `<roadmap-id>:` form despite prior feedback**: Title `M2.7.e.b: add outbox publisher worker` failed commitlint (`type-empty`/`subject-empty`). The git-master brief suggestion from M2.7.e.a has not yet been promoted into `references/agent-briefs/git-master.md`, so the pattern repeated. Orchestrator fixed via `gh pr edit` to `feat(keep): add outbox publisher worker (M2.7.e.b)`. Third occurrence of this anti-pattern indicates the brief urgently needs update.

**Phase 7 pre-step: local main diverged from origin/main before merge**: Commit `21483fa` (from M2.7.e.a's Phase 7 lessons) was on origin but not local. Squash-merge would have absorbed it; `git pull --ff-only` would have diverged. Orchestrator pushed local main first, then merged. The `git-master merge` brief currently doesn't account for this — worth adding a check: "before `gh pr merge`, ensure local `main` matches `origin/main`; if not, fast-forward push first or escalate."

### Suggested skill changes

- Update `references/agent-briefs/git-master.md` Phase 5 §"Open the PR" to require a conventional-commits-conformant title (provide the `<type>(<scope>): <subject> (<roadmap-id>)` pattern explicitly, citing a recent commit on the branch as ground truth).
- Update `references/agent-briefs/git-master.md` Phase 7 to add pre-merge check: "before `gh pr merge`, run `git fetch origin && git log -1 --oneline main ^origin/main`; if non-empty, push local main or escalate" to prevent squash-absorbing foreign commits or diverging on merge-base.
- Consider adding Preflight Check 4.5: list untracked-but-gitignorable build artifacts (`*.o`, `keep`, `main`, common binaries) and offer to add to `.gitignore` rather than blocking with the generic dirty-tree message.

### Metrics

- Review iterations: unknown for this TASK (Phase 4 occurred in prior session)
- PR-fix iterations: 2 (iter 1: test bugs; iter 2: AC4 nit)
- Operator interventions outside of gates: 1 (preflight Check 4 clarification)
- Total wall time from /rdd to merge: ~2 hours

---

## 2026-05-02 — M2.8.a: keepclient package skeleton

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/13
**Phases with incidents**: 7

### What worked

Phase 4 converged on iteration 1 (0 blocker, 0 important, 3 nits) — scope discipline held tight, executor delivered all 8 ACs + 10 test-plan cases in one pass with 87.6% coverage. Phase 6 converged on iteration 1 (all 9 CI checks green on first push, CodeRabbit posted 2 nits matched severity contract). Reuse of the commitlint LESSON from M2.7.e.b (applying conventional-commits title pattern upfront) eliminated Phase 5 friction entirely — PR title `feat(keep): add keepclient package skeleton (M2.8.a)` passed Meta CI on first attempt.

### What wasted effort

**Phase 7 operator intervention outside gates**: Initial merge attempt via `gh pr merge 13 --squash --delete-branch` failed because the active `gh` account (`vadym-trunov_wbt`, EMU) lacked write access to the repository. Operator had to diagnose, switch to the correct account (`vadimtrunov`), and retry. The Preflight checks do not surface the active vs. required GitHub account distinction when an organization uses EMU, leaving the mismatch invisible until merge time.

### Suggested skill changes

- Update Preflight Check 3 (`gh` CLI readiness) to explicitly verify that the active `gh auth` account (`gh auth status --hostname github.com | grep "Logged in"`) matches the repository's owner or org (inferred from `gh repo view --json owner`). If mismatch detected and the repo is under an organization using EMU, emit a loud warning: "Active account `<active>` is not a member of `<org>`; if write-blocked at merge, use `gh auth switch` to select the correct account."

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 1 (EMU account mismatch at Phase 7)
- Total wall time from /rdd to merge: ~5 days (2026-04-27 → 2026-05-02; mostly elapsed clock time, not active work)

---

## 2026-05-02 — M2.8.b: keepclient read endpoints (Search, GetManifest, LogTail)

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/14
**Phases with incidents**: 6

### What worked

Planner's "fits one PR" heuristic was correct — the three methods shared the M2.8.a transport plumbing tightly, making one cohesive unit. Phase 3 executor (opus, build) delivered all 7 ACs green in 4 commits (~1290 lines) with strong test coverage (115s keep-integration-test, all checks passing). Phase 4 iteration 1 surfaced 4 important items (all AC5 per-method sentinel gaps in GetManifest/LogTail); fixer resolved all in one commit. Iteration 2 converged cleanly (0 blocker/important). Phase 5 & 6 benefited from M2.8.a precedent: PR title `feat(keep): add keepclient read endpoints (M2.8.b)` passed commitlint on first attempt (conventional-commits pattern established in M2.8.a FEEDBACK).

### What wasted effort

**PR-title commitlint retrigger issue (Phase 6 iteration 1)**: PR title `M2.8.b: keepclient read endpoints` was initially non-conventional, flagged by Meta CI. Operator renamed via `gh pr edit` to conventional form. However, `gh run rerun --failed` replayed the cached event payload (still old title), requiring `gh pr close && gh pr reopen` to fire a fresh `pull_request: reopened` event with the new title. This is not a skill process failure (M2.8.a FEEDBACK had documented the fix) but a gotcha that recurred because the bounded-loop polling does not surface event-payload staleness as a distinct signal — it only sees "CI still failing" and advises retry without distinguishing "title still old" from "code still broken".

**Preflight Check 3 EMU auth caveat (Phase 7)**: Same as M2.8.a — the active `gh` account at merge time was EMU and lacked write scope, requiring operator account-switch. The M2.8.a FEEDBACK suggestion to tighten Preflight Check 3 has not yet been implemented, so the same friction recurred.

### Suggested skill changes

- In `references/bounded-loop.md` §"Phase 6 polling", add a clause: "when a commitlint-title failure is the sole remaining blocker, validate that the title edit persisted via `gh pr view` before retrying CI. If `gh pr view --json title` still shows the old form, use `gh pr close && gh pr reopen` instead of `gh run rerun`."
- Promote the Preflight Check 3 tightening from M2.8.a FEEDBACK into the actual `references/preflight.md` Check 3 implementation — explicitly verify the active `gh auth` account and warn on EMU mismatch.

### Metrics

- Review iterations: 2
- PR-fix iterations: 2
- Operator interventions outside of gates: 2 (PR title rename confirmation; auth-account switch)
- Total wall time from /rdd to merge: approx 5–6 hours

---

## 2026-05-03 — M2.8.c: keepclient write endpoints (Store, LogAppend, PutManifestVersion)

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/15
**Phases with incidents**: 7 (git-master merge mode)

### What worked

Auto-approved gates (operator's `/loop /rdd` variant-c blanket-yes) made the loop continuous; no checkpoint stalls. Phase 4 converged at iteration 0 (zero blocker/important) — TDD discipline + per-method status matrices from M2.8.b template carried over cleanly. Phase 6 polling Monitor with `bucket`+`state` allowlist worked exactly as written; CHECKS_COMPLETE arrived on schedule.

### What wasted effort

**Phase 7 git-master `merge`-mode agent truncation**: The `oh-my-claudecode:git-master` agent in `merge` mode produced a truncated report saying only "Found the line. Flipping M2.8.c only…" The actual steps (ROADMAP flip → commit → push) were NOT executed by the agent. Orchestrator had to perform the toggle, commit, and push directly (acceptable per SKILL.md Hard rule 1, but the agent should have completed step 4–5 of its brief). The agent reported success incompletely rather than surfacing the truncation as a failure — subtle but costly for trust.

### Suggested skill changes

- In `references/agent-briefs/git-master.md` mode=merge, add a "Verification before report" sub-step: require the agent to run `git log --oneline -1 origin/main` and `grep "M#.k.* \[x\]"` to prove its ROADMAP commit landed before claiming success. This ensures the agent surfaces a failure if the push didn't fire.

### Metrics

- Review iterations: 1 (converged)
- PR-fix iterations: 0 (no fixer dispatched)
- Operator interventions outside of gates: 1 (orchestrator self-corrected missing ROADMAP commit/push)
- Total wall time from /rdd to merge: ~25 min

---

## 2026-05-03 — M2.8.d.a: keepclient Subscribe SSE consumption with typed Event model and httptest contract tests

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/16
**Phases with incidents**: 1 (planner decomposition)

### What worked

Planner correctly flagged M2.8.d as "too large" — three distinct concerns (streaming primitive, reconnect policy, dedup hooks) each warrant their own review iteration. Decomposition into M2.8.d.a + M2.8.d.b made each PR fit cleanly. `references/roadmap-migration.md` §"Decomposition at Gate 1" worked as written: nest as sub-bullets under the original `M2.8.d`, commit `docs(roadmap): decompose M2.8.d into sub-items`, take `M2.8.d.a` as the unit of work. Phase 4 converged at iter 0 again — second consecutive M2.8.* TASK with no blockers/important. Phase 6 polling Monitor with `bucket`+`state` allowlist landed CHECKS_COMPLETE on schedule for the second consecutive run.

### What wasted effort

Bypassed `git-master` agent for Phase 7 merge entirely after the M2.8.c incident (truncated report, missed ROADMAP flip). Did `gh pr merge` + ROADMAP edit + commit + push directly from the orchestrator. Worked, but means we can't yet validate whether the proposed `git-master` brief verification step would catch the failure mode.

### Suggested skill changes

In `references/agent-briefs/git-master.md` mode=merge, add a **mandatory verification block** at the end: agent must run `git log --oneline -1` (assert top commit is `chore(roadmap): mark <id> complete`), `git rev-parse origin/main` (assert pushed), and `grep "<leaf-id>" docs/ROADMAP-*.md` (assert `[x]` line present). Without this, "Found the line. Flipping..." trivially looks like progress without actual completion.

Consider documenting the "decomposition at Gate 1" path more prominently in `SKILL.md` Phase 1 — the operator should see it without having to chase `references/roadmap-migration.md`.

### Metrics

- Review iterations: 1 (converged)
- PR-fix iterations: 0
- Operator interventions outside of gates: 1 (orchestrator did Phase 7 merge inline due to prior agent flakiness)
- Total wall time from /rdd to merge: ~25 min

---

## 2026-05-03 — M2.8.d.b: Subscribe reconnect policy + Last-Event-ID + dedup hooks + integration smoke

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/17
**Phases with incidents**: 4 (1 important fixer iter), 6 (CI fail + pr-fixer iter)

### What worked

Sleeper-injection seam in the unit tests caught the backoff math without flakes. Bounded-loop.md Phase 6 polling Monitor + GitHub-side severity classifier kept the loop deterministic. Reviewer at iter 1 caught the empty-`id:` clobber that CodeRabbit + executor both missed. Frame-count fixer pattern (Fix A in pr-fixer brief) was a clean drop-in for the bytes-threshold flake.

### What wasted effort

The smoke test's outbox-row leak ONLY surfaced because `TestOutbox_OrgScopeDelivered` decoded our payload — not because we wrote a leak detection. Test isolation should be enforced by the harness, not discovered by accident. Suggest a `t.Cleanup` audit helper or a defer that DELETEs all outbox rows touched in the test (track via a `chan string`/slice).

CodeRabbit's 🟠 Major comment (replacement stream inherits per-call `Next` ctx) is a real semantic bug. The `bounded-loop.md` classifier downgrades all bot comments to `nit`, so it didn't block merge. Worth a SKILL discussion: is "real defect flagged by a bot" still nit?

### Suggested skill changes

- `references/agent-briefs/code-reviewer.md`: add "verify resource-cleanup discipline in test files" — specifically, any test that inserts to a shared schema must register a `t.Cleanup` deletion. Would have caught the outbox leak in iter 1.
- `references/bounded-loop.md` §"Phase 6 severity recognition": consider promoting CodeRabbit comments tagged `🟠 Major` or `⚠️ Potential issue` to `important` rather than blanket-classifying as `nit`. Trade-off: more iterations vs. shipping known bugs.

### Metrics

- Review iterations: 2 (Phase 4: 1 important + 1 fixer + 1 converge iter)
- PR-fix iterations: 1 (Phase 6: 1 CI fail + 1 fixer + 1 green re-poll)
- Operator interventions outside of gates: 1 (orchestrator did Phase 7 merge inline)
- Total wall time from /rdd to merge: ~50 min

---

## 2026-05-03 — M2b.1: Notebook SQLite + sqlite-vec storage substrate

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/19
**Phases with incidents**: 4 (1 blocker fixer iter)

### What worked

Gate 2's "driver decision matrix" section laid out all three options (mattn CGo, ncruces+wazero, modernc pure-Go) with explicit reject criteria. Executor used Option B exactly per rubric, hit a WASM-incompatibility wall, and fell back to Option A with documentation. No design churn or back-and-forth. Fixer for the FK blocker landed in one commit — DSN flag + PRAGMA readback + two tests + sync-contract docs all addressed cleanly. Reviewer correctly classified mattn's silent-DSN-flag-drop as a blocker (not nit) because AC4 required FK enforcement; this is exactly the bounded-loop severity judgment that the definitions exist for.

### What wasted effort

The first executor session had a truncation incident in the prior M2.9.a run; this M2b.1 session did NOT repeat it because the brief explicitly cited the FEEDBACK truncation guard and required the structured report block before any "Stop". Pattern works — keep enforcing. Pre-push license-scan emitted 2 warnings for CGo modules (`sqlite-vec-go-bindings/cgo`, `mattn/go-sqlite3`) lacking machine-readable license files. Did NOT block the push (warnings, not errors). Worth confirming upstream licenses (both MIT/BSD-equivalent in READMEs) and either suppressing the warning or adding manual license metadata to lefthook config.

### Suggested skill changes

- In `references/agent-briefs/executor.md`, formalize the "encode driver/integration matrix in TASK" pattern: when a new dependency has multiple Go integrations, the TASK MUST list them with explicit reject criteria so the executor can pick evidence-driven, not preference-driven.
- In `references/agent-briefs/code-reviewer.md`, add a checklist item for "SQLite + foreign-key enforcement": any new SQLite connection in the diff must set `_foreign_keys=on` AND have a PRAGMA readback. This blocker would have been caught by a checklist; reviewer found it via judgment, which is fine but slower to scale.
- In `SKILL.md` Phase 7, mention that the orchestrator should `TaskStop` any orphan Monitor before exiting — this run had a stale poller still emitting POLL events after CHECKS_COMPLETE because the Monitor script raced its own exit.

### Metrics

- Review iterations: 2 (iter 1: 1 blocker + 1 important; iter 2: converged)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-03 — M2.9.a: Manifest personality/language constraints, validation, and docs

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/18
**Phases with incidents**: 3 (executor truncation)

### What worked

Reviewer caught zero issues on first iteration — a sign that the TASK brief was specific enough (chose regex, picked length cap, named the reason codes, cited the M2.8.d.b cleanup pattern) that the executor had no design ambiguity to fumble. Cascade rule from `roadmap-migration.md` §"Cascade at Phase 7" worked exactly as written: M2.9.a → M2.9 → M2 traversal flipped three checkboxes in one commit, with the orchestrator stopping at M2 because that's the deepest ancestor with all-`[x]` children.

### What wasted effort

The first executor session committed only the migration and returned a truncated report mid-implementation: `"Now add the language pattern constant + the validation logic:"` — that's the silent-exit anti-pattern flagged in FEEDBACK 2026-04-22 manifesting in an executor agent rather than the orchestrator. The orchestrator detected the truncation by reading `git log --oneline main..HEAD` and `git diff --stat`, then dispatched a continuation executor with a focused brief. Cost: ~5 minutes of orchestrator self-correction. Could have been worse if the orchestrator had trusted the partial report.

### Suggested skill changes

- In `references/agent-briefs/executor.md`, add a "Truncation guard" section at the bottom: the executor MUST print its full structured report (`COMMITS: ... TEST CMD: ... TEST EXIT CODE: ...`) BEFORE its `Stop` event. If the model is about to hit a token budget and can only print partial content, it should print `INCOMPLETE: <what is left>` so the orchestrator knows to dispatch a continuation. The current brief documents the report format but does not flag truncation as a failure mode worth its own checklist item.
- Add a "trust-but-verify" line in `SKILL.md` Phase 3: after the executor returns, the orchestrator should ALWAYS run `git log --oneline main..HEAD` and `git diff --stat main...HEAD` before declaring Phase 3 complete, even when the report claims success. This caught M2.9.a truncation but is currently informal.

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 1 (orchestrator detected executor truncation and dispatched continuation)
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-03 — M2b.2.a: Notebook in-process CRUD (Remember/Recall/Forget/Stats)

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/20
**Phases with incidents**: 1 (git auth halt mid-decomposition)

### What worked

Reviewer caught zero blockers and zero importants on the first iteration — the TASK brief was specific enough (named the indexes, documented sqlite-vec MATCH+k pattern, listed the sentinels, cited M2b.1 sync-contract docs) that the executor had no design ambiguity. Executor truncation guard (added after M2.9.a incident) worked: structured report delivered before Stop, no continuation needed. Decomposition rule from `references/roadmap-migration.md` §"Decomposition at Gate 1" applied cleanly: nested as sub-bullets under M2b.2, commit `docs(roadmap): decompose M2b.2 into sub-items`, took M2b.2.a.

### What wasted effort

**Phase 1 git auth halt mid-decomposition**: The `gh` CLI active account had silently switched from `vadimtrunov` (admin) to `vadym-trunov_wbt` (no write access) between prior iterations. The decomposition push hit a 403. Preflight Check 3 caught it on FIRST run earlier in the session, but the `/rdd` loop does not re-run preflight Check 3 between iterations. Without operator intervention, a `/loop` variant would have stalled silently on every push thereafter. Cost: ~30 seconds of operator attention for account switch.

### Suggested skill changes

- `references/preflight.md` Check 3 should be re-run on EVERY `/rdd` invocation, not just the first of the session. Auth state can change between loops (account switch, token expiry, MFA reauth). Currently preflight runs at Phase 0 of each `/rdd`, but in /loop variant-c the operator may not see the failure until a push hits 403. Consider adding a self-healing step: if `gh auth status` shows the configured-admin account as "inactive", `gh auth switch --user <admin>` automatically and continue.
- The git-master pr/merge agents could `gh auth status` before push as a guard, halting with a clear message rather than failing with 403 inside lefthook output. Currently the failure mode is the lefthook output dump (which we have to grep through).

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 1 (gh auth switch)
- Total wall time from /rdd to merge: ~25 min

---

## 2026-05-03 — M2b.2.b: Notebook Archive + Import snapshot lifecycle

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/21
**Phases with incidents**: 4 (1 important fixer iter)

### What worked

Reviewer caught the embedding-bytes coverage gap that the executor's "happy path looks fine" test would have masked. The byte-equality check was explicit in the AC ("match by ID/category/content/embedding-bytes") but easy to miss when implementing — pattern: when an AC names specific match criteria, the reviewer must spot-check that the test asserts each one explicitly, not just "Recall returns the row". Truncation guard from FEEDBACK 2026-05-03 still holding — both executor sessions on this branch printed structured reports before exit.

### What wasted effort

Iter-1 reviewer flagged 4 nits (`TestImport_AtomicRename` name oversells, `&immutable=1` DSN suggestion, comment duplication, dead `IsAbs` check) — none of these are real bugs, all deferred to Follow-up. The reviewer brief asked for "real defects only — name nits sparingly", and yet 4 nits landed. Not a process failure (nits are correctly classified), but worth noting that the bar for "report a nit at all" is lower than ideal — each nit is a small distraction the operator must decide to defer.

### Suggested skill changes

- `references/agent-briefs/code-reviewer.md` — clarify that nit count >= 3 on a converged-iter-1 review usually indicates the reviewer is reaching for content; consider raising the bar to "report only nits that change a future maintainer's behaviour, not pure preference". OR introduce a per-iteration nit budget.
- `references/agent-briefs/code-reviewer.md` — add a checklist item: "if an AC names specific match criteria (e.g. 'match by X/Y/Z'), the reviewer must enumerate each criterion and verify the test asserts it explicitly". This would have caught the embedding-bytes gap on iter 1 even faster (it did, but explicit checklist accelerates).

### Metrics

- Review iterations: 2 (iter 1: 0/1/4; iter 2: converged)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~50 min

---

## 2026-05-03 — M2b.3.a: ArchiveStore interface + LocalFS implementation

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/22
**Phases with incidents**: 4 (1 important fixer iter)

### What worked

Truncation guard from prior FEEDBACK entries still holding — executor session printed full structured report before exit. Decomposition rule from M2b.2 proved correct: M2b.3 split into M2b.3.a (interface + LocalFS) and M2b.3.b (S3Compatible), kept each PR compact. Reviewer iteration 1 converged to 0/1/2 severity split — the `important` was genuine (embedding-byte round-trip test must open the imported SQLite file directly, not mock). Fixer resolved it cleanly: added `assertEmbeddingBytesRoundTrip` helper that registers `sqlitevec.Auto()` in `init()`, opens the notebook-owned file directly via `sql.Open("sqlite3", "file:..?mode=ro&_foreign_keys=on")`, SELECTs the embedding, and `bytes.Equal`-s it. Pattern reusable for any external integration test touching vec0.

### What wasted effort

The fixer for embedding-byte assertion required cross-package SQLite access (archivestore test opening a notebook-owned file). The setup ceremony — `init()` registering `sqlitevec.Auto()`, closing the notebook handle before opening a raw `*sql.DB`, gocyclo extraction of `assertEmbeddingBytesRoundTrip` to satisfy lint — was ~70 LOC of test boilerplate. Worth the coverage but a reminder that "test embedding bytes from outside the package" is a recurring tax on any external integration test.

### Suggested skill changes

- Consider whether `notebook` should expose a small test-helper API for "open the imported file in a way external tests can verify". E.g. `func (d *DB) DBPath() string` plus a doc note on how to register vec extension. This would let archivestore (and M2b.3.b) avoid duplicating the `init()` + raw-`sql.Open` boilerplate.
- The third-consecutive-decomposition trend (M2.8.d, M2b.2, M2b.3 all split) suggests milestones in ROADMAP-phase1.md are bundled too tightly. Consider a SKILL note: "if planner decomposes 3 in a row, lower the heuristic threshold from ~1 day to ~half day for the rest of the milestone".

### Metrics

- Review iterations: 2 (iter 1: 0/1/2; iter 2: converged)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-03 — M2b.3.b: S3Compatible ArchiveStore via minio-go + testcontainers-go

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/23
**Phases with incidents**: none (clean run)

### What worked

Reviewer iteration 1 converged immediately with 0 blocker, 0 important, 5 nits — the TASK brief named the patterns (Stat-then-Read for NoSuchKey, singleton container with sync.Once, per-test bucket isolation) so the executor had no guesswork. The "extract helpers on second-backend's PR" timing was right: M2b.3.a shipping LocalFS-private helpers minimised that PR's surface; M2b.3.b's refactor commit (`87e798f`) was small and reviewable on its own. Both PRs stayed under the per-PR budget. Truncation guard from FEEDBACK 2026-05-03 still holding — executor printed structured report cleanly.

### What wasted effort

Reviewer flagged 5 nits — same "low bar for nits" pattern observed on M2b.2.b (4 nits) and M2b.3.a (2 nits). Three consecutive iterations with 2–5 nits each suggests reviewers reach for content when convergence is otherwise easy. Worth promoting the FEEDBACK suggestion from M2b.2.b: tighten the bar to "nits that change a future maintainer's behaviour, not pure preference".

### Suggested skill changes

- `references/agent-briefs/code-reviewer.md`: add explicit guidance "if iter-1 has 0 blocker AND 0 important, prefer 0–2 nits over 4+. Doc-comment-style suggestions belong in a separate doc-pass, not the review JSON."
- M2b.3 milestone is the third consecutive case where the planner correctly decomposed a "feature + N implementations" sub-item. Pattern is well-established. Consider promoting it to a SKILL note or `references/decomposition.md`.

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-03 — M2b.4: Notebook ArchiveOnRetire shutdown helper

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/24
**Phases with incidents**: 4 (1 important fixer iter)

### What worked

Reviewer caught a real goroutine leak that would have shipped silently. The leak doesn't trigger in tests (fakeStore drains via `io.Copy`) and only surfaces in production with a real S3/LocalFS that fails before reading. Reviewer-as-design-pressure pattern paid off. The reviewer ALSO caught the test masking the leak (Issue #2) — catching both the production defect and the test fragility together is the gold standard. Converter truncation guard from prior FEEDBACK entries still holding — executor delivered full structured report before exit.

### What wasted effort

The TS-vs-Go harness structural ambiguity was caught at planner time, but only as a "decompose to library helper, defer harness wiring" workaround. The underlying question — should we build a Go harness or shell out from TypeScript? — is still unanswered. Future M2b.4-successor TASKs will hit this again. No process waste, but a reminder that deferring structural decisions at Gate 1 postpones rather than resolves the ambiguity.

### Suggested skill changes

- Promote a generic LESSON pattern to `SKILL.md`: "when a planner decomposes around a structural ambiguity (e.g. language gap, layering issue), open a follow-up TASK in the BACKLOG that names the ambiguity explicitly so it doesn't get forgotten when the deferred work re-surfaces."
- Add to `references/agent-briefs/code-reviewer.md` an explicit `io.Pipe` checklist item: "any code using `io.Pipe` must verify that BOTH sides can unblock each other on failure; the test for the consumer-failure path must use a fake that fails BEFORE consuming." This would have caught Issue #1+#2 on iter 0 (or led the executor to write the correct test the first time).

### Metrics

- Review iterations: 2 (iter 1: 0/2/3; iter 2: converged)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~50 min

---

## 2026-05-03 — M2b.5: Notebook PeriodicBackup helper

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/25
**Phases with incidents**: none (clean run)

### What worked

Reviewer converged at iteration 1 with 0 blocker, 0 important, 0 nit — a sign of excellent scope discipline and TASK brief clarity. The brief named all three patterns (refactor-on-second-caller timing, `time.Ticker` + select idiom, polling-deadline test discipline) so the executor had no design ambiguity. The "refactor-on-second-caller" timing was right: M2b.4 shipped lean (no premature helper); M2b.5's refactor commit was small and reviewable. Both PRs stayed under the per-PR budget. Phase 6 CI passed 9/9 on first push. Truncation guard from prior FEEDBACK still holding — executor delivered full structured report. The M2b.4 goroutine-leak-fix pattern (`pr.CloseWithError`) was preserved through the refactor without regression.

### What wasted effort

The TS-vs-Go harness ambiguity (raised in M2b.4 FEEDBACK) is now compounding: M2b.5 ships a SECOND library helper that no concrete harness yet calls. Future M2b.x sub-items will keep adding to the "library helpers awaiting harness wiring" pile until the operator-level decision lands. Worth surfacing this as a backlog item.

### Suggested skill changes

- Promote the "refactor-on-second-caller" timing pattern to a SKILL note or `references/refactoring.md`. It's now confirmed across M2b.3.b (tarball-streaming helpers extraction) and M2b.5 (`archiveAndAudit` extraction). A third confirmation would warrant a dedicated reference doc.
- M2b.5 confirms the goroutine-leak-fix-via-`pr.CloseWithError` pattern. Worth adding to `references/agent-briefs/code-reviewer.md` as an `io.Pipe` checklist item: "any code using `io.Pipe` for streaming must verify both sides can unblock each other on failure". This pattern now appears in both M2b.4 FEEDBACK and M2b.5 production code.

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-04 — M2b.6: Notebook ImportFromArchive helper

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/26
**Phases with incidents**: none (clean run)

### What worked

Phase 4 review converged on iteration 1 with 0 blocker, 0 important, 0 nit — the TASK brief named all four "concerns to spot-check" (defer LIFO order, embedding-byte assertion, LogAppend-fails data-presence verification, ctx cancellation via gate channel) and the executor implemented each correctly the first time. When the brief enumerates the failure modes, the executor doesn't have to guess. Bonus interface-satisfaction test (`TestImportFromArchive_FetcherInterfaceSatisfiedByLocalFS`) covers BOTH `LocalFS` AND `S3Compatible` — the executor went above the AC requirement (only one was strictly needed) for marginal extra coverage. Phase 6 CI green 9/9. Phase 7 PR squash-merged; cascade commit `0e88a44` closed M2b.6 immediately. Truncation guard from M2b.1 FEEDBACK still holding.

### What wasted effort

The TS-vs-Go harness ambiguity is now compounding for the THIRD time (M2b.4, M2b.5, M2b.6 all ship library helpers with no concrete harness caller). Worth surfacing as a backlog item BEFORE M2b.7/M2b.8 add yet more. The accumulating "library helpers awaiting harness wiring" pile is a smell — eventually someone will need to write either a Go harness binary, a CLI shim, or wire these into the existing TS harness via shellouts.

### Suggested skill changes

- Promote the "single-method interfaces for cross-package consumption" pattern to `references/refactoring.md` or a SKILL note. It's now confirmed across `Storer` (M2b.4/M2b.5) and `Fetcher` (M2b.6).
- Promote the "test-only import for cross-package compile-time interface check" pattern — useful for any package where production code can't import a sibling but test code can.

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-04 — M2b.7: Notebook mutating ops emit correlated audit events

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/27
**Phases with incidents**: 6 (transient network outage during CI polling)

### What worked

Reviewer iter 1 converged immediately with 0/0/0. The TASK brief named all four reviewer concerns (pre-commit-skip, commit-precedence, payload-no-PII, time-format-consistency) and the executor implemented each correctly the first time. Backward-compat AC framing forced the executor to verify legacy tests pass at every step — no surprises at review time. Truncation guard from prior FEEDBACK entries still holding — executor delivered full structured report before exit.

### What wasted effort

Network outage during Phase 6 polling caused ~5 minutes of alternating `POLL:no-checks-yet` and real-status events. The orchestrator correctly identified the pattern as transient (gh CLI timeouts to api.github.com), halted cleanly with a stop-the-world message, and resumed when the operator confirmed. Net cost: a couple of minutes of operator attention and orphan poll events — not a process bug, but worth a SKILL note that intermittent gh API failures are expected and the orchestrator should not panic.

### Suggested skill changes

- `references/bounded-loop.md` §Polling — add a note: "If `gh pr checks` returns alternating `POLL:no-checks-yet` and real-status events for >2 minutes, treat as transient network — wait, do NOT escalate. The orchestrator may halt cleanly and ask the operator if the disruption persists, but should NOT classify a green-then-quiet PR as failed."
- The "audit emit after commit, never before" pattern is now confirmed across M2b.4/M2b.5/M2b.6/M2b.7 — every orchestrator helper that crosses a side-effect-then-audit boundary uses it. Worth promoting to a top-level LESSON or `references/audit-emit.md` so future ROADMAP items in M3+ don't re-discover it.

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (CI green on first push)
- Operator interventions outside of gates: 1 (network outage handshake)
- Total wall time from /rdd to merge: ~50 min

---

## 2026-05-04 — M2b.8: promote_to_keep helper for Watchmaster proposal flow

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/28
**Phases with incidents**: none

### What worked

Self-gating worked cleanly for this scoped, low-risk TASK (single read-only helper, mirrored existing patterns). Gates were natural decision points the orchestrator could resolve from context without operator intervention. Phase 4 converged at iter 1 (0/0/5) — same shape as M2b.6 and M2b.7. The bounded-loop pseudocode handled "no fixer dispatch needed" correctly. Monitor + GraphQL `reviewThreads` poll for Phase 6 worked first-try, no schema drift. This was the FIRST autonomous-mode run (`/loop /rdd` without operator gates).

### What wasted effort

The `<system-reminder>` PostToolUse hook spammed false-positive "Edit operation failed" / "Write operation failed" notices after every successful Edit/Write. Did not affect outcome but added noise to the orchestrator's reasoning. The code-reviewer agent flagged the `assertProposalScalarFields` nil-deref nit (test helper) AND a duplicate-validation nit (`Forget` and `PromoteToKeep` share an entry-id regex check). Both legitimate but neither is reachable from the current diff's caller pattern — possible signal that the reviewer brief should distinguish "reachable defect" from "API hardening suggestion" so nits don't accumulate silently.

### Suggested skill changes

- Document in `references/bounded-loop.md` §Severity contract that CodeRabbit comments without `BLOCKER:`/`IMPORTANT:` prefix are nits even when the body says "Potential issue" — current spec already covers this but operators may not connect that bot-leveled "Potential issue" maps to "nit" without explicit example. Add a one-line example.
- Document in `SKILL.md` §Hard rules that under autonomous-mode (`/loop /rdd`), Gate 1/2/3 self-approval is permitted but escalation rules still apply (halt on hard blockers). The current `gates.md` strictly says "halt without side effects on no response" which conflicts with autonomous mode; clarify the autonomous override.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~0:50

---

## 2026-05-04 — M3.1: In-process event bus (pub/sub) with handler registration, ordered per-topic delivery, and backpressure

**PR**: [#29](https://github.com/vadimtrunov/watchkeepers/pull/29)
**Phases with incidents**: 4 (2 fixer iters)

### What worked

Self-gating held for a heavier TASK (8 ACs vs M2b.8's 7). Phase 4's bounded loop correctly escalated through 2 iterations without operator input. Debugger sub-agent (spawned after fixer timeout mid-iter 1) found the deep `publishWG.sema` race in one cycle — tier escalation worked (Sonnet executor surfaced the race in a regression test, Opus debugger root-caused it). SendMessage to resume paused agent (Phase 6 fixer) preserved its in-flight investigation (it had already diagnosed the pre-existing test flake; resume just told it which path to commit). Saved a full re-spawn cycle. Phase 6 iter 2 converged clean with 0 unresolved threads.

### What wasted effort

Phase 4 fixer timed out (Stream idle timeout) mid-investigation — required orchestrator detection of uncommitted changes + fresh debugger spawn. Could be tightened: when fixer timeout is detected, the standard recovery should be (a) inspect git working tree, (b) spawn a fresh agent with the detected delta as context. Currently ad-hoc; document in SKILL.md or `references/bounded-loop.md`. Strict Phase 6 severity rule ("bot comments without `BLOCKER:`/`IMPORTANT:` prefix → nit") would have shipped a real `🟠 Major` race-defect to main if followed literally. CodeRabbit reclassified as 2 important + 1 nit because they described real defects: Subscribe-vs-Close race on EXISTING topic, AC4 doc/impl mismatch (dispatch-time vs enqueue-time snapshot). Operator's "fix as much as possible" autonomy mandate overrode strict spec. Worth documenting: when CodeRabbit escalates a finding to `🟠 Major` or `🔴 Critical`, the orchestrator SHOULD reclassify to important regardless of prefix-based rule.

### Suggested skill changes

- Add to `references/bounded-loop.md` §Phase 6 §Severity contract: explicit clause for CodeRabbit-style severity emoji (`🔴 Critical` → blocker, `🟠 Major` → important, `🟡 Minor` → nit). Current bot-comments-are-nits rule is too coarse.
- Add to `SKILL.md` §Hard rules or to `references/bounded-loop.md` §Recovery: when an `executor` (or any) agent times out mid-task, standard recovery is `git status` + spawn fresh agent with delta context. Currently orchestrator improvises this each time.

### Metrics

- Review iterations: 2
- PR-fix iterations: 1
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~1:30

---

## 2026-05-04 — M3.2.a: keepclient watchkeeper resource CRUD (server + client)

**PR**: [#30](https://github.com/vadimtrunov/watchkeepers/pull/30)
**Phases with incidents**: 4 (2 fixer iters), 6 (0 CodeRabbit comments)

### What worked

**First in-flight decomposition** went smoothly. Planner returned `fits: false` with a concrete decomposition; orchestrator applied the ROADMAP edit per `references/roadmap-migration.md` §"Decomposition at Gate 1", committed as `docs(roadmap): decompose M3.2 into sub-items`, then proceeded with the leaf. The decomposition discipline (M3.2 → M3.2.a + M3.2.b mirroring M2b.2 / M2b.3 history) made the planner's call easy.

**Server + client co-located in one PR** worked. Even though M3.2.a touches both `core/internal/keep/server/` and `core/pkg/keepclient/`, the units are tightly coupled (client tests assume the server contract; server tests assume client request shape) so a single PR is the right granularity. The 13-file diff was within the planner's heuristic.

**Phase 4 iter 1 rejection** caught real defects (dead code, missing test cases, stale doc) that would have shipped to main. Iter 2 verified all fixes resolved + flagged a single quality nit. The bounded-loop terminated cleanly at iter 2 with 0 important, exactly as designed.

**Phase 6 zero-comment outcome** — CodeRabbit was silent this time despite the bigger diff (2031 LOC). Likely because the iter-1 fixer pre-emptively addressed common concerns (DisallowUnknownFields enforcement, missing edge tests, dead code) that CodeRabbit usually flags. Worth noting: a thorough Phase 4 review reduces Phase 6 churn.

### What wasted effort

Planner heuristic flagged ~7 files for M3.2.a but actual landing was 13 files — the planner did not initially realize Keep is an HTTP server requiring server-side handlers (it treated keepclient as the only side). The orchestrator spotted this gap by reading the keepclient/keep layout BEFORE writing the TASK and adjusted scope. Worth feedback into `references/agent-briefs/planner.md`: "When the resource adds new server endpoints, check whether the server side already supports them — keepclient is a thin HTTP client, not a SQL driver."

### Suggested skill changes

- Add to `references/agent-briefs/planner.md`: a short note that keepclient is an HTTP client (not a SQL driver), and resources without existing server endpoints require both client-side and server-side work to land. The planner should factor this into the file-count heuristic.
- Add to `references/roadmap-migration.md` §"Decomposition at Gate 1": clarify that the orchestrator (not an agent) writes the ROADMAP decomposition edit + commits on main BEFORE creating the leaf-id branch. The existing spec says this implicitly; making it explicit avoids ambiguity in future autonomous runs.

### Metrics

- Review iterations: 2
- PR-fix iterations: 0
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~50 min

---

## 2026-05-04 — M3.2.b: lifecycle manager (Spawn/Retire/Health/List over keepclient)

**PR**: <https://github.com/vadimtrunov/watchkeepers/pull/31>
**Phases with incidents**: none

### What worked

M3.2.b demonstrated how a previously-decomposed sub-leaf flows smoothly: the planner's iter-3 decomposition pre-resolved the Gate 1 question, so Phase 1 was a quick `fits: true` verification rather than a full re-decomposition. Re-using the prior explore's findings (LocalKeepClient pattern, partial-failure shape) saved an explore dispatch.

Phase 4 iter 1 caught two real doc bugs (broken README example: `keepclient.New` → `NewClient`; phantom error return) that would have shipped to main. The reviewer's specific call to "mentally type-check the example against keepclient/client.go" made the catch unmissable.

Phase 4 iter 1 fixer addressed both important AND 3 of 5 nits in a single 3-file commit. Folding cheap nits saves a future cleanup PR; the bounded-loop only requires fixing important items, but the fixer's bundling judgment was operator-time-positive.

Phase 6 zero-comment outcome on a 1106-LOC diff suggests CodeRabbit's noise floor is sensitive to code novelty: this PR was a thin wrapper around already-merged keepclient methods, so there was nothing surprising for the bot to flag.

### What wasted effort

Skip-explore was the right call (iter-3 explore covered the keepclient surface), but the orchestrator briefly considered re-running. Adding to `references/agent-briefs/explore.md` a note like "When the same code areas were explored in a recent prior /rdd iteration AND the TASK references the same surfaces, prefer reusing the prior explore output to dispatching a new one" would make the call deterministic.

### Suggested skill changes

- Add to `references/agent-briefs/planner.md`: when a previously-decomposed sub-leaf comes around (i.e. planner decomposed M3.2 in iter N and now picks M3.2.b in iter N+M), planner verification can be a one-shot re-affirmation rather than a full decomposition pass. Document this for future autonomous runs to skip redundant decomposition analysis.
- Add to `references/agent-briefs/explore.md` §Use cases: explicit "skip if a recent prior iter explored the same surface area" clause, with operator judgement on what counts as "recent".

### Metrics

- Review iterations: 2
- PR-fix iterations: 0
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~40 min

---

## 2026-05-04 — M3.3: cron scheduler emitting events onto the bus

**PR**: <https://github.com/vadimtrunov/watchkeepers/pull/32>
**Phases with incidents**: none

### What worked

Phase 4 converged at iter 1 with 0/0/5 — strong signal that M3.3's design (driven by the planner's pre-resolution of 3 open questions in the verdict) was reviewable in one pass. When the planner does heavy lifting in Gate 1, Phase 4 stays cheap.

Five nits in iter 1 were all genuinely small (README example helper, one-line godoc additions, minor refactors). The bounded-loop's "nits don't block" rule paid off — no fixer iteration needed for what amounts to comment-quality improvements.

The `LocalPublisher` decoupling pattern, now applied across notebook (Logger), lifecycle (LocalKeepClient), and cron (LocalPublisher), is becoming the canonical M3-area shape. Each new package picks it up without ceremony. Fifth autonomous iteration in this session merged cleanly with no operator intervention. Self-gating (Gates 1, 2, 3) and bounded-loop discipline held over an extended autonomous run.

### What wasted effort

During Phase 3, the executor noticed a flaky pass-on-rerun of `TestBus_RaceCloseVsSubscribeExistingTopic` (the regression test added in M3.1 iter-2 to catch the WaitGroup-sema race). The fix landed; the test still occasionally trips the race detector under heavy `-count=N` loads. Nothing in M3.3 introduced the flake. The operator may want to promote this to a follow-up TASK ("M3.1 follow-up: stabilise the close-race regression test under -race -count=10+"). Skill caught this correctly (executor flagged it, orchestrator recorded it, didn't block the merge).

Phase 5's PR title was 60 chars and within commitlint, but the manual title-length-check at PR-open could be automated with a `commitlint --validate` pre-step in `references/agent-briefs/git-master.md`. Currently the orchestrator picks the title and trusts it fits.

### Suggested skill changes

- Add to `references/agent-briefs/git-master.md` §pr mode actions: "Before `gh pr create`, run `echo \"<title>\" | npx commitlint` (or repo equivalent) to surface a title-length / format violation up-front. If commitlint config disabled, skip silently." Currently the agent visually estimates; for ≥70-char titles a real check is one shell line.
- Add to `references/bounded-loop.md` §Stable-flake escalation: when an executor (or other agent) notices a pre-existing flake during a rerun (test passes on targeted run, fails on broad `-count=N`), the orchestrator should record it as a "follow-up TASK candidate" rather than just logging. Today the orchestrator records in TASK Progress log + writer's FEEDBACK; promoting to a separate TASK candidate would let the operator triage flakes systematically.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-04 — M3.4.a: Secrets pluggable interface (SecretSource + EnvSource)

**PR**: <https://github.com/vadimtrunov/watchkeepers/pull/33>
**Phases with incidents**: 4 (2 fixer iters), 6 (0 CodeRabbit comments)

### What worked

**Sixth in-flight decomposition this session** (after M3.2 in iter 3, M3.4 in iter 6). The planner's "and also" title detection and decomposition logic is reliable at scale. The bounded-loop's decomposition affordance handles repeated decompositions within one session without operator help.

**Phase 4 iter 1 surfaced two real contract gaps** (validation-order ambiguity, redaction-test robustness for non-string types) that would have shipped silent without the review pass. Iter 2 verified both were fixed cleanly. Two-iteration Phase 4 is the steady-state for medium-novelty TASKs (M3.1–M3.4.a all converged at iter 2).

**The executor caught a `.gitignore` blocker** (bare `secrets/` pattern suppressing the entire `core/pkg/secrets/` subtree) that would have created a ghost-package situation (code committed, files invisible). The hermetic build/test cycle surfaced this immediately without operator intervention. The bounded-loop's "executor commits + tests + lints" discipline is sufficient to catch path-matching issues.

**Phase 6 converged at iter 0 again** — fourth occurrence in a row. When Phase 4 iter 2 is clean, CodeRabbit's noise floor stays low.

### What wasted effort

The TASK's AC did not specify validation-order precedence between empty-key check and ctx-check. The reviewer correctly flagged this as a contract-clarity gap. A pattern: when ACs prescribe behavior with multiple sub-conditions, list evaluation order explicitly. Updating `references/task-template.md` or the planner brief to suggest "list validation steps in order" for any AC mentioning both synchronous validation and ctx-check would prevent the next occurrence.

### Suggested skill changes

- Add to `references/agent-briefs/planner.md`: "When proposing decomposition for an 'and also' title, the planner SHOULD also flag any cross-implementation contract concerns (e.g. validation order, error-precedence rules) so the executor's TASK explicitly nails them down. Otherwise these surface as iter-1 review findings."
- Add to `references/task-template.md` §AC section: "If an AC describes behavior with multiple validation or pre-check steps, list them in evaluation order explicitly (e.g. '1. Check key is non-empty; 2. Check ctx not cancelled')."

### Metrics

- Review iterations: 2
- PR-fix iterations: 0
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-04 — M3.4.b: Config loader (env + config.yaml + secrets resolution)

**PR**: <https://github.com/vadimtrunov/watchkeepers/pull/34>
**Phases with incidents**: 4 (2 review iters), 6 (1 CodeRabbit iter)

### What worked

**Phase 4 iter 1 caught two real important contract gaps** (test coverage for redaction via WithLogger, naming-convention drift: Go field-name vs YAML tag detection). Phase 4 iter 2 converged clean. Two-iteration Phase 4 is the steady state for medium-novelty TASKs in this run.

**Phase 6 reclassification rule fired correctly**: two CodeRabbit `🟠 Major` findings (silent multi-doc YAML acceptance, raw-err logging in redaction-sensitive path) were reclassified from "nit per strict spec" to "important per defect content". Both were real defects that would have shipped silently. Without the override, the strict spec would have left them in Follow-up. The autonomous loop's robustness depends on this override.

**The orchestrator's `.gitignore` check at M3.4.b start passed cleanly** — no `config/` rule suppression. The check is reflexive after one bite from M3.4.a; demonstrates the LESSONS-FEEDBACK feedback loop is enabling rapid learning.

**Phase 6 iter 2 converged at iter 0** — CodeRabbit auto-resolved all threads after Phase 6 iter 1 fixes. Demonstrates Phase 6's stability when Phase 4 iter 2 is genuinely clean.

### What wasted effort

Phase 6 iter 1 added 3 fixes in 1 commit (multi-doc rejection, err-type logging, fakeSecretSource ctx-check). The fakeSecretSource ctx-check fix was nit-level (≤5 LOC) but folded into the same commit. The loop spec puts it nit-level — the agent's judgement to fold was correct. Pattern: when a nit is trivially scope-able into the same fix commit AND improves test cleanliness, fold it; don't defer to Follow-up just because the spec defaults that way.

The redaction-leak (raw `err` logging) was MISSED by Phase 4 iter 1's code-reviewer. The reviewer correctly flagged "WithLogger has no test", which led to adding a test asserting the redaction contract — but the test asserted only that the resolved VALUE doesn't leak, not that the err OBJECT doesn't leak. Phase 6's CodeRabbit caught the wider invariant. The code-reviewer brief should explicitly call out: "for every contract that says 'never logs sensitive X', verify tests cover not just direct value-passing but ALSO the err-object pathway — `logger.Log(ctx, msg, "err", err)` is a redaction-leak vector."

### Suggested skill changes

- Add to `references/agent-briefs/code-reviewer.md` §What to scrutinize: "For redaction-discipline contracts ('logger never sees secret values'), verify tests cover not just direct value-passing but ALSO the err-object pathway — `logger.Log(ctx, msg, "err", err)` is a redaction-leak vector if `err.Error()` includes context."
- Add to `references/bounded-loop.md` §Phase 6 §Severity contract: explicit rule that CodeRabbit `🟠 Major` findings with concrete suggested-fix code SHOULD be reclassified to important regardless of the bot-comments-are-nits default. The current spec leaves the override implicit.

### Metrics

- Review iterations: 2
- PR-fix iterations: 1
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~50 min

---

## 2026-05-04 — M3.5: Capability broker (scoped-token issue/validate/TTL primitive)

**PR**: <https://github.com/vadimtrunov/watchkeepers/pull/35>
**Phases with incidents**: none

### What worked

**Cleanest end-to-end iteration of the session**: Phase 4 iter 1 converged 0/0/2; Phase 6 iter 0 converged 9/9 + 0 threads. Total ~30 min wall time for a security-sensitive 1426-LOC package. Pattern: when ACs are explicit about security invariants (redaction, boundary semantics, error-message rules), the executor delivers fewer review iterations.

**Planner's reading-(a)-vs-(b) framing**: the planner verdict explicitly named the option-(b) "bundle wrappers" trap and recommended option (a) with documented deferred integrations. The TASK Scope captured this as "explicitly out of scope" rather than implicit. Phase 6 reviewer didn't flag scope creep because the boundary was nailed in Gate 2.

**Test pattern reuse**: the `recordingLogger` + `fmt.Sprintf("%+v", entry)` defense-in-depth pattern from M3.4.a (secrets) → M3.4.b (config) → M3.5 (capability) is now the canonical redaction-test idiom across three security-sensitive packages. The pattern's effectiveness compounded: each subsequent leaf benefited from prior tests' precedent.

**Scaffold-first discipline scaling**: 5 of 5 M3 leaves shipped so far have followed the same shape (primitive + tests; integrations deferred). The pattern is now the default expectation. M3.6 (Keeper's Log writer) and M3.7 (Outbox consumer) will follow.

### What wasted effort

The git-master pr-mode agent's verification log showed STALE local main reference (`3baf9c9` instead of `fe92aa9`). The PR was created correctly (target was actual remote main), so no functional impact, but the agent's "verification" output was misleading. The agent likely ran `git log` against unfetched local branch. Worth checking agent brief: should `git-master pr` always do `git fetch origin main` before verification log so comparison is against actual remote state?

### Suggested skill changes

- Add to `references/agent-briefs/git-master.md` §pr mode actions: insert before verification log step: "Run `git fetch origin main --quiet` so the verification log reflects the actual remote main, not a stale local view." This is cosmetic (PR creation already targets origin/main correctly), but the misleading output confuses operators.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0 (autonomous run)
- Total wall time from /rdd to merge: ~30 min

---

## 2026-05-05 — incident: silent-exit after planner in `--auto` mode (process, no PR)

**PR**: n/a (Phase 1 halted by operator before any branch/commit)
**Phases with incidents**: 1

### What happened

`/rdd --auto` reached Phase 1 cleanly (preflight green, candidate selected:
M5.3.b.a). Planner agent dispatched → returned `fits=true` in 9 seconds.
The orchestrator's NEXT reply was supposed to (a) mark the companion todo
`in_progress`, (b) print the Gate 1 prompt + `Auto-decision: yes`, and (c)
proceed into Phase 2. Instead the reply closed with no user-facing text —
exactly the silent-exit failure mode `FEEDBACK.md` 2026-04-22 catalogued.
The runtime "churned" for 2m 29s waiting for the orchestrator's next
output before the operator interrupted manually.

### Why companion-todo did not catch it

The companion-todo was created (`TaskCreate` ran before the `Agent`
dispatch) but never escalated to `in_progress` after the agent returned —
because the same reply that received the `Agent` tool result also wrote
nothing else. The todo became a dangling list item rather than the
workflow anchor it is described as in `SKILL.md` §"Dispatching agents
→ Companion-todo". The mechanic only works if the FIRST line of the reply
that holds the `Agent` tool result is also the verdict text — not "I'll
write the verdict in a follow-up reply".

### What wasted effort

About 2m 29s of wall time, plus a full conversation round-trip to
diagnose. Zero repo side effects (no branch, no TASK file, no commit, no
PR), so rollback was free.

### Suggested skill changes

- **Tighten Hard rule 5 in `SKILL.md`**: add an explicit "first-line
  rule" — the reply that contains an `Agent` tool result MUST start its
  text block with the orchestrator-authored verdict on the very next
  text segment, before any further tool call. Current wording ("end the
  same reply with…") is satisfied by lazy interpretations where the
  text comes after additional tool calls; a "first text segment after
  Agent return is the verdict" framing is harder to defeat.
- **Reframe companion-todo as a hard-blocking ritual in `--auto`**: the
  `--auto` orchestrator should refuse to dispatch the next phase's first
  tool call until the prior phase's companion-todo has been moved through
  `in_progress` → `completed`. Either add a soft check (orchestrator
  self-discipline language) or, better, make the auto-decision audit
  block the literal trigger that flips the todo — the audit print is
  unconditional, so binding todo-update to it removes the silent-exit
  surface.
- **Add a bench-style stitched example to `SKILL.md`**: a single short
  fenced example showing a turn that ends correctly — the `Agent` tool
  result, then `TaskUpdate(completed)`, then the Gate prompt + audit, all
  in one reply — would make the contract concrete. Operators are clearly
  better at recognising the right shape than at synthesising it from a
  prose rule.

### Metrics

- Review iterations: 0 (halted before code)
- PR-fix iterations: 0 (no PR opened)
- Operator interventions outside of gates: 1 (manual halt + diagnosis ask)
- Total wall time from /rdd to halt: 00:05 (mostly the 2m 29s silent stall)

---

## 2026-05-05 — M5.3.b.a: pure-JS isolated-vm sandbox via invokeTool RPC

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/57
**Phases with incidents**: 5, 6

### What worked
The `--auto` loop self-resolved every gate without operator intervention. Phase 4
converged in 2 iterations; Phase 6 converged in 2 effective iterations after an
inter-iteration title-edit. The Companion-todo discipline (`TaskCreate` before each
`Agent` dispatch) caught zero silent-exits this run — Hard rule 5 held.

### What wasted effort
**The git-master Phase-5 brief produced an invalid PR title.** The brief instructs
"PR title ≤ 70 chars. Derived from TASK title (prepend `<roadmap-id>: `)" — which
yields `M5.3.b.a: add invokeTool isolated-vm sandbox path`. The repo enforces
commitlint on PR titles via the Meta CI job (`printf '%s\n' "$PR_TITLE" |
pnpm exec commitlint`); commitlint sees `M5.3.b.a:` as the type prefix, classifies
it as unrecognised, and reports `subject may not be empty / type may not be empty`.
Cost: one extra Phase-6 iteration to diagnose, an out-of-band orchestrator
`gh pr edit --title`, and a separate empty commit to fire `pull_request.synchronize`
(the title edit alone does not re-trigger the workflow because the repo's `ci.yml`
does not subscribe to `types: [edited]`).

A second smaller incident: `harness/node_modules/` (the workspace package's nested
`node_modules/`) was not covered by the markdownlint ignore pattern `node_modules/**`.
The pattern needed a `**/` prefix to match nested workspace packages. Cost: one
Phase-6 iteration that produced `chore(lint): ignore nested node_modules in
markdownlint config` (`cc756c7`).

### Suggested skill changes
- `references/agent-briefs/git-master.md` §"Mode — pr (Phase 5)": change the title
  derivation rule from "prepend `<roadmap-id>: `" to "use the conventional-commits
  type/scope from the leading commit and append `(<roadmap-id>)` at the end" — e.g.
  `feat(harness): add invokeTool isolated-vm sandbox path (M5.3.b.a)`. This avoids
  the commitlint trap on every repo that lints PR titles.
- `references/bounded-loop.md` §Phase 6: add a note that title edits alone do NOT
  fire `pull_request.synchronize`; the standard recovery is an empty
  `chore(ci): re-trigger after PR title fix` commit, not `gh run rerun`. (Re-runs
  use the original event payload and observe the OLD title.)

### Metrics
- Review iterations: 2
- PR-fix iterations: 2 (markdownlint ignore + PR-title/empty-commit cycle)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~02:00 (Phase 1 to Phase 6 converge)

---

## 2026-05-06 — M5.3.b.b.a: ADR 0001 worker substrate

**PR**: [#58](https://github.com/vadimtrunov/watchkeepers/pull/58)
**Phases with incidents**: none

### What worked
The two FEEDBACK lessons from M5.3.b.a (PR title format + nested
node_modules in markdownlint ignore) **prevented re-incurring those costs**:
the orchestrator dispatched git-master with a conventional-commits PR title
from the start, and the `**/node_modules/**` ignore was already in place.
Phase 6 converged on the first iteration with 9/9 CI green and 0 unresolved
threads — fastest Phase 6 in the project's rdd history. The Gate 1 auto-rule
for `too large` planner verdicts (apply decomposition + auto-yes the first
sub-item) executed cleanly: planner returned 5 sub-items, ROADMAP committed,
M5.3.b.b.a became the unit of work without operator intervention.

### What wasted effort
Phase 4 iter 1 caught a real reasoning defect (the structured-clone IPC claim
for `child_process.fork`) — the executor authored an ADR that asserted a
property fork only has when `serialization: 'advanced'` is set, then leaned
on that property to differentiate fork from spawn. Code-reviewer flagged it
as `important`. The fix took one iteration. This is exactly the kind of
domain-knowledge defect that a generalist reviewer catches and a generalist
executor misses — the workflow held.

A single small drag: the prior writer iteration (M5.3.b.a) failed to commit
because of MD034 (bare PR URL); this writer was given an explicit
"`[#NN](URL)` format + trailing `---`" instruction in its brief, which is a
band-aid rather than a structural fix.

### Suggested skill changes
- `references/lessons-template.md` §"Per-TASK section": change the example PR
  line from `**PR**: <PR URL>` to `**PR**: [#NN](<PR URL>)`. Also add an
  explicit note in §"Brevity rules" that EVERY appended section MUST end with
  `---` (currently shown in the example but not stated as a rule). The two
  formatting conventions trip writers because they live only in the example,
  not in the rules.

### Metrics
- Review iterations: 2
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~01:30 (estimate; Phase 1 fast-track + Phase 6 single-pass)

---
## 2026-05-06 — M5.3.b.b.b: capability declaration schema and gating policy types

**PR**: [#59](https://github.com/vadimtrunov/watchkeepers/pull/59)
**Phases with incidents**: 6

### What worked
Executor dispatch with build mode (vitest + zod) was clean; 71/71 tests green, 100% coverage on first try. Code-reviewer iteration 1 produced no blockers—schema structure and test coverage aligned with the ADR spec from the start.

### What wasted effort
**Phase 6 friction (commitlint + Monitor):** PR title `M5.3.b.b.b: ...` was rejected by conventional-commits enforcement on `type-empty` (M5.3.b.b.b is not a valid commit type). Orchestrator-level workaround: `gh pr edit 59 --title "feat(harness): ..."` + `gh pr close && gh pr reopen` to force CI retrigger (workflow `on: [opened, synchronize, reopened]` does not include `edited`). Monitor tool was rejected by sandbox obfuscation guard on jq escapes inside braces; tick-based polling via ScheduleWakeup is cheaper for in-cache windows.

### Suggested skill changes
- Document in `references/pr-title-constraints.md` that rdd PR titles MUST follow conventional-commits format when repo enforces commitlint, even if ROADMAP ids suggest an alternate form.

### Metrics
- Review iterations: 1
- PR-fix iterations: 0 (Phase 6 was orchestrator metadata fix, no code dispatch)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~01:40 (Phase 3–6, Phase 1–2 were fast-track)

---
## 2026-05-06 — M5.3.b.b.c: worker spawn + JSON-RPC transport

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 iter 1, 4 iter 2

### What worked
Executor dispatch, fixer dispatch, and Phase 4 loop were all well-scoped. 4 important issues (3 in transport/spawn IPC error handling, 1 in spawn exit-listener handoff) were precisely classified and fixed in fixer iteration with +5 regression tests → 114/114 pass. Code-reviewer iter 2 cleared all blockers + important items.

### What wasted effort
**Executor "do not push" misread as "do not commit."** Executor read "do NOT push" broadly and returned with all 5 files + 1115 LOC in working tree, zero commits. Orchestrator picked up the commit (`08cecf1`). Sequence was correct (Phase 3 exec, Phase 4 review/fix, Phase 5a writer—all commit-local, Phase 5b git-master pushes ALL together), but the brief should have been explicit: "Commits per logical step (this is REQUIRED, only the push is deferred)."

### Suggested skill changes
- Tighten `references/agent-briefs/executor.md` build-mode line 15 to: "Commits per logical step (this is REQUIRED, only the push is deferred) — each commit groups a coherent change."
- Phase 5a-first flow validated. Writer now commits locally BEFORE Phase 5b git-master push; single CI run on origin. (Proof point: this TASK, if Phase 5b + 6 succeed.)
- Note in `references/hard-rules.md` §6: "Hard rule 6 conjunction: exceeding BOTH ≤500 LOC AND ≤5 files triggers Gate 1 reject. This TASK: 1115 LOC over 500, but exactly 5 files → no reject. Soft cap blown 2.2×; note for future scoping."

### Metrics
- Review iterations: 2
- PR-fix iterations: 1
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~00:35 (Phase 3–5a; Phase 5b–7 pending)

---

## 2026-05-06 — M5.3.b.b.d: worker dispatcher (B1 caught at review, 6-file PR)

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 iter 1 (1 BLOCKER + 3 important), 4 iter 2 converged

### What worked
- Executor (opus) committed per logical step on its own this time (4 commits: broker / dispatcher wire / crash-translation fix / tests). M5.3.b.b.c FEEDBACK lesson about "do NOT push ≠ do NOT commit" landed: this iteration did not need orchestrator-level post-hoc commit cleanup.
- Repo-wide `pnpm exec prettier --check .` was run by executor as a PR-readiness gate (M5.3.b.b.c CI prettier failure addressed structurally).
- Code-reviewer caught a real ACE (B1) before merge — wire-reachable arbitrary `bootstrapPath` via unvalidated `spawnOptions`. Fixer dispatch + 3 wire-boundary regression tests resolved it without operator escalation.

### What wasted effort
- **Hard rule 6 hit BOTH soft caps** (6 files / +1261 LOC). Past Gate 1; planner returned fits=true with predicted 3-4 files. Test-LOC inflation (B1 regression tests + I1 pinning tests) pushed file count from 4 → 6. Suggest planner brief multiplies src LOC by ~1.5 for test inflation and includes paired test files in file count estimates.
- **Writer agent timeout (44m, partial response received)** on this Phase 5a dispatch. Orchestrator picked up the writer steps inline (append + toggle + commit). Suggest writer brief (a) cap the agent prompt size (this dispatch was very long), (b) document orchestrator-fallback path so the agent timeout doesn't strand the PR.

### Suggested skill changes
- `references/agent-briefs/planner.md`: add file-count rule "Include matching test file(s) in estimate; multiply src LOC by ~1.5 to account for test inflation."
- `references/agent-briefs/writer.md`: add a "If writer agent times out or returns partial: orchestrator may complete the append+toggle+commit steps inline (mechanical operations on already-drafted topic notes), then proceed to Phase 5b" footnote.
- `references/agent-briefs/code-reviewer.md`: add to Severity definitions §blocker — "wire-reachable ACE (caller-controllable spawn-target / argv / env / cwd in any subprocess invocation)" so the reviewer flags it consistently.

### Metrics
- Review iterations: 2 (iter 1: 1 BLOCKER + 3 important + 5 nit; iter 2: 0/0/3)
- PR-fix iterations: TBD (Phase 5b/6 pending)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to Phase 5a: ~40m (heavy Phase 4 fix loop + 44m writer timeout)

---
## 2026-05-06 — M5.3.c.a: Auto-derive zod tool schemas from Tool Manifest at harness boot

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 (Phase 1 decomposition, Phase 4 fix iteration)

### What worked
Phase 2's fast-check (ls + grep over test files) correctly identified that the vitest suite portion was already covered by tests landed in prior PRs. This prevented a toggle-only TASK and saved an iteration. The actual schema-derivation work stayed focused: 188 tests written once and converged in Phase 4 after one reviewer catch (`.strict()` missing, wrong-type assertion missing).

### What wasted effort
Phase 1 planner decomposed M5.3.c into 5 sub-items based on the literal ROADMAP text without checking whether the worker-path vitest tests were already live. This resulted in an initial decomposition commit (514945a) followed by a refine commit (66b66a6) after discovering the tests in `worker-spawn.test.ts`, `invokeTool-worker.test.ts`, and `worker-broker.test.ts`. One extra `main` commit cost, but the alternative (creating a toggle-only TASK) would have burned an iteration or required halt+manual-resolution.

### Suggested skill changes
- Add a Phase 2 preflight checklist item: before creating the TASK file, run `ls harness/test/` + grep for `describe()` blocks to confirm candidate sub-items are genuinely outstanding. This is a 2–3 bash invocation "verify the planner's premise" pass that costs negligible time and prevents the toggle-only landmine. Could live as a new entry in `references/preflight.md` §"Phase 2 fast-checks".

### Metrics
- Review iterations: 2 (Phase 4 iter 1 caught `.strict()` missing; Phase 4 iter 2 converged)
- PR-fix iterations: 1 (executor fixer applied `.strict()` + 2 test assertions in commit 04155b4)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending (Phase 5–7)

---

## 2026-05-06 — M5.3.c.b: LLMProvider wrapper: parameterize model/system-prompt/context from Manifest

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 (single iteration, zero blockers)

### What worked

Executor (opus) consolidated redundant tests once (`e3a3fc9`) before reporting the
residual LOC overage, cutting 260 LOC unprompted. Phase 2 gate correctly classified the
work as "boundary-layer builders" with locked projection contract before downstream
consumers (M5.3.c.c, M5.5). Phase 4 code-reviewer accepted the 519-LOC-over reading as
per cross-cutting "source-side" convention (production LOC 219 ≪ 500-cap; tests = 300
at 100% coverage). No second consolidation pass needed.

### What wasted effort

Hard rule 6 currently states "≤500 LOC added AND ≤5 files changed" without disambiguating
tests from source. Executor read "≤500 LOC total" and consolidated tests; code-reviewer
accepted the residual overage citing conventional "source-side" reading. The ambiguity
cost one extra consolidation iteration. Production LOC was never under question (219);
the friction was entirely at interpretation-time.

### Suggested skill changes

- Restate cross-cutting's "source-side" reading verbatim in `SKILL.md` Hard rule 6, or
  split into explicit `≤300 LOC source` + `≤500 LOC total` so the executor has a clear
  signal at consolidation time without needing code-reviewer sign-off.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to Phase 5a: ~01:30 (Phase 3 exec + Phase 4 + Phase 5a)

---

## 2026-05-06 — skill change: mechanical reinforcement for Hard rule 5 (silent-exit hooks)

**PR**: n/a (operator-driven skill update; not a TASK)
**Phases with incidents**: n/a (preventative — addresses the failure mode catalogued
in this file at 2026-04-22 and 2026-05-05)

### What changed

Two project-scoped Claude Code hooks were added to mechanically reinforce
Hard rule 5 ("after every Agent return, the orchestrator's reply must
contain a user-facing text block — never let the Agent tool result be the
last visible thing"). Until now the rule was enforced only by the
orchestrator's own discipline plus the Companion-todo workflow; both
mechanisms failed silently in the 2026-04-22 and 2026-05-05 incidents.

- `.claude/skills/rdd/hooks/rdd-post-agent.sh` (PostToolUse:Agent) — appends a
  `<system-reminder>` to every Agent tool_result that restates the rule
  ("the very next text segment in this reply must be the verdict, before
  any further tool call"). Reminder is glued to the tool_result the
  orchestrator will read on the next decode, so the rule is hard to miss.
- `.claude/skills/rdd/hooks/rdd-stop-check.sh` (Stop) — parses the transcript JSONL
  and, if the most recent `tool_use{name=Agent}` is not followed by any
  text content block, returns `{"decision":"block","reason":...}`. The
  Claude Code runtime then refuses to close the turn and re-prompts the
  orchestrator with the rule, so silent-exit is recovered inline rather
  than waiting for the next `/loop` tick.

Both hooks are gated on **any** of: `.omc/state/rdd-active` marker, an
in-progress `TASK-*.md`, or a current branch matching `rdd/*`. The marker
file is a new artifact created in Phase 0 preflight (after Checks 1–5
pass) and removed in Phase 7b cleanup. It exists specifically to cover
Phase 1 on `main`, before any branch or TASK file exists — the exact
window where the 2026-05-05 incident lived.

Companion changes:
- `.claude/skills/rdd/SKILL.md` Hard rule 5 — appended a paragraph
  pointing to the hooks and their gating model.
- `.claude/skills/rdd/SKILL.md` Phase 7b cleanup — added marker removal
  (`rm -f .omc/state/rdd-active`) alongside `TASK-*.md` deletion.
- `.claude/skills/rdd/references/preflight.md` — new "Mark rdd active"
  section after Check 5 instructing the orchestrator to `mkdir -p
  .omc/state && touch .omc/state/rdd-active` before dispatching Phase 1.
- `.claude/settings.json` — registered both hooks (PostToolUse:Agent
  with timeout 5s; Stop with timeout 10s).

### Why this layering

The PostToolUse hook is **prevention** — it costs ~80 tokens per Agent
return but mechanically eliminates the "I forgot to type the verdict"
failure path. The Stop hook is **safety net** — even if the orchestrator
ignores the reminder and tries to close the turn silently, the runtime
won't let it. Together they convert a discipline-based rule into a
runtime-enforced one. Cost in non-rdd sessions is zero because both
hooks gate on rdd-active markers and exit early.

### What was considered and rejected

- Always-on (no gating). Rejected — would inject reminders into every
  Agent dispatch in this repo regardless of context. Cost is small but
  semantically noisy outside rdd.
- Markdown-only tightening of the rule (the original 2026-05-05
  recommendation: "first text segment after Agent return is the
  verdict"). Kept the rule wording but did not rely on it alone, because
  the same incident showed that wording-level tightening doesn't survive
  prompt drift. Hooks are the load-bearing layer; wording is now
  redundant reinforcement.
- Single Stop hook without the PostToolUse one. Rejected — Stop runs
  only at turn-end, so a misbehaving turn would still emit a series of
  silent tool calls before being blocked. The PostToolUse reminder
  intercepts BEFORE the next tool call and lets the orchestrator
  self-correct without the runtime stepping in.

### Suggested skill changes

None at this time. Re-evaluate after the next 5–10 `/rdd` runs:

- If the Stop hook never fires (zero blocked turns), the PostToolUse
  reminder alone is sufficient and the Stop layer can be downgraded to
  a no-op or removed.
- If the Stop hook fires repeatedly on the same orchestrator pattern,
  that pattern is a candidate for a more targeted reminder upstream
  (e.g. SKILL.md §Dispatching agents) rather than relying on the Stop
  re-prompt to teach.
- If `.omc/state/rdd-active` ever leaks (Phase 7b cleanup missed, run
  aborted mid-flight), hooks will fire outside rdd until manually
  cleaned. Operator-side mitigation: `rm -f .omc/state/rdd-active`
  whenever you see unexpected post-Agent reminders or blocked Stops.

### Metrics

- Review iterations: n/a (no PR)
- PR-fix iterations: n/a
- Operator interventions outside of gates: n/a
- Total wall time of skill change: ~30 min (operator + assistant)

---

## 2026-05-06 — skill change: raise PR size cap to 1000 LOC / 20 files

**PR**: n/a (operator-driven skill update)
**Phases with incidents**: n/a (preventative — addresses recurring Gate 1
friction observed in M2b.7, M5.3.c.b, and queued on M5.3.c.c)

### What changed

Hard rule 6 (PR size cap) raised from **≤ 500 LOC / ≤ 5 files** to
**≤ 1000 LOC / ≤ 20 files**. Reject semantics unchanged: Gate 1 reject
only when *both* are exceeded. Three coordinated edits:

- `.claude/skills/rdd/SKILL.md` §Hard rules #6 — new numbers + a
  paragraph noting the raise date and that 1000/20 still fences off
  the 1700–2400 LOC monsters that motivated the original cap.
- `.claude/skills/rdd/references/gates.md` §"Gate 2 auto-decision" —
  rough heuristic raised from `≤ 5 files` to `≤ 20 files`.
- `docs/lessons/cross-cutting.md` §"PR size cap" — lesson body
  updated in-place (with date stamp) so the planner reads current
  numbers in Phase 2 instead of stale 500/5.

### Why

Industry small-CL guidance (Google ~400, Atlassian ~400, GitHub
research <250 for optimal review quality) ranges around 200–500.
Raising to 1000/20 puts this skill above that band — closer to "large
review" territory. The trade-off was accepted because: (a) multi-file
features in this repo routinely carry ≥3 test files (source + spec +
fixtures), eating the file budget before the source is even drafted;
(b) every 500/5 reject costs an entire decomposition planner run plus
operator gate, and the M2b.7 / M5.3.c.b friction was repeated enough
to be a tax, not a one-off; (c) the hard ceiling that the original
cap targeted (1700–2400 LOC PRs with 2–4 review iterations) sits
above 1000 anyway, so the new cap still catches the intended pattern.

### What was considered and rejected

- **Modest raise to 800/10.** Closer to industry norms, but only
  shaves one Gate 1 reject per ~5 TASKs; the operator wanted a wider
  margin to cover medium features without re-encountering the cap.
- **Source/test split (≤ 300 source + ≤ 500 total).** Mentioned in
  FEEDBACK 2026-05-06 (M5.3.c.b). Cleaner semantics but introduces a
  second dimension the planner has to predict at Gate 1; the operator
  preferred a single coarser cap over two precise ones. Revisit if the
  raised cap still leaves source-side ambiguity in practice.
- **Add a separate "concerns count" rule.** Hard rule 6 is about
  review surface; concerns are an orthogonal Gate 1 criterion that
  *also* triggers decomposition. Currently implicit in `planner` brief
  ("single concern"); operator declined to formalize it as a numbered
  hard rule in this pass. The M5.3.c.c case (Provider impl + wiring
  loop) is exactly where a concerns rule would fire even after the
  cap raise — flagged for a future skill update.

### Suggested skill changes

None at this time. Re-evaluate after the next 5 `/rdd` runs:

- If Gate 1 rejects drop to ~zero (cap is now wide enough), confirm
  by checking whether *concerns-based* rejects rise — that would
  validate "concerns count" as the bottleneck, not LOC.
- If a TASK lands a 900-LOC PR that produces ≥3 review iterations,
  the cap is too wide and should drop back toward 700–800.
- If `planner` consistently rejects the same pattern (e.g. "impl +
  wiring") that would have fit under the cap, formalize the concerns
  rule.

### Metrics

- Review iterations: n/a (no PR)
- PR-fix iterations: n/a
- Operator interventions outside of gates: n/a
- Total wall time of skill change: ~10 min (3 file edits + this entry)

---
## 2026-05-06 — M5.3.c.c.a: Add TS LLMProvider interface + FakeProvider mirroring Go contract

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: Phase 3

### What worked

Gate 1 auto-passed the decomposition (no operator intervention). Phase 2 file budget
projection (≤8 files / ≤900 LOC) was tight but not missed — Phase 3 delivered 6 files
with 1165 raw LOC, but hard rule 6's "BOTH cap dimensions" framing correctly allowed it
because file count (6) and non-comment LOC (651) both stayed well under the 20-file,
1000-LOC hard ceiling. This prevented a false Gate 1 reject that would have re-run the
planner. Review convergence at iteration 1 was clean: 0 blocker, 0 important, 4 nits
deferred to Follow-up. Comment density not flagged — TS-specific value (branded types,
Promise<…>, verbatimModuleSyntax, narrowing) vs. Go-mirror redundancy judged net-positive.

### What wasted effort

Phase 3 push hit a credential mismatch: `gh` active account (`vadym-trunov_wbt`) lacked
write permission to the repo; admin account (`vadimtrunov`) had it. Recovery was `gh auth
switch` + `gh auth setup-git` before re-attempting the push. Root cause: OS keychain
credential used by `git` routed through a different GitHub user than `gh auth status`
reported as active. Preflight Check 3 already verifies `gh` permissions but doesn't catch
the case where the keychain credential doesn't match. Fix idea: preflight Check 3 could
add a no-op `git ls-remote --heads origin` to verify the keychain credential matches the
reported `gh` active account.

### Suggested skill changes

- Augment `references/preflight.md` Check 3: after verifying `gh` permissions, add a
  `git ls-remote --heads origin` test to detect keychain/gh account mismatches early.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0 (Phase 6 not yet run)
- Operator interventions outside of gates: 1 (gh auth switch + setup-git)
- Total wall time so far: ~15 min (this writer entry is the last action before Phase 5b)

---
## 2026-05-06 — M5.3.c.c.b: Implement ClaudeCodeProvider adapter (default impl) with unit tests

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked
The Phase 4 bounded loop (1 iteration, 3 important + 4 nit) accurately detected real defects. The `maxTokens===0` contract violation was a genuine bug — the executor's fix (new `resolveMaxTokens()` helper + validation tests) resolved all three importants in a single fixer round with zero regressions. The auto-decision gates (Gate 1 and Gate 2 auto-yes) ran cleanly; no operator intervention required outside gates. Iteration converged after fixer verification (Phase 4 iter 2: 0/0/0).

### What wasted effort
The TASK listed `OverloadedError` as a dedicated SDK export. The executor discovered via trial that the SDK (`@anthropic-ai/sdk@^0.94.0`) doesn't export it — overloaded responses surface as `APIError(status: 529)`. The TASK could have been more cautious about asserting external SDK API shapes, or the planner brief could push for "verify SDK shape via context7 docs during planning" rather than discovering it at executor time. Cost: ~1 executor round of head-scratching + one trial-and-error error handler.

### Suggested skill changes
- Add a line to `references/agent-briefs/planner.md` under "SDK-wrapping sub-items": "Request a context7 docs check during planning to verify the SDK's actual export list and API shape before drafting ACs."

### Metrics
- Review iterations: 2 (1 fix round + 1 verification)
- PR-fix iterations: 0 (Phase 6 not yet run)
- Operator interventions outside of gates: 0
- Total wall time so far: ~25 min from /loop tick

---

## 2026-05-06 — M5.3.c.c.c.a: Wire complete + countTokens + reportCost JSON-RPC methods with provider injection

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4

### What worked

Auto mode under `/loop /rdd --auto resume` ran cleanly through Phases 1→7a without intervention. The decomposition decision at Phase 1 was sound: streaming protocol IS its own concern (multi-event notification framing + cancellation semantics), and attempting to fit both synchronous and streaming wiring into a single PR would have stressed review headroom. Phase 4's bounded loop caught AC2 wire-shape drift on iteration 1 — that loop exists for exactly this kind of catch.

### What wasted effort

TASK-drafting AC2 enumerated the wire shape as a closed list but omitted `errorMessage?` from the Go `CompleteResponse` contract. Cost: 1 fixer iteration (5 lines of source JSDoc + 2 vitest cases). Preventable at TASK-drafting time by deriving the AC wire-shape field-by-field from the Go source with explicit `optional` annotations. Suggest adding a preflight Check 3 augmentation to `references/task-template.md`: "For each wire-shape AC, list the source-of-truth file and confirm field-by-field parity before brainstorm closure."

### Suggested skill changes

- Tighten `references/task-template.md` Phase 2 brainstorm guidance: add a step "for each wire-shape AC, enumerate fields from the source-of-truth interface and mark optional fields explicitly."
- Add a companion note to `references/agent-briefs/planner.md`: "When decomposing a feature that wraps a foreign interface (e.g., Go contract), prioritize decomposition that isolates interface-wiring concerns into dedicated sub-items; this eases per-item brainstorm rigor and reduces fixer iterations."

### Metrics

- Review iterations: 2 (1 fix + 1 verification)
- PR-fix iterations: 0 (Phase 6 not yet run)
- Operator interventions outside of gates: 1 (manual git push retry after transient HTTP2 framing error on decomposition commit `48c4c30`)
- Total wall time from /loop tick to Phase 5a: ~30 min

---
## 2026-05-06 — M5.3.c.c.c.b.a: Add JSON-RPC notification builder + inject shared stdout writer into LLM method wiring

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked
Auto-mode under `/loop /rdd --auto resume` ran cleanly through Phases 1–4. Phase 4 converged on iteration 1. The executor brief's "do NOT use the writer in any of the existing three handlers" was clear enough that scope creep didn't appear. The `harness/ready` boot notification gave the TASK feature-shape and avoided the toggle-only-PR gray zone for an infrastructure leaf.

### What wasted effort
(a) The planner's first sub-item title "Add JSON-RPC notification builder + inject shared stdout writer" assumed the notification builder didn't exist. Reading `jsonrpc.ts` at TASK-drafting time discovered it already existed from M5.3.a. Cost: minor scope re-framing during brainstorm. Worth a planner-brief tweak: "When assessing 'Add X' sub-items, grep the codebase for X before proposing it as work." (b) The Phase 5a writer dispatch was TRUNCATED after the lesson append — only 1 of 4 actions completed. The orchestrator detected the partial state via `git status` (showed `M docs/lessons/M5.md` unstaged + no commit) and re-dispatched a continuation. No data loss — the lesson append rides along in the second dispatch's commit. Worth a `references/agent-briefs/writer.md` note: "if the agent is dispatched as a CONTINUATION (lesson already appended), do NOT touch `docs/lessons/<milestone>.md`; just complete the FEEDBACK + ROADMAP + commit." Adds a few lines to the brief, eliminates a class of double-append bugs.

### Suggested skill changes
- Add planner-brief grep step: "When assessing 'Add X' sub-items, grep the codebase for X before proposing it as work."
- Add writer-brief continuation handling: "If dispatched as a CONTINUATION (lesson already appended), do NOT touch `docs/lessons/<milestone>.md`; just complete the FEEDBACK + ROADMAP + commit."

### Metrics
- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: ~15 min (including truncation recovery)

---

## 2026-05-06 — M5.3.c.c.c.b.b: Implement stream + stream/cancel JSON-RPC methods with stream registry and multi-event notification protocol

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 iter 1, 4 iter 2

### What worked

Auto-mode under `/loop /rdd --auto resume` ran cleanly through both iterations. Phase 4 bounded loop caught two real defects on iter 1: (1) concurrency test serialization issue — the test "passed" but never exercised actual interleaving because the synchronous fake completed both streams before either promise resolved; (2) registry cleanup missing on dispatch-loop exception — AC8 explicit text discovered the cleanup path only when a reviewer re-read the spec. Both defects were implementation mistakes, not spec issues. The decomposition (fits one PR) was correct — no late split forced. The 5-level cascade was identified upfront so the writer's job at toggle time was deterministic.

### What wasted effort

Executor flagged an AC11 ambiguity: whether updating `registry.size` assertion in `llm-notification-plumbing.test.ts` (from 3→5) was owned by this TASK or not. Reasonable interpretation; no harm; but worth tightening future TASK templates. When an AC authorizes modification of an existing test file, list EXACTLY which assertions inside that file are owned by the TASK to remove ambiguity.

### Suggested skill changes

- `references/agent-briefs/executor.md`: add a note for streaming/async TASKs — "when testing concurrent behavior against a synchronous fake, capture handler callbacks via `vi.spyOn` and drive interleaved manually; sequential `await fake.method(...)` calls do NOT exercise concurrency."
- TASK template: when an AC authorizes modification of an existing test file, specify EXACTLY which assertions are owned by the TASK (e.g., "AC9: update ONLY these assertions in llm-notification-plumbing.test.ts: Line 45 (registry.size: 3→5), Line 60 (capabilities array contents)").

### Metrics

- Review iterations: 2 (1 fix + 1 verification)
- PR-fix iterations: 0 (Phase 6 not yet run)
- Operator interventions outside of gates: 0
- Total wall time from /loop tick to Phase 5a completion: ~30 min

---

## 2026-05-06 — M5.4.a: Sandbox guardrails — wall-clock timeout + output-byte cap

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 (iter 1: 1 important, 3 nits; iter 2: clean)

### What worked

Auto-mode under `/loop /rdd --auto resume` ran cleanly through Phase 4 iterations. Phase 4 iter 1 caught a real test-coverage gap: the zero-config test passed but lacked goroutine-delta assertion (Test plan row 9 explicit requirement). The kind of "test passes but doesn't pin the invariant" gap that signals incomplete AC mapping. Fixer cycle was tight (+8 LOC, one commit). Decomposition (M5.4.a timer-only vs M5.4.b rlimits) was the right call — the syscall-based path will benefit from dedicated review, and this PR shipped without platform-specific complexity.

### What wasted effort

(a) TASK projected ≤500 LOC; executor delivered 673 LOC, mostly godoc density (30% comment ratio) matching the existing package style. Go-side TASK budgets in comment-heavy codebases should plan +50% over pure-source estimate. (b) `cmd.Start()` failure path lands as `TermReasonNatural` with `ExitCode: -1` — semantically outside the AC3 closed set. Should have been caught at TASK-drafting time as an explicit carve-out or fifth constant.

### Suggested skill changes

- `references/agent-briefs/planner.md`: Go-side TASKs in comment-heavy codebases (>30% comment ratio): project LOC budget +50% for godoc density.
- TASK template: when defining closed-set discrimination (e.g. `TermReason` string union), AC list should enumerate corner cases (Start-failure, partial-completion) rather than assume the closed set covers all paths.

### Metrics

- Review iterations: 2 (1 fix + 1 verification)
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /loop tick to Phase 5a writer dispatch: ~25 min

---

## 2026-05-06 — M5.4.b: Sandbox guardrails — CPU-time + memory-ceiling rlimits

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 2 (iter 1: 1 blocker + 3 important; iter 2: converged)

### What worked

Phase 4 caught the `t.Parallel()` + `t.Setenv` panic via code review BEFORE Linux CI crashed — exactly the bounded-loop's purpose for cross-platform tests where the dev machine (Darwin) doesn't exercise the failure path. The decomposition decision in M5.4 (timer-only / rlimit syscalls) paid off: this PR's risk profile (syscalls + platform divergence) was concentrated and reviewable. Build-tagged file split kept the logic isolated and easy to verify per-platform.

### What wasted effort

(a) The TASK referenced `core/go.mod` and `cd core && ...` paths that don't exist in this single-module repo (go.mod at root). Caused minor confusion; documented in Progress log. Future Go-side TASKs in this repo: verify go.mod location at TASK draft time. (b) The `coder/websocket` dep flip from indirect→direct came in via `go mod tidy` and got carried in the diff as scope creep. Caught at review. Future executor briefs: explicit instruction to revert `go mod tidy` flips not directly caused by the TASK's own imports.

### Suggested skill changes

- TASK template: when scoping a Go-side TASK, add a verification step — "confirm go.mod location and module structure before drafting AC paths".
- Executor brief: add anti-pattern — "after `go mod tidy`, diff `go.mod` and revert any indirect→direct flips not caused by your own new imports".

### Metrics

- Review iterations: 2 (1 fix + 1 verification)
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /loop tick to Phase 5a completion: ~30 min

---
## 2026-05-06 — M5.5.a: Harness boot fetches Manifest via keepclient and templates personality/language into system prompt

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

Decomposition of M5.5 into atomic leaves (M5.5.a/b/c/d) paid off immediately — M5.5.a landed as ~282 LOC (109 + 173), single concern, zero nits at Phase 4 iter 1. The 60s `/loop` dynamic-mode tick cadence (short enough to catch context-switch costs, long enough to complete each phase end-to-end without thrashing) worked smoothly: Phase 1→4 completed in 5–7 ticks with no escalation or operator intervention outside gates.

### What wasted effort

None. Phase 3 executor delivered clean code matching all ACs; Phase 4 reviewer found zero issues (blocker=0, important=0, nit=0). Minimal coordination overhead.

### Suggested skill changes

- None for this TASK. Continuing to monitor tick-cadence effectiveness in Phase 5 (next TASKs in M5 family will refine further if needed).

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from Phase 1 to Phase 5a dispatch: ~22 min

---

## 2026-05-07 — M5.5.b.a: Decode Manifest toolset jsonb and enforce ACLs at harness InvokeTool gate

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 (iter 1 + iter 2)

### What worked

Phase 4 autonomous /loop mode (iter 1 + fixer dispatch) converged cleanly in ~6 minutes: fixer dispatch in tick N, reviewer dispatch in tick N+1, both at 60s cadence. The code-reviewer severity rule (nit vs important) held under autonomous run — one "Critical" CodeRabbit suggestion classified as nit and held; still tracked in Follow-up.

### What wasted effort

Phase 4 iter 1 ESM spyOn trap: `vi.spyOn(invokeToolModule, "runIsolatedJs")` does not intercept in-module lexical calls, so the test's `.not.toHaveBeenCalled()` was trivially satisfied. Required iter 2 dispatch to replace with structural proof (BROKEN_SOURCE + error-code divergence). Add a "structural vs behavioral witness" example to `references/agent-briefs/code-reviewer.md` so the next TS mocking round flags this faster.

### Suggested skill changes

- Add ESM spyOn caveat + structural-witness pattern to `references/agent-briefs/code-reviewer.md` §Test review so future code-reviewer instances catch lexical-call mock misses.

### Metrics

- Review iterations: 2
- PR-fix iterations: 0 (fixer dispatch in Phase 4, not Phase 6)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to Phase 4 convergence: 00:33

---

## 2026-05-07 — M5.5.b.b.a: Add manifest_version.model column + server response projection

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 (iter 1 + iter 2 fixer)

### What worked

Autonomous /loop at 60s cadence (tick 24 build → 25 review → 26 fixer → 27 review) converged cleanly in two iterations. Fixer dispatch in Phase 4 is the right pattern: code-reviewer flags importants, executor fixes, reviewer re-runs — all in one bounded loop without operator escalation. Wire-schema-first decomposition (M5.5.b.b.a/b/c) added one extra cycle but prevented an over-large PR; review clarity and rollback ergonomics are far better as a result.

### What wasted effort

Phase 4 iter 1 nil-arg trap: six nullable args in the handler meant "some arg is nil" passed vacuously without verifying `model` actually wired. SQL-shape assertion (missing in iter 1) is the cheapest regression guard. Plan-step split (one GET test, three write tests) was not honoured in iter 1, requiring a fixer iteration. Cost is minimal but preventable with explicit example in the executor brief showing split-by-path test layouts.

### Suggested skill changes

- Add example to `references/agent-briefs/executor.md` showing test file split (GET-path tests in `handlers_read_test.go`, PUT/POST in `handlers_write_test.go`) when plan specifies one.
- Add SQL-shape assertion pattern example (e.g., `strings.Contains(gotSQL, "model")`) to reviewer brief so column-list regressions are caught in iter 1.

### Metrics

- Review iterations: 2
- PR-fix iterations: 1 (fixer, not Phase 6)
- Operator interventions outside of gates: 0
- Total wall time from Phase 1 to Phase 4 convergence: ~23 min

---

## 2026-05-07 — M5.5.b.b.b: Extend keepclient.ManifestVersion with Model field + decoder tests

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

First-iteration Phase 4 convergence on ~200 LOC client-surface PRs. M5.5.b.b.b mirrors a recently-merged sibling (M5.5.b.b.a server schema), so the contract is already validated upstream. Review found only 2 nits (doc-comment wording, import style); zero behavioural defects. Schema-first decomposition (server → client struct → loader projection) keeps scope crisp per PR.

### What wasted effort

None. Phase 1+2 folded into a single planner tick because the planner enumerated entry files, test references, and sibling context inline; this saved one tick on a straightforward "mirror X just landed" sub-item. Total iterations: planner → executor → reviewer → writer. No retries.

### Suggested skill changes

None. Process scaled well to a small, focused mirror PR.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending — Phase 5b+

---

## 2026-05-07 — M5.5.b.b.c: Project Model via manifest loader into LLMProvider boot config

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

Phase 1+2 fold-in for a re-decomposed leaf. M5.5.b.b.c's planner verdict (`fits` immediately, no blockers — both siblings merged) and explore enumeration (all needed file:line refs in one pass) converged in ~1 minute. No need to split Phase 1 and Phase 2 across ticks. Pattern: when a leaf is the third decomposition of a labeled M-id and prior two siblings just merged, planner+explore consumes ~20 KB tokens and ~1 min agent time.

### What wasted effort

None. Phase 4 iter 1 reviewer noticed the loader's struct initialiser walks fields in `runtime.Manifest` declaration order — a gratuitous-but-cheap consistency check preventing diff-noise on future field additions. Worth promoting into executor brief as a "preserve field order" rubric line.

### Suggested skill changes

- Add "preserve struct-field initialization order matching declaration order" as an explicit rubric line in `references/agent-briefs/executor.md` §Go code style for field initializers, with an example (manifest initialiser walking AgentID/SystemPrompt/Personality/Language/Model/Toolset in struct order).

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending — Phase 5b+

---

## 2026-05-07 — M5.5.b.c.a: Add manifest_version.autonomy column + server PUT/GET projection

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

Phase 1+2 folded into single tick: precedent M5.5.b.b.a provided every file:line reference, enum-via-CHECK pattern, and test-scaffold shape. Planner correctly identified the near-clone structure; executor consumed the spec without ambiguity. Zero-iteration executor run → converged Phase 4 review on first iteration with 0 blockers, 0 important items.

### What wasted effort

None. Review loop was clean.

### Suggested skill changes

Estimation undershot wire-field cost. M5.5.b.c.a landed at 384 LOC (24% over target 320) due to mechanical test-scaffold reindexing. Wire-shape extensions on `manifest_version` consistently cost ~350-400 LOC once test index cascades are counted. Suggest retargeting TASK-size estimates from 280-320 → 350-400 for future `manifest_version` wire-field adds.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending (Phase 5a in progress)

---
## 2026-05-07 — M5.5.b.c.b: Extend keepclient.ManifestVersion with Autonomy field + tests

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: 4 (Phase 4 iteration 1)

### What worked

Folding Phase 1 + 2 on near-clones continues to pay off: the precedent (M5.5.b.b.b, commit `2612b2c`) gave every file:line anchor, so planner skipped straight to executor with seven acceptance criteria and a seven-step plan. Executor converged in two TDD iterations; Phase 4 reviewer caught the aggregator-test gap (AC6a) immediately, and the one-line fixer was dispositive. Cost-benefit on the fixer iteration is right: 3 ticks total (iter 1 → fixer → iter 2 converged).

### What wasted effort

None. The bounded-loop severity contract held: the `important` flagged in Phase 4 iteration 1 was a genuine semantic gap (omitted-key slice missing `"autonomy"` at line 111), and the 3 nits correctly deferred to follow-up.

### Suggested skill changes

- None at this time. Cascade the "aggregator-test AC gap" pattern from this TASK into the executor brief for future wire-field PRs: add a checkbox-style verification step to verify aggregator test covers the field and add to assertion slice if missing.

### Metrics

- Review iterations: 2 (iter 1 flagged important, iter 2 converged)
- PR-fix iterations: 1 (single-line fixer)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to converge: approximately 14 minutes

---

## 2026-05-07 — M5.5.b.c.c.a: Loader projection for AuthorityMatrix + Autonomy

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

Phase 1+2 fold-in continues to scale to deep decompositions: M5.5.b.c.c.a sits at depth 5 (M5→5.5→5.5.b→5.5.b.c→5.5.b.c.c→5.5.b.c.c.a) and the planner+TASK-write fold-in still works because precedent reference (M5.5.b.b.c) provides a complete blueprint. Executor converged iteration 1; Phase 4 reviewer verified all 7 ACs and noted only 1 nit (doc-comment line-cite precision). Clean precedent inheritance makes deep branches tractable.

### What wasted effort

None. Review converged iteration 1.

### Suggested skill changes

- None at this time. Cumulative session metric: 9 cycles closed in this session (PRs #71–77 + this one + impending M5.5.b.c.c.b). Consistent ~9-tick cycle when Phase 4 converges iteration 1; ~14 ticks when iter 2 required. Pattern is stable.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to converge: approximately 9 minutes

---

## 2026-05-07 — M5.5.b.c.c.b: Runtime authority/autonomy enforcement at approval gate

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

Phase 1+2 folded scope verification into planner's inline reasoning without separate explore step. Design question (where does the gate sit? what's the API signature?) resolved in one tick via `inspect the repo briefly` on the prior commit (M5.5.b.c.c.a). Executor converged iter 1 with clean diff and full truth-table coverage. Code-reviewer iter 1 produced 3 minor nits deferred to follow-up (test duplication, switch vs early-return readability, godoc table fragility). No blocker or important items; all 7 ACs verified line-by-line. Session now at 10th sub-item closed under continuous `/loop /rdd --auto resume` — small-scope mirror-pattern PRs converge on iter 1 (~9 ticks); design-introduction PRs (this + M5.5.b.b.a) converge on iter 1 with 3-4 nits; cross-layer wire-up PRs (M5.5.b.a) needed iter 2 (~14 ticks).

### What wasted effort

None. Planner's decomposition rationale (no consumer integration; library API only) was sound and executor respected it perfectly. No operator override needed.

### Suggested skill changes

- None at this time. Planner's `inspect the repo briefly` technique for design-question resolution at Phase 1+2 scales well beyond pure mirror PRs.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending (Phase 5b+)

---

## 2026-05-07 — M5.5.c.a: Add manifest_version columns notebook_top_k + notebook_relevance_threshold + server projection

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

`--auto resume` after a previous tick scaffolded the migration but did not commit — resume mode detected the untracked file as in-flight Phase 3 work and executor folded it cleanly into `b75b5c3`. State recovery via TASK progress log + branch name matched correctly. Phase 4 converged in 1 iteration because the diff is a close clone of M5.5.b.b.a/b.c.a precedent — the reviewer brief's "consistency with established pattern" check did its job in one pass.

### What wasted effort

Preflight Check 4 ("clean tree") strictly interpreted would have halted on the untracked migration scaffold. The orchestrator made the right resume-mode interpretation (in-progress executor scaffold ≠ dirty) but a tighter skill rule would prevent deadlock: in resume mode, untracked files inside the TASK plan's expected directories should be treated as in-flight Phase 3 rather than blocking preflight.

### Suggested skill changes

- In `references/preflight.md` Check 4, add a resume-mode exception: when the current branch matches a resumable `rdd/*` and every untracked file is inside the TASK plan's expected paths, treat as in-flight Phase 3 instead of failing.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0 (Phase 6 not yet run — pending after PR open)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

---
## 2026-05-07 — M5.5.c.b: Extend keepclient.ManifestVersion with NotebookTopK / NotebookRelevanceThreshold + loader projection

**PR**: pending
**Phases with incidents**: none

### What worked

After M5.5.c.a closed and `--auto resume` re-entered on `main` with no in-progress TASK, the orchestrator correctly interpreted this as Phase 1 fresh-pick (clean tree on `main`, preflight Check 5 satisfied) and selected the unique top-down first leaf with met deps (M5.5.c.b). The lessons file `docs/lessons/M5.md` had four near-clone precedent entries (M5.5.b.b.b/c.b/b.c/c.c.a) plus the just-written M5.5.c.a entry—the orchestrator wrote the TASK file directly without dispatching `explore`, reducing one agent hop. Phase 4 converged iter 1 because the diff is a 5th repetition of an established pattern.

### What wasted effort

Minimal. The `--auto resume` semantics are mildly under-specified for the case "TASK closed on tick N, tick N+1 should pick fresh"; SKILL.md says strict resume halts when no TASK is in progress. The orchestrator interpreted the loop intent as "continue ROADMAP" and proceeded with Phase 1 fresh-pick. Worth tightening.

### Suggested skill changes

- In SKILL.md §State recovery, clarify that `/rdd --auto resume` after a TASK closes auto-falls-through to Phase 1 fresh-pick when on `main` and ROADMAP candidates remain. The current strict text "If no such file exists, tell the operator and exit" applies to interactive `/rdd resume` but should be relaxed for `--auto resume` in `/loop`.
- In `references/agent-briefs/code-reviewer.md`, consider adding "doc-comment / validator range asymmetry" as a recognized nit-class, so the reviewer's rationale can cite a stable category instead of free-form prose.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0 (Phase 6 pending after PR open)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

---

## 2026-05-07 — M5.5.c.c: Open per-agent Notebook on harness boot; close on terminate

**PR**: pending
**Phases with incidents**: 3

### What worked

The planner correctly reframed the ROADMAP title's "harness boot" as Go-side runtime lifecycle rather than TS-side notebook code. Without that reinterpretation, the executor would have hunted for non-existent TS-side notebook integration and stalled. First non-clone PR in the M5.5.c stream — design + TDD per AC produced clean code that converged Phase 4 in 1 iteration with zero comments.

### What wasted effort

Phase 3 executor exited silently on its first turn with the message "The race test is still running. Let me wait for it to finish." but did not actually wait, just returned. The orchestrator caught this via Hard rule 5 reminder hooks and recovered by sending a follow-up SendMessage to the same agentId requesting the full report. The recovery cost: one extra agent turn (~90s) and one orchestrator action. The premature exit is structurally similar to the silent-exit class documented in FEEDBACK 2026-04-22/2026-05-05 but with a twist: the agent ITSELF did the silent exit (not the orchestrator). Hard rule 5 hooks fire on Agent return, so recovery worked; but an orchestrator-side defense for "agent returned without the required final-report fields" would tighten the loop.

### Suggested skill changes

- In `references/agent-briefs/executor.md` §"Mode — build (Phase 3) Required final report", strengthen the wording: "If a long-running command is in flight (e.g. `-race` test that may take >60s), wait for it before returning. Do NOT return with 'still running' as the final message — the orchestrator interprets a return as 'phase complete'."
- Optionally: add an orchestrator-side check that the executor's return body contains the required structured fields (commit SHAs, exit codes, LOC totals) and re-prompts via SendMessage if missing.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0 (Phase 6 pending)
- Operator interventions outside of gates: 0 (orchestrator self-recovered the executor silent-exit)
- Total wall time from /rdd to merge: pending

---
## 2026-05-07 — M5.5.c.d.a: Introduce llm.EmbeddingProvider seam (interface + in-process fake) for per-turn query embedding

**PR**: pending
**Phases with incidents**: Phase 4 iter 1

### What worked

Decomposition at Gate 1 was clean. The planner caught that M5.5.c.d's bundled scope ("auto-recall + inject") required a non-existent embedding seam — instead of forcing a too-large PR, returned `fits: false` with a 2-way decomposition. ROADMAP edit + commit on main per `references/roadmap-migration.md` §"Decomposition at Gate 1" + auto-yes on the FIRST decomposed sub-item per `references/gates.md` §"Gate 1 auto-decision" landed cleanly.

The 2 iter-1 important items (AC1 constant re-export + precedence test) were genuine AC-compliance findings, not over-strict review noise. The fixer commit addressed both in one focused diff. Phase 4 converged in 3 iterations (initial + fixer + re-review) without escalation.

### What wasted effort

The TASK's AC1 wording "re-exported from notebook (OR imported by callers)" was ambiguous — the executor read it as "OR imported by callers" satisfied via the test file's `notebook.EmbeddingDim` reference, but the reviewer read it as requiring a local handle in the llm package. Tighter AC wording ("MUST add `const EmbeddingDim = notebook.EmbeddingDim` in `embedding_provider.go`") would have avoided the iter-1 round-trip.

### Suggested skill changes

- In `references/task-template.md` or `references/agent-briefs/planner.md`, add an AC-precision rule: when an AC offers a choice ("X OR Y"), the executor will pick whichever is cheapest, which may differ from reviewer expectations. Prefer concrete single-path ACs ("MUST do X") and let the executor propose alternatives in the report if X is infeasible.

### Metrics

- Review iterations: 3 (initial + fixer + re-review)
- PR-fix iterations: 0 (Phase 6 pending)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

---

## 2026-05-07 — M5.5.c.d.b.a: Add llm.WithRecalledMemory option + RecalledMemory shape + injection into System slot

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked

The second decomposition pass at Gate 1 (M5.5.c.d.b → b.a + b.b) was clean. `planner` correctly identified that "WithRecalledMemory option" + "manifest-aware turn helper with fail-soft matrix" are two coupled-but-distinct concerns — splitting kept each PR small and reviewable. ROADMAP edit + commit on main per `references/roadmap-migration.md` §"Decomposition at Gate 1" landed cleanly (commit `78edafc`).

First Phase-4-iter-1 zero-comment converge in the M5.5.c.d branch since M5.5.c.c. The option mirrors the established `WithMaxTokens` / `WithMetadata` pattern verbatim — reviewer brief's "consistency with established pattern" check did its job in one pass.

### What wasted effort

The second-tier Edit-after-merge race repeated: writer agent on M5.5.c.d.a's squash merge updated the ROADMAP file on `main`, so the orchestrator's first Edit attempt for M5.5.c.d.b decomposition failed with "File has been modified since read". Reproduced from M5.5.c.b/c → c.d sequence. Fix: orchestrator should Read ROADMAP file just-in-time before applying decomposition Edits, especially right after a Phase 7a merge that may have updated it.

### Suggested skill changes

- In `references/roadmap-migration.md` §"Decomposition at Gate 1", add an explicit "always Read ROADMAP at HEAD just before the Edit" reminder so future orchestrators avoid the post-merge file-state-drift retry.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0 (Phase 6 pending)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

---

## M5.5.c.d.b.b (2026-05-07)

**Phase 4 iter 1**: 0 blocker / 3 important / 4 nit. Important: (1) negative TopK falls through to recall instead of disabled — fix `<=0`; (2) godoc claims `NotebookRelevanceThreshold` consumed but unused — implement post-filter; (3) nil-embedder branch missing test. Iter 2 fixer (5388b61) addressed all 3. Iter 3 re-review: 0/0/0 converged.

**What worked**: Two-tier decomposition (M5.5.c.d → b.a/b.b) was clean. Each PR converged in 1–3 iterations. Reviewer iter 1 caught real contract drift (`NotebookRelevanceThreshold` claimed but unused) plus semantic defect (`==0` not `<=0`) — both undetected by 9 tests because fixtures didn't exercise negative-TopK/non-zero-threshold edges. High-value review confirms bounded-loop ceiling of 5 iterations is right shape.

**What wasted effort**: Iter-1 important #2 (contract drift on `NotebookRelevanceThreshold`) caused by ambiguous TASK AC. TASK said "calls notebook.DB.Recall(topK, threshold)" but actual API has no threshold parameter — executor inherited planner wording verbatim. Planner-level audit ("which method-name + parameter list does the task ACTUALLY call?") would have surfaced drift pre-TDD. Cost: one fixer pass + 150 LOC of threshold post-filter test scaffolding.

**Suggested process change**: In planner-phase verification, for any sub-item claiming to call downstream API with N parameters, verify the API actually accepts N parameters (not N-1 or N+1). Ambiguity pre-Gate-1 saves a Phase-4 cycle.

**Metrics**: Review iterations: 3. PR-fix iterations: 0. Operator interventions outside gates: 0. Executor commits: 91d1c0c (original) + 5388b61 (fixer). Final: 3 files / +749 LOC.


## 2026-05-07 — M5.5.d.a.a: bidirectional NDJSON JSON-RPC framing over stdio

**PR**: [pending merge]
**Phases with incidents**: Phase 4 iter 1 (1 blocker + 3 important); Phase 4 iter 2 fixer (all cleared)

### What worked

**Tier-2 decomposition pattern cascaded correctly.** Planner returned `fits: false` twice: M5.5.d → c.d.a/b/c (M5.5.d too-large), then M5.5.d.a → a.a/a.b (M5.5.d.a too-large). Each decomposition was crisp; auto-rule "auto-yes on FIRST decomposed sub-item" cascaded through both layers, landing on M5.5.d.a.a (fits+buildable, 1383 LOC). Without the second pass, executor would have committed to a 1200+ LOC PR instead.

**Reviewer caught a genuine race.** Phase 4 iter 1 blocker was real: `-race -count=10` reproduced 7-of-8 timeouts. Phase 3's `pnpm test --run` + `go test -race` (default count=1) didn't trip it. Reviewer's `-count=10` test-suggestion made the flaky race deterministic; fixer's atomic-write fix made it pass deterministically. **Demonstrates value of Phase 4 being more aggressive than Phase 3.**

**First cross-language PR through one fixer iteration.** Blocker + 2 important items fixed in one commit (`de956ec`). Iter 3 re-review converged clean.

### What wasted effort

**AC8 LOC cap on cross-language seam was tighter than realistic.** Target 700 / cap 1000 assumed one-language deltas. Reality: cross-language parser+errors+tests double surface (TS `RpcClient`+tests, Go `Host`+tests+integration test). The cap should have been ≥1500 OR LOC accounting should exclude first-cross-language-seam structural cost.

### Suggested skill changes

- In `references/agent-briefs/planner.md` or `references/task-template.md`: add AC-precision rule — when the TASK introduces a **NEW cross-language seam** (TS+Go file pairs, e.g., JSON-RPC method on both sides), AC8's LOC cap should be **≤1500** (not 1000) to account for inherent surface doubling. File cap stays **≤6**.
- In `references/agent-briefs/executor.md`: recommend **`go test -race -count=10`** (not default count=1) for any TASK whose ACs include "concurrent" or "race" semantics. Catches flakes in Phase 3 instead of Phase 4.

### Metrics

- Review iterations: 3 (initial → fixer → re-review)
- PR-fix iterations: 0 (Phase 6 pending)
- Operator interventions: 0
- Files touched: 5 (TS: 2, Go: 3) — ≤6 target met
- Final diff: +1383 LOC (38% over 1000-LOC target; acceptable for foundational cross-language seam)


## M5.5.d.a.b — Wire notebook.remember over the M5.5.d.a.a bridge (2026-05-07)

### Review Iterations
Phase 4 iter 1: 1 blocker + 4 important + 5 nit (heavier than typical Phase 4 round but all caught real defects).
Phase 4 iter 2: fixer addressed all 5 in one commit; zero new defects in iter 3 re-review.

### What Worked
**Per-method delta on fresh seam was small as expected**: M5.5.d.a.a's framing PR was 1383 LOC; M5.5.d.a.b's per-method addition is ~720 LOC including extending host.go for the `RPCError.Data` field. Seam reuse model held: a third method should be ~400–500 LOC, no host.go changes if Data field stays sufficient.

**Reviewer iter 1 caught 5 real issues**: 1 blocker (missing re-export, literal AC2 violation) + 4 important (category enum duplicate, RPCError.Data shape, v4-vs-v7 UUID, code-prefix doubling). None were false positives. Structural issues (3 important) + wire-cleanliness (1 important). Phase 4 contract value high.

**Single-commit fixer**: addressed all 5 issues in one commit; all tests green in iter 3. Confirms reviewer's prescriptions were actionable.

### What Wasted Effort
**Initial executor delivered v4-UUID handler and RPCError without Data field** — both called out as design pointers in the TASK, not ACs. Executor didn't treat non-AC pointers as blocking. Future TASKs with critical design pointers should promote them to ACs OR add a Phase 3 checklist requiring executor acknowledgment ("addressed: yes / deferred — reason").

### Metrics
Review iterations: 3 (initial + fixer + re-review); PR-fix iterations: 0 (pending Phase 6 merge); operator interventions outside gates: 0; total wall time from /rdd to merge: pending.


## 2026-05-07 — M5.5.d.b: Register Remember as a built-in harness tool

**PR**: [#81](https://github.com/vadimtrunov/watchkeepers/pull/81)
**Phases with incidents**: Phase 4 iter 1 (1 important — out-of-scope wiring caught without test).

### What worked

**Per-method dispatch on the seam was small and clean**: the ACL gate from M5.5.b.a continues to apply unchanged for builtins. No security regression. ~600 LOC for a new tool kind + first builtin + bidirectional wiring is acceptable for a single PR.

**Reviewer caught the out-of-scope wiring's missing test**: the executor wired `looksLikeResponse` into `runHarness` because `RpcClient` had to land somewhere, but didn't write a focused unit test for the classifier. iter 1 caught it; iter 2 fixer extracted the function into a unit-testable export and added a 6-case table test. Phase 4 reviewer's value-add is highest when the executor lands necessary side-effects without recognizing them as testable surface.

### What wasted effort

**Out-of-scope wiring decision was implicit**: M5.5.d.a.a's TASK Scope explicitly deferred `runHarness` integration to M6 ("This PR introduces the package but does NOT have to wire it into runtime/runtime.go — the package can be tested standalone via in-memory pipes"). The executor of M5.5.d.b correctly identified that `RpcClient` had to land in `runHarness` to thread it into the dispatcher, but didn't surface this as a TASK-Scope expansion in the executor's report. A clearer "TASK assumes M6 wiring is unbuilt; doing the minimum here" check would have prompted explicit acknowledgement.

### Suggested skill changes

In `references/agent-briefs/executor.md`, recommend: when the executor recognizes that a sibling-deferred integration is needed RIGHT NOW (because dispatch requires it), the executor MUST surface this in their final report as a "scope expansion" line item with rationale. The reviewer can then evaluate whether the expansion is justified or warrants halt-for-decomposition.

---

## 2026-05-07 — M5.5.d.c: manifest projection + end-to-end test

**PR**: [#82](https://github.com/vadimtrunov/watchkeepers/pull/82)
**Phases with incidents**: 0

### What worked

**Pragmatic AC scoping at Gate 2 paid off**: TASK ACs noted that AC1-AC3 might be near-zero LOC if existing tests covered projection/ACL paths. Executor confirmed AC1 fixture was a 28-line addition to existing decodeToolset tests; AC2/AC3 were 85 lines of TS test extension feeding the real `setManifest` handler. Bulk was the e2e test (139 lines). Total 251 LOC vs target 600 — 58% under target. Pre-empting "may collapse to docs-only" in the TASK saved an iteration of executor judgement.

**Phase 4 iter 1 cleanup-correctness check**: reviewer specifically verified `t.Cleanup`/`defer` for the bridge goroutine + DB close + temp dir. This catches the "test passes but leaks goroutines/handles" failure class invisible in CI but bites later in large test suites under `-race`.

### What wasted effort

Minimal — 1 commit, 1 review iteration, 0 fixer. Closest thing to "wasted" was dispatching reviewer for what turned out to be a clean PR; but reviewer iter 1 is the contract.

### Suggested skill changes

In `references/agent-briefs/code-reviewer.md`, recommend (as already practiced informally) that reviewers EXPLICITLY check `t.Cleanup`/`defer` discipline for any test constructing goroutines, opening files/DBs, or setting process-global env. Make the check a numbered focus area, not just implicit reading.

### Metrics

- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

## 2026-05-07 — M5.6.a: Inline-DDL + guarded-ALTER migration for SQLite notebook schema

**PR**: pending
**Phases with incidents**: 3

### What worked
Gate 2 produced tight AC/test-plan decomposition (7 AC, 9 test bullets, 8 step plan). Executor opus finished 4 commits in one continuation (no splits). The 7-AC threshold + opus mapping held. Planner's 6-way decomposition of M5.6 yielded an appropriately-scoped first sub-item: 9 files, 780 LOC, well under hard caps.

### What wasted effort
Phase 3 executor hit a mid-sentence split on a cyclomatic-complexity refactor in a stats helper; SendMessage to the same agent UUID resumed cleanly. Recoverable but worth noting: silent-exit recovery via SendMessage works for partial-progress executor returns, parallel to /loop recovery for Phase-1-through-7 silent exits.

### Suggested skill changes
Document the SendMessage recovery pattern in `references/executor-briefs.md` under "Resuming partial-progress turns". Currently only /loop recovery is called out.

### Metrics
- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from Phase 1 to Phase 4 convergence: 01:45

---
## 2026-05-08 — M5.6.b: Auto-reflect on tool error

**PR**: pending
**Phases with incidents**: 4

### What worked
Gate 2 flushed out the full AC+test matrix upfront, preventing mid-implementation scope creep. The bounded-loop discipline (executor iter 1 → review iter 1 → fixer iter 1 → review iter 2 → fixer iter 2 → review iter 3 converged) caught a scope violation that would have shipped if not for the fresh review pass after each fixer commit.

### What wasted effort
Phase 3 executor turn cut off mid-sentence on a gofumpt formatting fix; SendMessage to the same agent UUID resumed cleanly (same recovery as M5.6.a). Phase 4 iter 1 fixer silently bundled unrelated M5.5.d.b harnessrpc/* hunks into the commit, discovered by reviewer iter 2. The iter-1 commit message advertised only the two intended fixes and did not disclose the harnessrpc edits—scope creep was invisible until diffed.

### Suggested skill changes
- Extend `references/agent-briefs/executor.md` §"Mode — fixer" with a hard rule: "List every touched file in the commit message body; reviewer asserts the list matches the comments-to-fix list before approving." This makes scope creep auditable via `git show --stat` without diffing.
- Confirm that bounded-loop discipline (fresh review after every fixer commit) is non-negotiable, even when prior iterations converged quickly. Scope violations like M5.5.d.b hunks would have shipped without iter 2.

### Metrics
- Review iterations: 3
- PR-fix iterations: 2
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending (Phase 5b opens PR)

---

## 2026-05-08 — M5.6.c: Atomic evidence write with keepers-log Append before Embed

**PR**: pending
**Phases with incidents**: 4 (iterations 1–2)

### What worked

Gate 2 acceptance criteria (8 AC) and test cases (9) were precise enough that Phase 3 executor understood the requirements without clarification. Phase 4's bounded-loop discipline caught the AC2 ordering deviation in iteration 1, surface it to the fixer as "important", and converged in iteration 2 with zero additional blocker/important issues. The `recordingAppender` stub pattern from Phase 3 tests proved reusable in wired_runtime tests without modification.

### What wasted effort

Phase 4 iteration 1 flagged AC2 ordering as "important": the initial implementation deferred `Append` until after `Embed` to preserve backward-compat with M5.6.b's cancelled-ctx test. The test's actual intent (no runtime crash) differs from its implementation (Embed before Append), which meant the deviation was real, not defensive. The executor's deferred-decisions section listed "kept M5.6.b ordering for backward-compat", but did not surface it as a contract deviation upfront. A surface-level read by the reviewer would miss this without opening the diffs. If the executor had promoted the AC2 deviation to the TASK's Progress log as a heads-up, the reviewer's initial round could have triaged it before the fixer loop.

### Suggested skill changes

- Extend `references/agent-briefs/executor.md` §"Mode — build" to require that any AC deviations (especially ordering, atomicity, timing) be promoted to a TASK Progress-log entry as a heads-up bullet, not buried in the deferred-decisions section. Example: "AC2 deviation: kept Embed-before-Append for M5.6.b test backward-compat, but this loses audit rows on ctx cancel. Recommend fixer review."

### Metrics

- Review iterations: 2
- PR-fix iterations: 1 (db427b5 reorder)
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

---
## 2026-05-08 — M5.6.d: Gate auto-injection in BuildTurnRequest by active_after and needs_review

**PR**: pending — to be opened in Phase 5b
**Phases with incidents**: none

### What worked
Phase 3 executor reported deferred AC deviations (silent-swallow on counter failure vs literal AC4 "log via existing logger"; 5 files vs ≤ 4 target) directly in the build report BEFORE review. Phase 4 reviewer accepted both as defensible nits and converged at iteration 1 — exactly the M5.6.c follow-up suggestion. First observable payoff: deferred-deviation surfacing shortened the review loop from 3 iters (M5.6.b, M5.6.c) to 1 iter.

### What wasted effort
None — Phase 4 converged at iteration 1.

### Suggested skill changes
- Extend `references/agent-briefs/executor.md` §"Mode — build" to require deferred AC deviations be promoted to a TASK Progress-log entry as heads-up to Phase 4, documenting this M5.6.d dataset as the first observed payoff from flagging deviations pre-commit.

### Metrics
- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from Phase 3 start to Phase 4 convergence: est. 02:15 (executor + 1-iter review)

---

## 2026-05-08 — M5.6.e.a: Typed-slice migration with backward-compat helper

**PR**: pending — Phase 5b
**Phases with incidents**: none

### What worked
The pattern from M5.6.c (pre-flagging deviations in the TASK Progress-log) continues to deliver. Phase 4 converged at iteration 1 with zero findings — this is the second consecutive TASK where Phase 4 converged cleanly at iter 1 (M5.6.d also did). The pre-flagged deviation surfacing kept the review loop tight.

### What wasted effort
None. Both Phase 3 commits (`1b878da`, `c8d189d`) stayed within stated scope as verified via `git show --stat` per-commit. The executor brief and pre-flagging discipline paid off again.

### Suggested skill changes
Suggest cementing the deferred-deviation-surfacing pattern as a hard rule in the next executor brief (was soft suggestion in M5.6.c, but two consecutive iter-1 convergences prove it works).

### Metrics
- Review iterations: 1
- PR-fix iterations: 0
- Operator interventions outside of gates: 0
- Total wall time from /rdd to merge: pending

---

## 2026-05-08 — M5.6.e.b: Boot-time superseded-lesson scan

**PR**: pending — Phase 5b
**Phases with incidents**: 3

### What worked

Gate 2 captured the AC4 deviation (`WithFlagLogger` vs `WithLogger`) proactively.
The executor's TASK Progress-log pre-documented the shadowing risk and rationale,
so the code-reviewer accepted it without escalation. The pre-flagged deviation
pattern from M5.6.d lesson (documenting non-conformances in TASK Progress-log
early) continues to pay off: zero review friction on a legitimate API choice.

### What wasted effort

Phase 3 silent-exit (executor agent stopped mid-turn without sending a final
message). Orchestrator recovered by reading git state directly and running
tests/lint manually, verifying all ACs were satisfied. The recovery worked
because the executor's commits were coherent and test suite green — silent-exit
is recoverable when the workspace is left in a clean state. No re-prompting was
needed; state inspection from disk sufficed.

### Suggested skill changes

Add a "silent-exit recovery via direct state inspection" idiom to
`references/agent-briefs/executor.md` §"Mode — build" as an alternative to
SendMessage when the agent leaves the workspace in a clean state. Pattern:
(1) check git log for new commits, (2) run tests/lint, (3) if all green, infer
the agent completed its work; no need to wait for a final message.

### Metrics

- Review iterations: 1 (converged immediately)
- PR-fix iterations: 0 (no blocker/important; Follow-up nits deferred)
- Operator interventions outside of gates: 1 (phase 3 silent-exit recovery)
- Total wall time from /rdd to merge: pending Phase 7a

---
