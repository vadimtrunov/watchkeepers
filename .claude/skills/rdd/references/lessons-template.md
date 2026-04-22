# `docs/LESSONS.md` Entry Template

Used by the `writer` agent at Phase 7 to append a new section to
`docs/LESSONS.md`.

## File structure (first time only)

If `docs/LESSONS.md` does not exist, create it with this header:

```markdown
# Project Lessons — Watchkeepers

Patterns, decisions, and lessons accumulated during implementation.
Appended by the `rdd` skill after each merged TASK (one section per TASK).

Read by the `rdd` skill at the start of Phase 2 to seed brainstorming with
prior context. Read by humans whenever.

---
```

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
