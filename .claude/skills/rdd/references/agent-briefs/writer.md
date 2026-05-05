# Agent Brief — writer (Phase 7a)

Dispatched once at Phase 7a, **after CI is green and before the merge**.
The writer commits its appends to the **feature branch** `rdd/<slug>` —
not to `main`. The squash-merge in Phase 7b folds the lesson + ROADMAP
toggle into the same single commit on `main`. There is no separate
follow-up commit on `main`.

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

- **Roadmap id (leaf), title, PR URL, merge timestamp** (use "pending —
  to be merged" if Phase 7b has not run yet — the writer runs *before*
  the merge in the new sequence).
- **Branch name** `rdd/<slug>` — the writer commits and pushes here, NOT
  to `main`.
- **Target milestone file** — derive from the leaf id per
  `references/lessons-template.md` §"Target file selection". If it does
  not exist, create it from the per-family header in §"File structure
  (first time only — new milestone family)" AND add an index row to
  `docs/LESSONS.md`.
- **TASK file path** — still present at this point; deleted by the
  orchestrator *after* this agent returns.
- **Progress log excerpts** from the TASK file covering each phase —
  especially Phase 4 and Phase 6 iteration entries, for the FEEDBACK
  section.
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
4. **Stage, commit, push** all three files in one commit on the **feature
   branch**:
   ```bash
   git add docs/lessons/<milestone>.md docs/LESSONS.md .claude/skills/rdd/FEEDBACK.md docs/ROADMAP-*.md
   git commit -m "docs: lessons + roadmap toggle for <roadmap-id>"
   git push origin rdd/<slug>
   ```
   (`docs/LESSONS.md` only changes when adding a new milestone family
   row; if untouched, drop it from `git add`.)

## Hard rules

- Commit on `rdd/<slug>`, **never** on `main`. The squash merge in Phase
  7b is what lands the changes on `main`.
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
