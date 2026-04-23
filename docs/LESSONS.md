# Project Lessons — Watchkeepers

Patterns, decisions, and lessons accumulated during implementation.
Appended by the `rdd` skill after each merged TASK (one section per TASK).

Read by the `rdd` skill at the start of Phase 2 to seed brainstorming with
prior context. Read by humans whenever.

## 2026-04-22 — M2.1: Complete Keep schema foundation (knowledge_chunk + RLS + outbox)

**PR**: [#7](https://github.com/vadimtrunov/watchkeepers/pull/7)
**Merged**: 2026-04-22

### Context

Bundled M2.1.c (knowledge_chunk table + pgvector setup), M2.1.d (RLS policy + FORCE
semantics), and M2.1.e (outbox event table + per-FK indexes) into a single 8-commit PR.
Established the full Keep schema scaffold with vector embeddings, row-level security,
and event-sourcing outbox for downstream Keep mutations.

### Pattern

**pgvector + HNSW recipe for first use**: Created extension via `CREATE EXTENSION IF NOT
EXISTS vector;` at the migration top level (not in `001_init`). HNSW index uses
`vector_cosine_ops` with `m=16, ef_construction=64` tuning parameters. Test requires
≥100 rows + `ANALYZE` + plan-text grep to verify index selection. Deterministic test
seed uses `random() + 0.001` to guarantee non-zero vector components (cosine safety).

**FORCE RLS owner-baseline assertion pattern**: `ENABLE ROW LEVEL SECURITY` alone does not
restrict the table owner. Setting `FORCE ROW LEVEL SECURITY` forces all roles — including
the owner — into policy checks. Correct test pattern: assert owner-baseline (no policy
filters, owner sees all rows), then assert policy-subject assertions (filtered rows visible
via SET ROLE). Naive test of SET ROLE without owner-baseline misses a semantic gap.

**Per-FK index coverage bundled with RLS**: M2.1.a flagged "defer per-FK indexing until
before RLS"; this bundle bakes all per-FK indexes into the RLS migration. Pattern: review
FK coverage in the same migration you add RLS to keep the dependency implicit.

### References

- Files: `deploy/migrations/005_knowledge_chunk.sql`, `006_rls_and_outbox.sql`
- Docs: `docs/ROADMAP-phase1.md` §M2.1

---

## 2026-04-22 — M2.1.b: keepers_log table DDL + append-only trigger

**PR**: [#6](https://github.com/vadimtrunov/watchkeepers/pull/6)
**Merged**: 2026-04-22 21:15

### Context

Created the `keepers_log` audit table with append-only enforcement via PL/pgSQL
triggers. This establishes the event-sourcing foundation for tracking all
mutations to core entities (organization, watchkeeper, watch_order). Migration
`003_keepers_log.sql` introduces the pattern for immutable audit logs that
future tables will reuse.

### Pattern

**Append-only audit table via trigger-owned error messages**: PL/pgSQL function
`keepers_log_reject_mutation()` raises a stable, locale-independent phrase
(`keepers_log is append-only`). Two BEFORE-ROW triggers (one for UPDATE, one for
DELETE) call this function, enforcing immutability per-row. Unlike grepping
Postgres-translated error text (M2.1.a anti-pattern), we own the message,
making tests locale-independent. Grep for the phrase in test assertions;
SQLSTATE codes handle Postgres-native errors.

**Partial index on optional correlation columns**: `CREATE INDEX ... ON
(correlation_id) WHERE correlation_id IS NOT NULL` avoids bloating the index
with nulls. Applied when a column starts nullable and fills over time — here,
correlation IDs link mutations to external events but are initially sparse.

### Anti-pattern

TRUNCATE cleanup order comment incorrectly justified "keepers_log first because
it has nullable FKs" — nullable FKs do not affect TRUNCATE ordering. Correct
reason: reverse-dependency order (newest-leaf tables first). Future migrations
should cite dependency order, not FK nullability.

### References

- Files: `deploy/migrations/003_keepers_log.sql`,
  `scripts/migrate-schema-test.sh`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.1 → M2.1.b

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

## 2026-04-22 — M2.7.a: Keep service skeleton (HTTP server, health, pgx pool)

**PR**: [#8](https://github.com/vadimtrunov/watchkeepers/pull/8)
**Merged**: 2026-04-22

### Context

Bootstrapped the Keep service as a separate Go binary (`core/cmd/keep/`) with HTTP
as the chosen protocol (over gRPC), environment-based config, Postgres connection
pooling via pgx/v5, graceful shutdown handling, and a distroless multi-stage Dockerfile.
Established the service boot template for future M2.7.b/c/d/e endpoints.

### Pattern

**Go service boot pattern (signal + context + shutdown)**: `signal.NotifyContext` wraps
a base context; main defers `http.Server.Shutdown(ctx.WithoutCancel())` to close
gracefully without cancellation races. Two-stage `main()` (exit wrapper + `run(args,
stdout, stderr) int`) enables unit testing without mocking `os.Exit`. Config loaded
via `os.Getenv`-first approach + typed `Config{}` + `Load()` sentinel errors (`ErrMissingDatabaseURL`)
— locale-independent, no framework yet (viper/koanf promoted only when multi-service
configs share a model).

**HTTP-vs-gRPC decision recorded**: Protocol choice documented in `docs/DEVELOPING.md`
"Keep service" section with reversibility criteria (future M9/Phase-2 streaming
benefits can revisit). Rationale: simpler initial endpoints, JSON compatibility,
debuggability (`curl`), deferring RPC overhead.

**Distroless + multi-stage Dockerfile template**: `golang:1.26-alpine` (build) →
`gcr.io/distroless/static-debian12:nonroot` (runtime), final image ~10 MB, `USER
nonroot:nonroot`, `COPY go.mod go.sum ./` for cacheable `go mod download`, hadolint-clean.

### References

- Files: `core/cmd/keep/main.go`, `core/internal/keep/config/`, `core/internal/keep/server/`,
  `deploy/Dockerfile.keep`, `Makefile` (keep-build, keep-run targets)
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.7 → M2.7.a, `docs/DEVELOPING.md` "Keep service"

---

## 2026-04-23 — M2.7.b+c: Keep read API — capability-token auth + read endpoints

**PR**: [#9](https://github.com/vadimtrunov/watchkeepers/pull/9)
**Merged**: 2026-04-23

### Context

Extended the Keep service with a capability-token authentication middleware and three read endpoints
(`/v1/search`, `/v1/manifests/{id}`, `/v1/keepers-log`) that enforce row-level security via transactional
scope binding. Bundled two ROADMAP leaves (M2.7.b auth + M2.7.c endpoints) because the read contract
depends on the auth layer.

### Pattern

**Minimal stdlib JWT verifier (HS256 without external dependency)**: Implemented in-repo verifier using only
`crypto/hmac`, `crypto/sha256`, and `crypto/subtle`. Signs `base64url(header).base64url(payload)` where
header is fixed `{alg:HS256,typ:JWT}` and payload JSON-encodes `Claim{Subject, Scope, Issuer, ExpiresAt, IssuedAt}`.
No external `golang-jwt/*` dependency required; smaller attack surface, no transitive supply-chain risk.
Exposed `testIssuer` helper in `auth/testing.go` (co-located with production verifier) so integration tests
mint their own tokens without test-only wire format.

**SET LOCAL ROLE + SET LOCAL watchkeeper.scope transactional wrapping**: `db.WithScope(ctx, pool, claim, fn)`
opens a transaction, validates scope format (prefix matching: `org`, `user:<id>`, `agent:<id>`), runs
`SET LOCAL ROLE wk_<kind>_role` derived from the prefix, sets `watchkeeper.scope = '<value>'` as a session
GUC, executes the read query inside `fn`, and commits. Role validation done as a closed set of literal strings
before string substitution into SQL — no SQL injection risk. Isolation guaranteed: two concurrent requests
with different scopes do not observe each other's `SET LOCAL` state.

**Input-bounding discipline for LLM-facing endpoints**: `POST /v1/search` request body wrapped with
`http.MaxBytesReader(1 MiB)` to prevent unbounded buffering. Embedding vector capped at 4096 dimensions
(hardcoded literal, checked before unmarshaling). Oversized body returns `413 Payload Too Large`. Missing
or malformed `Content-Type: application/json` returns `415 Unsupported Media Type`. Anti-pattern: JSON body
decoded without `MaxBytesReader` + unbounded slice = DoS vector on authenticated routes; `DisallowUnknownFields`
does NOT save you because the decoder has already buffered the full body before field validation.

### References

- Files: `core/internal/keep/auth/`, `core/internal/keep/server/{middleware,handlers_read}.go`,
  `core/internal/keep/db/scope.go`, `core/cmd/keep/read_integration_test.go`,
  `deploy/migrations/007_read_grants.sql`, `scripts/migrate-schema-test.sh`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.7 → M2.7.b + M2.7.c, `docs/DEVELOPING.md` "Keep service"

---

## 2026-04-23 — M2.7.d: Keep write API — store, log_append, put_manifest_version

**PR**: [#10](https://github.com/vadimtrunov/watchkeepers/pull/10)
**Merged**: 2026-04-23

### Context

Added three HTTP write endpoints to the Keep service (`POST /v1/knowledge-chunks`, `POST /v1/keepers-log`,
`PUT /v1/manifests/{id}/versions`) with the same token-bound scope isolation and input-validation discipline
as the read side. All mutations respect RLS and per-scope role semantics established in M2.7.b+c.

### Pattern

**Token-bound writes and server-stamped actor columns**: Handler stamps `scope = claim.Scope`; client body
cannot override it (`DisallowUnknownFields` rejects unknown fields). `actorFromScope(scope)` derives actor
columns (`actor_watchkeeper_id` / `actor_human_id`) from token prefix (`agent:<uuid>` / `user:<uuid>` / `org`)
with UUID shape validation before DB cast — malformed UUIDs return 400, not 500.

**Unique-violation translation via error inspection**: Duplicate `(manifest_id, version_no)` raises
`pgconn.PgError` with `Code == "23505"`. Use `errors.As(&pgErr)` to detect and translate to 409
`{"error":"version_conflict"}` without surfacing raw Postgres text. Pattern applies to any future
uniqueness-constrained write.

**Exact-dimension vector check**: `knowledge_chunk.embedding` is `vector(1536)`. Hardcode a constant
`knowledgeChunkEmbeddingDim = 1536` and reject `!= 1536` with 400 `invalid_embedding`. Checking only an
upper bound (e.g. 4096) turns client-shape errors into 500s.

### Anti-pattern

Checking only a maximum bound (e.g. `len(embedding) <= 4096`) for a fixed-dimension column. Client input
errors should be 400, not 500; enforce exact match to the schema's declared dimension.

### References

- Files: `core/internal/keep/server/handlers_write.go`, `core/cmd/keep/write_integration_test.go`,
  `deploy/migrations/008_write_grants.sql`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.7 → M2.7.d, `docs/DEVELOPING.md` "Keep service"

---
