# Agent Brief — writer (Phase 7)

Dispatched once at the end of Phase 7, after the merge and the ROADMAP
commit. Appends one section to each of:
- `docs/LESSONS.md` — project-level patterns.
- `.claude/skills/rdd/FEEDBACK.md` — skill self-reflection.

## Preferred `subagent_type`

`oh-my-claudecode:writer` (fallback: `general-purpose`).

## Input (pass in the agent prompt)

- **Roadmap id, title, PR URL, merge timestamp.**
- **Branch name** (for history look-ups if needed).
- **TASK file path** — still present at this point; deleted by the
  orchestrator *after* this agent returns.
- **Progress log excerpts** from the TASK file covering each phase —
  especially Phase 4 and Phase 6 iteration entries, for the FEEDBACK
  section.
- **Templates**:
  - `references/lessons-template.md`
  - `references/feedback-template.md`

## Actions

1. If `docs/LESSONS.md` does not exist, create it with the header from
   `references/lessons-template.md` §"File structure (first time only)".
2. Append a new section to `docs/LESSONS.md` following the exact template
   in `references/lessons-template.md` §"Per-TASK section".
3. Append a new section to `.claude/skills/rdd/FEEDBACK.md` following the
   exact template in `references/feedback-template.md` §"Per-TASK section".
4. Stage and commit both files in one commit:
   ```bash
   git add docs/LESSONS.md .claude/skills/rdd/FEEDBACK.md
   git commit -m "docs: record lessons and feedback from <roadmap-id>"
   git push origin main
   ```

## Hard rules

- Use the templates verbatim for structure. Only the content inside each
  section varies.
- Do not edit existing sections of either file; append only.
- Keep each section under 50 lines. Trim if needed.
- Never suggest changes to business code in `FEEDBACK.md` (that belongs in
  `LESSONS.md`). Never suggest changes to the skill in `LESSONS.md` (that
  belongs in `FEEDBACK.md`).
- If you find that you have nothing new to add in either file, still append
  a minimal entry — the running record must be continuous, and the pattern
  of minimal entries is useful signal.

## Report

Back to the orchestrator:
- New commit sha.
- Character count of each appended section.
