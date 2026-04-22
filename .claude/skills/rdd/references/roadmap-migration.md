# ROADMAP Migration

The skill expects `docs/ROADMAP-*.md` to carry **hierarchical checkboxes**:

- A `[ ]` / `[x]` marker on every milestone heading (e.g. `### M1 — Foundation [ ]`).
- A `[ ]` / `[x]` list item under **Scope** for each atomic sub-item,
  identified by a stable id like `**M1.1**`.

If the file does not yet have these markers, the skill proposes a one-time
migration at Gate 1 and applies it only after the operator approves.

## Trigger

Before Phase 1 can produce a candidate list, the orchestrator checks whether
the ROADMAP already has markers.

Detection heuristic (runs per file, then aggregates):

```bash
# count milestone headings with checkbox markers, portable across GNU/BSD grep
for f in docs/ROADMAP-*.md; do
  marked=$(grep -cE '^### M[0-9]+.*\[( |x)\][[:space:]]*$' "$f")
  total=$(grep -cE '^### M[0-9]+' "$f")
  if [ "$total" -gt 0 ] && [ "$marked" -ne "$total" ]; then
    echo "$f needs migration ($marked/$total marked)"
  fi
done
```

- If no file prints a "needs migration" line — migration is not needed.
- Otherwise — propose migration for each flagged file.

Note: `[[:space:]]` is used instead of `\s` because `\s` is not POSIX ERE;
BSD grep (macOS default) does not treat it reliably as a whitespace class.

## Transformation rules

For each milestone heading `### M# — <title>`:

1. Append ` [ ]` at the end (or ` [x]` if the operator marks it complete during
   the migration dialog).

For each bullet under the milestone's `**Scope**` section that represents an
atomic unit of work (engineering judgement + operator confirmation):

1. Convert the bullet to `- [ ] **M#.k** <rest of the bullet>` with a
   sequential `k`.

Lines that are not atomic sub-items (prose paragraphs, nested explanations,
external dependency notes) stay unchanged.

## Gate 1 dialog

Show the operator the proposed diff (unified `diff` format). Ask:

> `<roadmap-path>` has no progress markers. I propose the migration
> above: `[ ]` on each milestone, and numbered `- [ ] **M#.k**` bullets for
> each atomic sub-item. This will be committed to `main` as one commit
> titled `docs(roadmap): add hierarchical progress checkboxes`. Apply?

(Substitute `<roadmap-path>` with each file flagged by the detection
heuristic above, e.g. `docs/ROADMAP-phase1.md`, `docs/ROADMAP-phase2.md`.)

Only apply on explicit "yes". On "no", stop the skill and tell the operator
the migration is required before any TASK can be picked.

## Application

After approval:

1. Rewrite the ROADMAP file(s).
2. Stage and commit on `main` directly (no PR):

   ```bash
   git add docs/ROADMAP-*.md
   git commit -m "docs(roadmap): add hierarchical progress checkboxes"
   git push origin main
   ```

3. Proceed to Phase 1 with the migrated file.

## Decomposition at Gate 1 (in-flight ROADMAP edit)

If the `planner` agent (see `references/agent-briefs/planner.md`) finds that an atomic
sub-item is still too large, it returns a proposed decomposition. On Gate 1
approval:

1. Replace the original bullet `- [ ] **M#.k** <title>` with a nested block:

   ```markdown
   - [ ] **M#.k** <title>
     - [ ] **M#.k.a** <sub-title-1>
     - [ ] **M#.k.b** <sub-title-2>
   ```

2. Commit on `main`:

   ```bash
   git add docs/ROADMAP-*.md
   git commit -m "docs(roadmap): decompose <M#.k> into sub-items"
   git push origin main
   ```

3. Take `M#.k.a` as the unit of work.

## Cascade at Phase 7

When marking the completed leaf `[x]`:

1. Flip the leaf bullet to `- [x] **M#.k.a.b** ...`.
2. Walk up the hierarchy: if every direct child of `M#.k.a` is `[x]`, flip
   `M#.k.a` to `[x]`. Continue up to `M#.k`, then to `M#`.
3. Stop at the first ancestor that still has an unchecked child.

This generalizes to arbitrary decomposition depth introduced at Gate 1.

## Rollback

If migration or decomposition was committed but the operator wants to revert
before any further work:

```bash
git revert <sha> --no-edit
git push origin main
```

The skill itself never rolls back — this is a manual operator action.
