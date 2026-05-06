# Agent Brief — writer (Phase 5a)

Dispatched once at Phase 5a, **after the pre-PR review loop converged
and before the PR is opened**. The writer commits its appends to the
**feature branch** `rdd/<slug>` — not to `main`. The squash-merge in
Phase 7a folds the lesson + ROADMAP toggle into the same single commit
on `main`. There is no separate follow-up commit on `main`.

**Why this slot (not post-CI):** Pushing the writer's text-only commit
AFTER Phase 6 converged retriggered CI for no functional reason
(lessons/ROADMAP/FEEDBACK don't affect tests). Running the writer
BEFORE the PR is opened means git-master `pr` mode pushes ALL commits
(executor + writer) at once; the single `pull_request:opened` event
then triggers ONE CI run on the full HEAD. Push to a feature branch
without an open PR does not trigger CI — `on: pull_request` fires only
on PR events, not raw branch pushes.

Appends one section to each of:
- `docs/lessons/<milestone>.md` — project-level patterns for the milestone family.
- `.claude/skills/rdd/FEEDBACK.md` — skill self-reflection.

Also performs the **ROADMAP checkbox toggle** (leaf + cascade) per
`references/roadmap-migration.md` §Cascade — moved here from the merge
agent so the toggle lands in the same squash commit and no extra
chore-toggle PR is needed.

## Preferred `subagent_type`

`oh-my-claudecode:writer` (fallback: `general-purpose`).

## Input (pass in the agent prompt)

- **Roadmap id (leaf), title, planned PR URL placeholder, merge timestamp**.
  Since Phase 5a runs BEFORE the PR is opened, use the strings
  "pending — to be opened in Phase 5b" for the PR URL and
  "pending — to be merged in Phase 7a" for the timestamp. The orchestrator
  amends the lesson/FEEDBACK PR-URL placeholders only if it does a
  retro-edit; in practice the writer's appended PR URL line is left as
  the placeholder and the lesson/FEEDBACK file's index/grep can find the
  entry by date + roadmap-id alone. (If the operator strongly prefers a
  filled PR URL, dispatch a second tiny writer-amend pass after Phase 5b
  to in-place rewrite "pending — to be opened in Phase 5b" → the actual
  URL; but this costs an extra commit on the feature branch and another
  CI run, so default OFF.)
- **Branch name** `rdd/<slug>` — the writer commits here. **Writer does
  NOT push** in the new flow; git-master `pr` mode in Phase 5b owns the
  push so all commits arrive on origin together with the `gh pr create`
  call (one CI run total).
- **Target milestone file** — derive from the leaf id per
  `references/lessons-template.md` §"Target file selection". If it does
  not exist, create it from the per-family header in §"File structure
  (first time only — new milestone family)" AND add an index row to
  `docs/LESSONS.md`.
- **TASK file path** — still present at this point; deleted by the
  orchestrator *after* this agent returns.
- **Progress log excerpts** from the TASK file covering each phase up
  through Phase 4 — Phase 6 (PR-fix loop) hasn't run yet. If a Phase 6
  friction worth recording appears later, the operator can append a
  retro-FEEDBACK entry manually after merge — the bigger win from the
  Phase 5a slot is one-CI-run-per-iteration.
- **Templates**:
  - `references/lessons-template.md`
  - `references/feedback-template.md`

## Actions

1. **Append lesson** to `docs/lessons/<milestone>.md` following the exact
   template in `references/lessons-template.md` §"Per-TASK section". Use
   the append-only mechanic from §"Append mechanics (no full-file read)"
   — do NOT Read the whole file. The cheapest path is `cat >> file
   <<'LESSON' ... LESSON` because the writer's only action on this file
   is the append.
2. **Append feedback** to `.claude/skills/rdd/FEEDBACK.md` following the
   exact template in `references/feedback-template.md` §"Per-TASK
   section". Same append-only mechanic.
3. **Toggle ROADMAP checkbox** (leaf + cascade) per
   `references/roadmap-migration.md` §Cascade. Use targeted `Edit`
   operations on the leaf line and any cascading ancestors — do NOT
   rewrite the whole ROADMAP file.
4. **Stage and commit** all three files in one commit on the **feature
   branch**. Do NOT push — git-master `pr` mode in Phase 5b owns the
   push:
   ```bash
   git add docs/lessons/<milestone>.md docs/LESSONS.md .claude/skills/rdd/FEEDBACK.md docs/ROADMAP-*.md
   git commit -m "docs: lessons + roadmap toggle for <roadmap-id>"
   # NO git push — git-master pushes everything in Phase 5b
   ```
   (`docs/LESSONS.md` only changes when adding a new milestone family
   row; if untouched, drop it from `git add`.)

## Hard rules

- Commit on `rdd/<slug>`, **never** on `main`. The squash merge in Phase
  7a is what lands the changes on `main`.
- **Do NOT push.** git-master `pr` mode in Phase 5b is the single push
  point so executor commits + writer commit reach origin together and
  the `gh pr create` event triggers exactly one CI run.
- Use the templates verbatim for structure. Only the content inside each
  section varies.
- Do not edit existing sections of any file; append only (lessons +
  feedback) or targeted-toggle (ROADMAP).
- Do not Read the full lessons or feedback file before appending. Use the
  append mechanic from `lessons-template.md`.
- Keep each section under 50 lines. Trim if needed.
- Never suggest changes to business code in `FEEDBACK.md` (that belongs in
  the milestone lessons file). Never suggest changes to the skill in the
  lessons file (that belongs in `FEEDBACK.md`).
- If you find that you have nothing new to add in either file, still append
  a minimal entry — the running record must be continuous, and the pattern
  of minimal entries is useful signal.

## Report

Back to the orchestrator:
- New commit sha on `rdd/<slug>`.
- Target milestone file path.
- ROADMAP ids flipped (leaf plus any cascaded ancestors).
- Character count of each appended section.
