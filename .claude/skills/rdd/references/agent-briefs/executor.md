# Agent Brief — executor

The executor is dispatched in three modes:
- **build** — Phase 3, fresh implementation of the TASK.
- **fixer** — Phase 4, minimal patches for review comments.
- **pr-fixer** — Phase 6, minimal patches for PR comments and CI failures.

## Preferred `subagent_type`

`oh-my-claudecode:executor` (fallback: `general-purpose`).
Pass `model: opus` when the TASK is marked complex in Gate 2 (operator
judgement) or when the acceptance criteria list ≥ 6 items.

## Common input (all modes)

- **TASK file path** (e.g. `TASK-M1.1-monorepo-layout.md`).
- **Acceptance criteria** and **test plan** copied verbatim from the TASK
  file.
- **Project conventions pointer**: `docs/LESSONS.md` and, if present,
  `.claude/CLAUDE.md`.
- **Branch name** (already checked out by the orchestrator).
- **Hard rules**:
  1. All repo content is English.
  2. TDD: failing test first, minimal code to pass, then refactor. One
     acceptance criterion at a time.
  3. Commit per logical step; commit messages follow conventional-commits
     as configured by the repo (or plain prose if the repo is not yet on
     commitlint).
  4. Never modify `docs/ROADMAP-*.md`, `SKILL.md`, or anything under
     `.claude/skills/rdd/` (the skill itself). Those are orchestrator-only.
  5. Never mark an AC checkbox in the TASK file yourself — the orchestrator
     reads git diff to infer progress.
  6. Do not open a PR; do not push. Phase 5's `git-master` owns that.

## Mode — build (Phase 3)

Additional input:
- **Plan** (ordered steps from the TASK file).

Expected output:
- Commits on the current branch implementing the plan.
- No failing local tests per the configured test command (`make test` if
  Makefile exists, otherwise repo-default).

Report at end:
- List of commit SHAs.
- List of files touched.
- Test command run and its exit code.
- Open questions, if any (do not guess; stop and ask via the report).

## Mode — fixer (Phase 4)

Additional input:
- **Comments to fix** (`blocker + important` list from the pre-PR
  `code-reviewer` run, each with `file`, `line`, `rationale`,
  `suggested_fix`).

Expected output:
- Minimal patches addressing exactly the listed comments. No unrelated
  changes.
- Tests still pass.
- One commit per addressed comment *is acceptable but not required*;
  grouping fixes in one commit is fine if it is coherent.

Report at end:
- Map of comment → commit(s).
- Test command run and its exit code.

## Mode — pr-fixer (Phase 6)

Additional input:
- **PR number.**
- **Failing checks** (name, conclusion, log snippets).
- **Unresolved blocker/important comments.**

Expected output:
- Minimal patches, pushed to the PR branch.
- For each addressed comment, post a reply via:
  ```bash
  gh pr comment <pr> --body "fixed in <sha>"
  ```
  Do **not** resolve the comment thread.
- Tests still pass locally before push.

Report at end:
- Map of comment/check → commit(s) + push sha.
- For each comment you did NOT address, a one-sentence reason (if any
  were consciously deferred).

## Anti-patterns (all modes)

- "While I was there I also cleaned up …" — refuse. Scope discipline.
- Adding new dependencies without a justification line in the commit body.
- Renaming files or symbols unless the TASK plan explicitly calls for it.
- Silent changes to public API surface outside the diff the comments
  addressed.
