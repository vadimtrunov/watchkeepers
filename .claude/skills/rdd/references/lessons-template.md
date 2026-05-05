# Lessons Entry Template

Used by the `writer` agent at Phase 7a to append a new section to a
**per-milestone** lessons file at `docs/lessons/<milestone>.md`.

`docs/LESSONS.md` itself is the index — never appended to per TASK; it is
edited only when a brand-new milestone family appears.

## Target file selection

Map the TASK's roadmap id to a milestone family file:

| Roadmap id pattern | Target file |
|--------------------|-------------|
| `M2.*` (and `M2 verification ...`) | `docs/lessons/M2.md` |
| `M2b.*` | `docs/lessons/M2b.md` |
| `M3.*` (incl. `M3.5.a.*` security follow-ups) | `docs/lessons/M3.md` |
| `M4.*` | `docs/lessons/M4.md` |
| `M5.*` | `docs/lessons/M5.md` |
| Process / cross-milestone (verification batches, loop behaviour) | `docs/lessons/cross-cutting.md` |
| New milestone family `MN.*` | create `docs/lessons/MN.md` and add index row to `docs/LESSONS.md` |

## File structure (first time only — new milestone family)

If the target file does not exist, create it with this header:

```markdown
# Project Lessons — <milestone>

Patterns and decisions for the <milestone> milestone family of
`docs/ROADMAP-phase1.md` (<one-line scope>).

Appended by the `rdd` skill at Phase 7a when the closed TASK belongs to
<milestone>. Read by the `rdd` skill at the start of Phase 2 when the
next TASK is in the same milestone family.

See `docs/LESSONS.md` for the index across all milestones.

---
```

Then add a new row to the table in `docs/LESSONS.md`.

## Per-TASK section (append this at the bottom)

Exact template:

```markdown
## <YYYY-MM-DD> — <roadmap-id>: <TASK title>

**PR**: <PR URL>
**Merged**: <YYYY-MM-DD HH:MM>

### Context
<one paragraph: what we were solving and why (from the TASK Scope)>

### Pattern
<one-to-three paragraphs: the reusable pattern or decision that emerged from
this TASK — the kind of thing the next brainstorm should consider. Concrete
names, file paths, library versions welcome>

### Anti-pattern
<optional: one paragraph if something was tried and rejected and future work
should avoid repeating it>

### References
- Files: <list of key files introduced or meaningfully modified>
- Docs: <ROADMAP section, ADR if any>

---
```

## Brevity rules

- Keep the whole section under 50 lines. If more, shrink Context and lean on
  References.
- Never copy the TASK file contents verbatim. Summarize.
- No marketing language ("robust", "elegant"). State what was done.
- If nothing new was learned (e.g. pure bugfix), still append a short entry
  so the running record is continuous.

## Append mechanics (no full-file read)

The lessons file may be 10–30 KB. The writer **must not** Read the whole
file before appending. Use one of:

- `Edit` with `old_string` set to the closing `---` of the previous-last
  section plus 1–2 lines of unique context (read with `Read offset/limit`
  to find the anchor — typically the last 30 lines suffice).
- `Bash` heredoc append:
  ```bash
  cat >> docs/lessons/<milestone>.md <<'LESSON'
  ## <new section>
  ...
  LESSON
  ```
  Acceptable for the lesson append because the file is markdown and the
  append is the entire writer action; this is the explicit exception to
  the project rule that prefers the `Write` tool over `cat <<EOF`.

Either way, the writer pays at most the cost of the new section + its
30-line anchor — not the cost of re-reading the entire history.

## Example

```markdown
## 2026-04-22 — M1.1: monorepo layout

**PR**: https://github.com/vadimtrunov/watchkeepers/pull/1
**Merged**: 2026-04-22 17:14

### Context
Bootstrapped the Go + TS monorepo layout per ROADMAP §M1 (Foundation). First
PR; establishes directory conventions the rest of Phase 1 builds on.

### Pattern
Go modules under `/core` and `/cli`, pnpm workspace under `/harness` and
`/tools-builtin`, shared CI matrix in `.github/workflows/ci.yml`. All
build targets routed through `make <target>` — no direct `go build` or
`pnpm build` calls from anywhere (including CI). When adding a new
package, mirror the structure: `/<service>/cmd/`, `/<service>/internal/`,
`/<service>/README.md`.

### References
- Files: `Makefile`, `go.work`, `pnpm-workspace.yaml`, `.github/workflows/ci.yml`
- Docs: `docs/ROADMAP-phase1.md` §M1

---
```
