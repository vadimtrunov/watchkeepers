# Project Lessons — Watchkeepers

Patterns, decisions, and lessons accumulated during implementation.
Appended by the `rdd` skill after each merged TASK (one section per TASK).

Read by the `rdd` skill at the start of Phase 2 to seed brainstorming with
prior context. Read by humans whenever.

---

## 2026-04-22 — M2.6: Migration tool chosen and wired

**PR**: [#4](https://github.com/vadimtrunov/watchkeepers/pull/4)
**Merged**: 2026-04-22 17:00

### Context

Selected and wired goose (github.com/pressly/goose v3.27.0) as the schema
migration engine to support subsequent Phase 2 schema tasks (M2.1–M2.5). Added
Makefile targets (migrate-up, migrate-down, migrate-status, migrate-create),
CI job with postgres:16-alpine service, and round-trip sanity test
(up → down → up with schema-dump diff).

### Pattern

**Tool pinning via `go run <module>@<version>`**: Goose installed in CI and
local dev via `go run github.com/pressly/goose/v3/cmd/goose@v3.27.0` rather
than a `go.mod` require, avoiding premature license-scan noise for an
external tool not embedded in the final binary. Version pinned once in
`Makefile` (`GOOSE_VERSION ?= v3.27.0`) and one-time entry in `.tool-versions`
(asdf convention, stripped of v-prefix). Pattern applies to any CI-only tool;
promotes to `go.mod` when the library is embedded (M2.7+).

**Makefile target-specific `export` for user-provided values**: Names passed
to `migrate-create NAME=<slug>` are unsafe for Make-variable substitution
because `$(NAME)` expands into recipe text before any shell validation runs.
Correct pattern: `target: export MIGRATION_NAME := $(NAME)` so the shell sees
an env-var literal. Validate in the script, never in Make. Similar injection
risks apply to any user string in a Makefile recipe.

**Round-trip migration sanity check**: Canonical pattern for migration
validation is `migrate-up` → `pg_dump --schema-only` → `migrate-down` to 0 →
`migrate-up` → second `pg_dump --schema-only` → diff (must be empty, ignoring
migration-tracking table). Implemented in `scripts/migrate-round-trip.sh` and
inherited by future migrations (M2.1+).

### Anti-pattern

Iteration-1 attempt to validate `NAME` with regex _after_ Make expansion was
bypassable. CodeRabbit showed exploit `x' ; printf INJECTED >&2 ; echo '`.
Never quote to fix injection in Makefile recipes — use `export` and an env
var instead.

### References

- Files: `Makefile`, `deploy/migrations/001_init.sql`,
  `scripts/test-migrate.sh`, `scripts/migrate-round-trip.sh`,
  `docs/DEVELOPING.md`, `.github/workflows/ci.yml`, `.tool-versions`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.6

---

## 2026-04-22 — M2.1.a: Core business-domain tables DDL

**PR**: [#5](https://github.com/vadimtrunov/watchkeepers/pull/5)
**Merged**: 2026-04-22 18:40

### Context

Created the first real Keep migration (`002_core_business_tables.sql`) with six
core business-domain tables — organization, human, watchkeeper, manifest,
manifest_version, watch_order — under the watchkeeper schema. Added psql-driven
schema smoke tests (happy-path inserts, unique-constraint rejection, FK
rejection) and integrated into CI.

### Pattern

**UUID primary keys + pgcrypto**: All core tables use `uuid` PKs with
`gen_random_uuid()` from pgcrypto. Protocol-neutral (works for HTTP+JSON and
gRPC), no exposed ordering, federation-ready. Reused for M2.1.b/c/d/e and
beyond.

**SQLSTATE over English error text**: Schema tests grep on locale-independent
SQLSTATE codes (`23505` unique_violation, `23503` foreign_key_violation)
instead of English error messages. Server's `lc_messages` setting may not match
the client; CI is safe on C locale, but local dev on non-English systems fails
if matching error text.

**Protocol-neutral DDL**: All column types portable (`uuid`, `text`,
`timestamptz`, `jsonb`, `integer`, `boolean`). Deliberate decision to keep
M2.7 protocol choice (HTTP vs gRPC) open.

**DROP EXTENSION in Down is a cross-migration footgun**: Extensions are
database-scoped, not migration-scoped. Per-migration Down should not drop
extensions created with `IF NOT EXISTS` — future migrations may depend on them.

### Anti-pattern

Per-FK auto-indexing deferred. Postgres does not auto-index FKs; current DDL
relies on unique-index prefixes only. Worth adding before real traffic or RLS
(M2.1.d).

### References

- Files: `deploy/migrations/002_core_business_tables.sql`,
  `scripts/migrate-schema-test.sh`, `Makefile`, `docs/DEVELOPING.md`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.1.a

---
