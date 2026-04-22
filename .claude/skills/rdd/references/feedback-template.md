# `FEEDBACK.md` Entry Template

Used by the `writer` agent at Phase 7 to append a new section to
`.claude/skills/rdd/FEEDBACK.md`.

This file is the skill's self-reflection. **Nothing in it is read by the
skill at runtime.** The operator reviews it periodically and manually
promotes useful changes into `SKILL.md` or `references/*`.

## Per-TASK section (append this at the bottom)

Exact template:

```markdown
## <YYYY-MM-DD> — <roadmap-id>: <TASK title>

**PR**: <PR URL>
**Phases with incidents**: <list, or "none">

### What worked
<one-to-two paragraphs: places where the skill's process (gates, bounded
loops, agent briefs, templates) made work easier or faster>

### What wasted effort
<one-to-two paragraphs: where iterations were lost, where the operator had
to step in to unblock, where a brief was ambiguous. Be specific: "the
code-reviewer agent flagged three nits as important because the brief did
not give it examples" is useful; "reviewer was too strict" is not>

### Suggested skill changes
- <file-level suggestion: "tighten severity contract in
  references/bounded-loop.md §Severity to exclude X from 'important'">
- <may be empty>

### Metrics
- Review iterations: <n>
- PR-fix iterations: <n>
- Operator interventions outside of gates: <n>
- Total wall time from /rdd to merge: <HH:MM>

---
```

## Tone rules

- Concrete and blunt. This is an internal log; flattery and hedging are
  noise.
- No suggestions that touch the business code — use `LESSONS.md` for that.
  This file is strictly about the skill itself.
- No "we should refactor the whole X" entries. Suggest one tight change at
  a time; a later operator review promotes or rejects it.

## Example

```markdown
## 2026-04-22 — M1.1: monorepo layout

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/1
**Phases with incidents**: 4

### What worked
Gate 2 flushed out a scope ambiguity that would have burned a review
iteration — operator caught that "Go quality stack" was implicit in "Layout".

### What wasted effort
Phase 4 iteration 1 produced 6 `important` items; 4 of them were really
nits (field ordering in struct literals). The reviewer brief does not
list examples of "important vs nit" so the classification drifted.

### Suggested skill changes
- Add an "important vs nit" example table to
  `references/agent-briefs/code-reviewer.md`.
- Consider raising the iteration budget for the first TASK per milestone
  from 5 to 7 (foundational PRs attract more comments).

### Metrics
- Review iterations: 3
- PR-fix iterations: 1
- Operator interventions outside of gates: 1 (stuck CI cache, manual flush)
- Total wall time from /rdd to merge: 04:37

---
```
