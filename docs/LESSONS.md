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

## 2026-04-24 — M2.7.e.a: Keep subscribe endpoint + in-process publish Registry

**PR**: [#11](https://github.com/vadimtrunov/watchkeepers/pull/11)
**Merged**: 2026-04-24

### Context

Added the `GET /v1/subscribe` Server-Sent Events endpoint to the Keep service under capability-token auth. Introduced an in-process `publish.Registry` with strict scope-equality fan-out and wired it into graceful shutdown. The endpoint runs entirely in-process without outbox reads; outbox-backed event sourcing lands in M2.7.e.b.

### Pattern

**SSE over WebSocket/long-poll for unidirectional publish→subscribe**: Aligns with HTTP choice from M2.7.a. Uses stdlib-only `http.Flusher` with `Content-Type: text/event-stream`, no hijack of ResponseWriter. `curl -N` debuggable. Backpressure falls out of TCP + Flusher without external transport complexity.

**In-process Publisher seam + Registry fan-out decouples endpoint from outbox worker**: `NewRegistry(bufSize, heartbeat)` exports `Publish(ctx, Event) error` and `Subscribe(ctx, claim) (<-chan Event, func())` / `Close()`. The `Event` carries explicit `Scope` field (a superset of outbox DDL) so fan-out filter is exact-match equality on `claim.Scope` — no hierarchy widening, mirrors M2.7.b+c's RLS model.

**Non-blocking fan-out with drop-on-full backpressure**: `Publish` does a non-blocking send to each matching subscriber; on a full buffer, that subscriber closes (client reconnects) and other subscribers unaffected. Publisher never stalls. Shutdown ordering critical — `Registry.Close()` BEFORE `httpSrv.Shutdown(ctx)` — so stream handlers return cleanly and clients observe `io.EOF`/`io.ErrUnexpectedEOF`, not broken-pipe. Registry-wide `done` channel also releases per-subscription watchdog goroutines.

### References

- Files: `core/internal/keep/publish/{event,registry,registry_test,export_test}.go`, `core/internal/keep/server/{handlers_subscribe,handlers_subscribe_test,server,server_test,export_test}.go`, `core/internal/keep/config/config.go` (KEEP_SUBSCRIBE_BUFFER, KEEP_SUBSCRIBE_HEARTBEAT), `core/cmd/keep/{main.go,subscribe_integration_test.go}`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.7 → M2.7.e → M2.7.e.a, `docs/DEVELOPING.md` "Keep service"

---

## 2026-04-27 — M2.7.e.b: outbox publisher worker consuming outbox table into subscribe publish API

**PR**: [#12](https://github.com/vadimtrunov/watchkeepers/pull/12)
**Merged**: 2026-04-27

### Context

Implemented an outbox publisher worker inside the Keep service. The worker polls the `watchkeeper.outbox` table for unpublished rows, converts each to a `publish.Event`, invokes the in-process `publish.Registry.Publish()` for SSE fan-out to `/v1/subscribe` subscribers, and stamps `published_at=now()` on success. Worker lifecycle is wired into graceful shutdown with strict ordering. Migration adds the `scope` column to `outbox` (deferred from M2.7.e.a).

### Pattern

**Outbox publisher worker with `FOR UPDATE SKIP LOCKED`**: Worker selects unpublished rows ordered by `created_at ASC` using `FOR UPDATE SKIP LOCKED` within a transaction, publishes each via `reg.Publish(ctx, event)`, and stamps `published_at=now()` in the same transaction on success. Lock-skip enables future multi-replica scale-out without double-publish. Publish failures leave the row unpublished for the next tick; errors are logged, never panicked. Semantics are at-least-once (dedup required in consumers).

**Shutdown ordering on both happy and error paths**: `server.Run` cancels workerCtx → waits `<-workerDone` → calls `reg.Close()` → calls `httpSrv.Shutdown(ctx.WithoutCancel())`. This ordering must be honored on both `<-ctx.Done()` (graceful) and `<-errCh` (error) branches. Registry closes before HTTP shutdown so stream handlers drain cleanly.

**Environment-driven poll interval with range validation**: Config field `OutboxPollInterval` plus env var `KEEP_OUTBOX_POLL_INTERVAL` (default `1s`, range `100ms`–`60s`) follows the existing `KEEP_SUBSCRIBE_*` pattern — env-first lookup, typed defaults, sentinel-error validation in `config.Load`. Migration `009_outbox_scope.sql` adds `scope text NOT NULL DEFAULT 'org'` with CHECK constraint matching RLS format (`org`, `user:<uuid>`, `agent:<uuid>`).

**Read-after-commit race in integration tests**: SSE delivery to client races ahead of the same-transaction Commit visible to a separate pool connection. Test pattern: `awaitPublishedAt` polling helper (25ms backoff, 2s timeout) retries until `published_at IS NOT NULL` on the same row, confirming worker has completed the transaction.

### Anti-pattern

Docstring claim of "exactly-once delivery via stamp-in-txn" is wrong. Stamping and publishing span an in-process side-effect plus a DB transaction; if Commit fails after Publish succeeds, the next tick re-publishes. Semantics are at-least-once; consumers must dedup on `Event.ID`. Don't claim "exactly-once" without an end-to-end transactional guarantee.

### References

- Files: `core/internal/keep/publish/outbox.go`, `core/internal/keep/server/server.go`, `core/cmd/keep/outbox_integration_test.go`, `deploy/migrations/009_outbox_scope.sql`, `core/internal/keep/config/config.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.7 → M2.7.e → M2.7.e.b

---

## 2026-05-02 — M2.8.a: keepclient package skeleton

**PR**: [#13](https://github.com/vadimtrunov/watchkeepers/pull/13)
**Merged**: 2026-05-02 12:00

### Context

Bootstrapped the `keepclient` package under `core/pkg/keepclient/` as the first reusable Go client for the Keep service. Established the transport plumbing (functional options, context-aware request helper, token injection, error mapping) and exercised it via the `Health(ctx)` endpoint — an open route requiring no authentication. Sets the canonical client shape for future M2.8.b/c/d business-endpoint slices.

### Pattern

**Functional-options + lowercase `do(ctx, method, path, body, out)` helper as canonical client shape**: `NewClient(opts ...Option)` accepts `WithBaseURL(url)`, `WithHTTPClient(client)`, `WithTokenSource(source)`, and `WithLogger(func)` via closures that mutate a `clientConfig`. The internal `do` helper marshals request JSON, injects `Authorization: Bearer <token>` only on paths prefixed `/v1/` (leaving open routes like Health clean), decodes response JSON, and parses server error envelopes to a typed `ServerError` whose `Unwrap()` returns a sentinel (`ErrUnauthorized`, `ErrForbidden`, etc.) for `errors.Is` pattern matching. This shape (conditional auth injection + sentinel errors) reused in M2.8.b/c/d.

**Smoke contract test pattern**: `core/cmd/keep/keepclient_smoke_test.go` boots the real Keep binary (reusing the `KEEP_INTEGRATION_DB_URL` gate from other integration tests) and constructs a client against it, verifying end-to-end Health call succeeds. Isolates the transport contract from unit tests (which use `httptest.Server`) and upstream client-library shape from server implementation.

### References

- Files: `core/pkg/keepclient/{client,do,errors,tokensource}.go`, `core/pkg/keepclient/client_test.go`, `core/cmd/keep/keepclient_smoke_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.8 → M2.8.a

---

## 2026-05-02 — M2.8.b: keepclient read endpoints (Search, GetManifest, LogTail)

**PR**: [#14](https://github.com/vadimtrunov/watchkeepers/pull/14)
**Merged**: 2026-05-02 19:11

### Context

Extended `core/pkg/keepclient/` with three typed read methods (Search, GetManifest, LogTail) that wrap stable server-side endpoints. Each method reuses the M2.8.a transport helper `do()`, which abstracts token injection, error mapping, and JSON marshalling. Adds unit tests against `httptest.Server` for contract surface (auth, query-string assembly, status mapping, JSON shape) and integration smoke test against the real Keep binary.

### Pattern

**TDD round per endpoint sharing transport plumbing**: Search, GetManifest, LogTail were structurally near-duplicates because `do(ctx, method, path, body, out)` absorbed all cross-cutting concerns. Each endpoint was ~60 LOC of typed models + a one-liner `do` call. Future M2.8.c/d should follow this layout.

**Per-method test matrices for status-code coverage**: Reviewer flagged AC5 gap: GetManifest and LogTail lacked full 400/500/context-cancel/transport-error sentinel cases per method. The fix is mechanical (table-driven `errors.As` + `errors.Is`), but critical: when an AC says "(per method)", every method needs the full matrix.

**Server JSON shape mirroring via JSON tags**: Client types (SearchResult, ManifestVersion, LogEvent) mirror server handlers verbatim — field names, `omitempty`, `*string` for nullable cols, `json.RawMessage` for jsonb. Keeping field-name correspondence at the JSON tag level avoids a translation layer and reduces shape divergence risk.

### References

- Files: `core/pkg/keepclient/{read_search,read_manifest,read_logtail}*.go`, `core/cmd/keep/keepclient_read_smoke_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.8 → M2.8.b

---

## 2026-05-03 — M2.8.c: keepclient write endpoints (Store, LogAppend, PutManifestVersion)

**PR**: [#15](https://github.com/vadimtrunov/watchkeepers/pull/15)
**Merged**: 2026-05-03 09:23

### Context

Extended `core/pkg/keepclient/` with three typed write methods (Store, LogAppend, PutManifestVersion) that wrap stable server-side endpoints, completing the read+write surface. Each method reuses M2.8.a's transport helper `do()` and follows M2.8.b's per-method test matrices. Subscribe and async streaming deferred to M2.8.d.

### Pattern

**Three endpoints, one shared `do()` helper confirmed for the third time**: Store/LogAppend/PutManifestVersion are each ~50 LOC of typed models + a one-liner `do(ctx, method, path, body, out)` call. Layout: one `write_<endpoint>.go` + one `write_<endpoint>_test.go` per method. The `do()` helper absorbed all token injection, error mapping, and JSON marshalling by design; write endpoints inherit the same shape.

**Path-parametric endpoints with belt-and-suspenders validation**: `PutManifestVersion` uses `PUT /v1/manifests/{manifest_id}/versions`. Client interpolates the id with `url.PathEscape(manifestID)` and pre-validates it as a canonical UUID (regex `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`). The server also rejects malformed ids — client validation is not trust, just early feedback.

**Server JSON-shape mirroring with deliberate `omitempty` divergence**: Client request structs add `omitempty` on optional fields (subject, correlation_id, jsonb fields, personality, language) while the server has none. Intentional — server defaults fire when a field is absent, and `DisallowUnknownFields` rejects unknown KEYS, not absent ones. Future write-endpoint clients should document this divergence in struct godoc: "omitempty tags allow callers to omit optional fields; server applies defaults."

**No new error sentinels needed for write endpoints**: Existing `ErrConflict` (409) covers `version_conflict` from Put manifest uniqueness violation. 413/415 surface as raw `*ServerError` via `errors.As` (no sentinel needed). Client-shape errors return 400; server doesn't surface new write-specific codes.

**Test-file commit shape constrained by lefthook pre-commit golangci-lint**: The strict "failing-test commit, then impl commit" cadence is physically impossible — undefined-symbol references fail the `golangci-lint typecheck` stage before the impl lands. Practical layout: combine test+impl per endpoint into one commit, mentioning the constraint in the commit body. Fixer can then land follow-up cleanup separately.

### References

- Files: `core/pkg/keepclient/{write_store,write_logappend,write_putmanifestversion}.go`(\_test.go), `core/cmd/keep/keepclient_write_smoke_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.8 → M2.8.c

---

## 2026-05-03 — M2.8.d.a: keepclient Subscribe SSE consumption with typed Event model and httptest contract tests

**PR**: [#16](https://github.com/vadimtrunov/watchkeepers/pull/16)
**Merged**: 2026-05-03

### Context

First streaming endpoint in `core/pkg/keepclient/`. Wraps `GET /v1/subscribe` from M2.7.e. Adds the SSE parser and iterator-style `*Stream` API; reconnect and Last-Event-ID deduplication deferred to M2.8.d.b.

### Pattern

**Streaming primitive ≠ unary `do(...)`**: The unary helper used by M2.8.b/c covers token injection, JSON marshalling, and status mapping, but cannot host long-lived SSE bodies. M2.8.d.a introduces a sibling streaming open path that reuses the unary code's auth + initial-status decoder, then hands `resp.Body` to a `*bufio.Reader`-backed parser. Future streaming endpoints follow this layout.

**Iterator over channel**: `Stream.Next(ctx) (Event, error)` + `Stream.Close()` chosen over `<-chan Event` to make `Close` deterministic and to give M2.8.d.b a clean place to layer reconnect (just wrap the iterator).

**`io.EOF` vs `ErrStreamClosed` distinction**: Clean server EOF returns `io.EOF`; local `Close()` makes subsequent `Next` return `ErrStreamClosed` (a non-EOF sentinel). Callers can distinguish "server is done" from "we closed it".

**Default 10s httpClient.Timeout caps the body read too**: `NewClient` sets a 10s timeout that applies to the entire request lifecycle, including streaming body reads. Subscribe godoc instructs callers to override with `WithHTTPClient(&http.Client{})` (no timeout) or a per-request `context.WithTimeout`. M2.8.d.b should consider splitting timeouts at the `Client` level (request-init vs body-read).

**SSE parser correctness**: Handles multi-line `data:` joined with `\n`, missing `data:` (Payload=nil), missing `event:` (EventType=""), and silently skips `:` heartbeats. Bare `\r` terminator (per HTML EventSource spec) is unhandled but server uses `\n`-only; documented as a known limitation.

**`id:` captured even though M2.8.d.a does not use it**: Keeping it in the `Event` struct now means M2.8.d.b can wire `Last-Event-ID` resume without a struct change.

### References

- Files: `core/pkg/keepclient/subscribe.go`, `core/pkg/keepclient/subscribe_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.8 → M2.8.d → M2.8.d.a
- Server contract: `core/internal/keep/server/handlers_subscribe.go`

---

## 2026-05-03 — M2.8.d.b: Subscribe reconnect policy + Last-Event-ID + dedup hooks + integration smoke

**PR**: [#17](https://github.com/vadimtrunov/watchkeepers/pull/17)
**Merged**: 2026-05-03

### Context

Layered resilience onto the M2.8.d.a streaming primitive: exponential-backoff reconnection on transport EOF or error, `Last-Event-ID` forwarding for dedup (W3C spec-compliant), and caller-supplied or built-in LRU dedup predicate. Entire surface is `(c).SubscribeResilient(ctx, opts...)` returning `*ResilientStream{Next, Close}` — same shape as the inner stream, allowing zero-change caller swap-up.

### Pattern

**Resilient streaming layered on top of an iterator-style primitive**: `*ResilientStream` wraps `*Stream` from M2.8.d.a. Adds reconnect-on-EOF/transport-error with exponential backoff + jitter, `Last-Event-ID` forwarding (forward-compat — server doesn't honor today), caller-supplied dedup predicate or built-in LRU. Entire surface is `(c).SubscribeResilient(ctx, opts...)` returning `*ResilientStream{Next, Close}` — same shape as the inner stream so callers can swap up.

**Sleeper-injection seam for backoff tests**: `type sleeper interface{ Sleep(ctx, d) error }` with a real impl and a `fakeSleeper` recording the requested durations. Backoff sequence assertions (`min(maxDelay, initial*2^n) ± 25%`) verify deterministically without any wall-clock waits. Pattern reusable for any future timer-based code.

**Empty-`id:` SSE frame state-machine bug**: the W3C SSE spec says `last-event-ID` only updates when a frame carried an `id:` field. Naive `s.lastID = ev.ID` clobbers a previously-recorded id with `""` whenever the server emits an id-less frame. Fixed with `if ev.ID != "" { s.lastID = ev.ID }`. Forward-compat critical: the moment the server starts honoring `Last-Event-ID`, this would silently replay events. Keep this guard pattern in mind for any future SSE-state-tracking code.

**Integration-test isolation in shared DB**: smoke tests that publish to `watchkeeper.outbox` MUST clean up their rows in `t.Cleanup`. The publisher worker stamps `published_at` async; if the binary tears down mid-tick (`context canceled`), unstamped rows persist and the NEXT test's worker re-publishes them, polluting unrelated subscriber tests. Pattern: helpers like `publishOutboxEvent` should return the row id; tests call `t.Cleanup(func(){ deleteOutboxRow(t, pool, id) })` for every insert.

**CI-vs-local determinism for transport failure injection**: a custom round-tripper that drops the body after N BYTES is flaky — TCP buffering and small payloads can let everything through in one Read. Drop after N FRAMES (count `\n\n` SSE boundaries) is deterministic regardless of network. Pattern usable for any SSE/chunked-stream test fixture.

### References

- Files: `core/pkg/keepclient/{subscribe_resilient,subscribe_resilient_test}.go`, `core/pkg/keepclient/subscribe.go` (added private `subscribeWithLastEventID`), `core/pkg/keepclient/errors.go` (`+ErrReconnectExhausted`), `core/cmd/keep/keepclient_subscribe_smoke_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.8 → M2.8.d → M2.8.d.b. **M2.8 is now COMPLETE** (cascade); M2 still `[ ]` because M2.9 pending.

---

## 2026-05-03 — M2.9.a: Manifest personality/language constraints, validation, and docs

**PR**: [#18](https://github.com/vadimtrunov/watchkeepers/pull/18)
**Merged**: 2026-05-03 (squash commit `847f42c`)

### Context

Added database and API constraints for the manifest `language` and `personality` fields to close the M2 milestone entirely. Migration `010_manifest_constraints.sql` enforces `language` as BCP 47-lite (`^[a-z]{2,3}(-[A-Z]{2})?$`) and `personality` as max 1024 Unicode codepoints via SQL CHECK constraints. Server handler `parsePutManifestVersionRequest` validates symmetrically before the row reaches Postgres. Client `(*Client).PutManifestVersion` pre-validates on the same shape for early feedback.

### Pattern

**Defense-in-depth: regex constraints in BOTH validation layers AND DB**: Server `parsePutManifestVersionRequest` rejects `invalid_language`/`personality_too_long` with stable 400 reason codes BEFORE the row reaches Postgres; the new migration adds matching CHECK constraints (`manifest_version_language_format` for the regex, `manifest_version_personality_length` for `char_length <= 1024`) so a regression in the handler or a non-Keep writer cannot sneak bad data in. Client `(*Client).PutManifestVersion` also pre-validates symmetrically — three validation layers, same regex string constant, same codepoint limit.

**`utf8.RuneCountInString` not `len()` for SQL `char_length` parity**: Postgres `char_length` counts Unicode codepoints, not bytes. Counting bytes via `len(s)` would silently accept a 256-byte 64-glyph personality on the server while the DB would still reject it as 64 chars (passing) — but the bug surfaces the OTHER way: a 1024-rune string of 4-byte chars (4096 bytes) would byte-rejected by the server even though SQL would accept it. Always use `utf8.RuneCountInString` to match Postgres semantics.

**BCP 47-lite for "ISO code" fields**: `^[a-z]{2,3}(-[A-Z]{2})?$` covers ISO 639-1 (`en`), ISO 639-3 (`eng`, `kab`), and optional ISO 3166-1 region (`en-US`, `pt-BR`). Lowercase language + uppercase region is a strict but stable subset; full BCP 47 (script tags, variants, extensions) is over-engineering for a manifest field. Document the regex shape inline (godoc + SQL header comment + migration body) so future readers don't reverse-engineer it.

**Goose migration for adding constraints to existing columns**: `ALTER TABLE ... ADD CONSTRAINT ... CHECK (col IS NULL OR <pred>)` is the canonical shape. Both `IF EXISTS` on `+goose Down` for safe rollback. Constraints fire only on non-NULL values so existing `NULL` rows aren't broken. No `ADD CONSTRAINT NOT VALID` needed because we're confident no bad rows exist (small table, short history).

**Test isolation in shared DB cemented as habit (3rd consecutive PR)**: Integration smoke registers `t.Cleanup(func(){ pool.Exec(... DELETE FROM ... WHERE id = $1 ...) })` for every inserted row. M2.8.d.b LESSONS introduced this pattern; M2.9.a adopted without prompting. Reviewer caught zero violations because the executor brief explicitly cited the LESSONS pattern.

### References

- Files: `deploy/migrations/010_manifest_constraints.sql`, `core/internal/keep/server/handlers_write.go` (+ test), `core/pkg/keepclient/write_putmanifestversion.go` (+ test), `core/cmd/keep/write_integration_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M2 → M2.9 → M2.9.a. **M2 milestone COMPLETE** — Keep service + keepclient + manifest validation done.

---

## 2026-05-03 — M2b.1: Notebook SQLite + sqlite-vec storage substrate

**PR**: [#19](https://github.com/vadimtrunov/watchkeepers/pull/19)
**Merged**: 2026-05-03 (squash commit `814e68c`)

### Context

Established the Notebook library's storage substrate using SQLite + sqlite-vec for vector embeddings. Three integration paths existed for SQLite + sqlite-vec in Go (mattn CGo, ncruces+wazero WASM, modernc pure-Go). After prototyping Option B (ncruces+wazero CGo-free), executor discovered a blocker: wazero v1.7.3 cannot enable `i32.atomic.store` instructions used in sqlite-vec's WASM bundle. Fallback to Option A (mattn/go-sqlite3 v1.14.44 + asg017/sqlite-vec-go-bindings/cgo v0.1.6) was clean and well-documented. Code-reviewer flagged a blocker on foreign-key enforcement and an important sync-contract documentation gap; fixer resolved both in one commit.

### Pattern

**Two-prong driver evaluation with documented fallback**: When adopting a new dependency with multiple integration options, encode the matrix in the TASK file with explicit reject criteria BEFORE writing code. Executor picked Option B per preference-driven rubric, hit a WASM-incompatibility wall, and fell back to Option A with confidence because the decision tree was already in place. Pattern: decision-matrix THEN evidence-driven pick THEN clean fallback.

**SQLite foreign-key enforcement OFF by default per connection**: `superseded_by TEXT NULL REFERENCES entry(id)` is silently a no-op unless the connection sets `PRAGMA foreign_keys=ON`. Mattn driver supports `_foreign_keys=on` DSN flag. Every new SQLite connection must (a) enable foreign-keys via DSN, (b) read it back via `PRAGMA foreign_keys` with fail-loud error if misnamed, and (c) carry a constraint-rejection negative test. Pattern: idempotent pragma readback mirrors M2.7.e.b's WAL pattern.

**Two-table sqlite-vec layout has explicit sync contract**: The vec0 virtual table and regular table joined on `id` is the canonical sqlite-vec pattern, but there is NO auto-cascade. INSERTs and DELETEs must be paired in the same transaction; UPDATE of join key is symmetric. Document the contract in package godoc + README so the next-layer API (M2b.2) doesn't get it wrong. Reviewer caught this on iter 1; fixer added the docs in `# Sync contract` godoc subsection + README mirror.

### References

- Files: `core/pkg/notebook/{db,path}.go` (+ `_test.go`), `core/pkg/notebook/README.md`, `go.mod` (added `mattn/go-sqlite3`, `asg017/sqlite-vec-go-bindings/cgo`)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.1. **M2b.1 substrate ready**; M2b.2 owns the `Remember`/`Recall`/`Forget`/`Archive`/`Import`/`Stats` public API and Recall-supporting indexes.

---

## 2026-05-03 — M2b.2.a: Notebook in-process CRUD (Remember/Recall/Forget/Stats)

**PR**: [#20](https://github.com/vadimtrunov/watchkeepers/pull/20)
**Merged**: 2026-05-03 (squash commit `6de9be1`)

### Context

Implemented the Notebook library's public CRUD API layer atop the M2b.1 substrate. Four endpoints (`Remember`, `Recall`, `Forget`, `Stats`) operationalize the sync contract between the `entry` and `entry_vec` tables. Phase 1 planner verdict was "too large"; decomposed M2b.2 into M2b.2.a (CRUD) and M2b.2.b (Archive/Import). M2b.2.a includes 18 tests passing under `-race`, schema migrations for partial indexes, and support for KNN recall with post-filtering. Code-reviewer converged at iteration 1 with zero blockers and zero importants (4 nits deferred to Follow-up).

### Pattern

**Sync-contract enforcement via single-tx wrapper**: M2b.1 documented the `entry`/`entry_vec` sync contract in godoc; M2b.2.a operationalized it — every public method that touches either table wraps the work in `BeginTx` + `defer tx.Rollback()` + explicit `tx.Commit()`. The `Remember` and `Forget` paths each touch both tables in one tx; rollback fires automatically on any error. Pattern: when a "sync contract" between two SQL artifacts is documented in the substrate, the layer above MUST wrap every mutation in a transaction so a partial failure doesn't leave the contract broken.

**sqlite-vec canonical query shape: `WHERE embedding MATCH ? AND k = ?`**: NOT `ORDER BY vec_distance_cosine(...) LIMIT ?`. The MATCH+k form uses the vec0 index; the ORDER BY+LIMIT form does a full scan. Tested via `TestRecall_TopK` against ≥5 rows. Always use the indexed form for Recall/KNN queries.

**Partial indexes for hot Recall predicates**: `CREATE INDEX entry_category_active ON entry(category) WHERE superseded_by IS NULL` and `entry_active_after ON entry(active_after) WHERE superseded_by IS NULL` are the canonical partial-index pattern for "active rows only" filters. Added via `CREATE INDEX IF NOT EXISTS` in the schema-init constant so existing M2b.1-era files transparently migrate on the next `Open` (verified by `TestSchema_IndexesAddedOnReopen`). Pattern reusable for any append-mostly table where most queries filter by a "is active" predicate.

**`COUNT(*) FILTER (WHERE ...)` requires SQLite 3.30+**: mattn driver bundles 3.46+ so this is fine. The pattern lets `Stats` compute totals + active + superseded in one query without a CASE WHEN dance.

**Pre-DB validation symmetric with substrate constraints**: `validate(*Entry)` checks `Category` ∈ enum (matches the DB CHECK constraint), `Content != ""` (matches NOT NULL), `len(Embedding) == 1536` (matches vec0 dim). All three rejections return `ErrInvalidEntry` synchronously before BeginTx, so a malformed Entry never opens a transaction.

### References

- Files: `core/pkg/notebook/{entry,errors,remember,recall,forget,stats}.go` + `_test.go`, `core/pkg/notebook/schema_test.go`, `core/pkg/notebook/db.go` (schema-init delta), `core/pkg/notebook/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.2 → M2b.2.a. **M2b.2.b owns** Archive/Import (snapshot lifecycle).

---

## 2026-05-03 — M2b.2.b: Notebook Archive + Import snapshot lifecycle

**PR**: [#21](https://github.com/vadimtrunov/watchkeepers/pull/21)
**Merged**: 2026-05-03 (squash commit `4013de0`)

### Context

Implemented the Notebook library's snapshot lifecycle: `Archive` exports the live `entry`/`entry_vec` tables to a standalone SQLite file via `VACUUM INTO`, and `Import` atomically replaces the live file with a spool-validated copy, maintaining single-writer isolation and exact embedding-byte preservation. Two distinct sync contexts (`archiveMu` for atomic reads, `importMu` per-receiver) and WAL/SHM sidecar cleanup ensure clean replacements. Reviewer iteration 1 surfaced one `important`: embedding-byte round-trip test coverage gap (AC5 explicitly required "embedding-bytes" match, but test used Recall ranking instead of direct `SELECT`).

### Pattern

**`VACUUM INTO` DDL escaping via `os.CreateTemp` + single-quote escape**: SQLite `VACUUM INTO 'path'` is DDL, not DML, so prepared-statement `?` placeholders do not apply. Safe pattern: (1) call `os.CreateTemp(dirPath, ".prefix-*")` to generate a path from a trusted system call; (2) validate it's absolute + NUL-free; (3) escape any single quotes via `'` → `''` (the SQL escape for string literals); (4) concatenate into the query string. Defense-in-depth: the path comes from a trusted source (CreateTemp) AND explicit quote-escaping guards against edge cases. Annotate the gosec G202 finding with a rationale comment citing this contract.

**Atomic file replacement requires same-FS rename**: Cross-device `os.Rename` fails on POSIX systems. For `Import` to atomically swap the live SQLite file, the spool-temp MUST be created in the SAME directory as the live file — use `os.CreateTemp(filepath.Dir(target), ".prefix-*")` instead of `os.TempDir()`. Using a temp-dir on a different filesystem breaks across mountpoint boundaries. Verified by `TestImport_AtomicRename`.

**WAL/SHM sidecar removal before rename**: SQLite WAL journal mode leaves `<file>-wal` and `<file>-shm` files next to the live DB. After a `Close()` (which checkpoints WAL via mattn driver) but BEFORE `os.Rename`, explicitly remove `-wal` and `-shm` so the new connection doesn't inherit stale journal pages. Belt-and-suspenders because Close should checkpoint, but the files may linger; verified by `TestImport_CleansSidecars`.

**`sync.Once` reset trick for re-openable resources**: M2b.1's `closeOne sync.Once` makes Close idempotent, but `Import` reopens the underlying `*sql.DB`. Pattern: under the receiver's `importMu` lock, swap the `*sql.DB` field, assign a fresh `sync.Once{}` to the close-once field, AND clear the cached `closeErr`. A subsequent `Close()` then runs once on the new connection. This is the canonical "re-init a once-guarded resource" Go pattern.

**Embedding-bytes round-trip requires direct `SELECT` + `bytes.Equal`**: Recall's KNN ranking is not the same as byte-for-byte embedding equality. To verify `Archive` → `Import` preserves embedding data exactly, the test must `SELECT entry_vec.embedding FROM entry_vec WHERE id = ?` and `bytes.Equal` against `vec.SerializeFloat32(seed.embedding)`. Without direct byte comparison, a vec0 truncation or zeroing bug would silently pass the KNN-ranking test. Pattern: when an AC names specific match criteria (ID, category, content, embedding-bytes), the test must assert each one explicitly, not just "Recall returns the row".

### References

- Files: `core/pkg/notebook/{archive,import,validate_archive}.go` + `_test.go`, `core/pkg/notebook/{db,errors,README.md}` (deltas)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.2 (now `[x]`). **M2b.3 owns ArchiveStore** — wraps Archive/Import with LocalFS + S3Compatible backends.

---

## 2026-05-03 — M2b.3.a: ArchiveStore interface + LocalFS implementation

**PR**: [#22](https://github.com/vadimtrunov/watchkeepers/pull/22)
**Merged**: 2026-05-03 (squash commit `ba28046`)

### Context

Introduced the `ArchiveStore` interface as an abstraction for backup-tarball storage, with a LocalFS implementation storing tarballs under a root directory. Phase 1 planner decomposed M2b.3 into M2b.3.a (interface + LocalFS) and M2b.3.b (S3Compatible). M2b.3.a includes parameterized contract tests (`runContractTests`) reusable by M2b.3.b, path-traversal defenses, and RFC3339 timestamp filenames. Executor delivered all 5 AC green in 1 commit; Phase 4 fixer iter 1 resolved 1 important (embedding-byte round-trip test coverage via external SQLite access).

### Pattern

**Stdlib `archive/tar` + `compress/gzip` cover backup-tarball needs without a new dep**: M2b.3.a wraps a `notebook.Archive` snapshot in a single-entry tarball at `<root>/notebook/<agentID>/<RFC3339>.tar.gz` using only stdlib. Pattern: spool the snapshot to a temp file in the same dir as the target tarball (cross-FS-rename safe per M2b.2.b LESSONS), `os.Stat` it for `tar.Header.Size`, then write tar+gzip in one streaming pass. No memory blowup, no third-party deps.

**Path-traversal defence via `filepath.Clean` + `strings.HasPrefix`**: For any `Get(uri)`-style API where the URI maps to a filesystem path under a known root, the canonical defence is: parse the scheme, strip prefix, `filepath.Abs` + `filepath.Clean` both the input AND the root, then verify `strings.HasPrefix(cleanedInput, cleanedRoot + filepath.Separator)`. Reject otherwise with a wrapped sentinel. Catches `file:///etc/passwd`, `file://<root>/../../etc/passwd`, and `s3://` schemes uniformly.

**Parameterised contract test suite for backend interfaces**: When introducing an interface that will have multiple implementations (here: `ArchiveStore` with `LocalFS` now and `S3Compatible` later), write the contract tests as `func runContractTests(t *testing.T, factory func(t *testing.T) ArchiveStore)`. Each implementation calls it with its own factory. Future M2b.3.b will reuse the suite verbatim with a minio-backed factory; no duplicated assertions.

**RFC3339 with hyphens-not-colons for filename-safe timestamps**: `time.Now().UTC().Format("2006-01-02T15-04-05Z")` produces fixed-length, lex-sortable, filename-portable timestamps. Colons are illegal in Windows filenames; the dash variant works on every filesystem.

**Cross-package vec0 access requires `sqlitevec.Auto()` registration**: Tests that open a SQLite file from outside the `notebook` package must register the sqlite-vec extension before the first connection. The notebook package does this via `vecOnce.Do(func() { sqlitevec.Auto() })` in `db.go`; external packages should call the same helper from `init()` so the global SQLite auto-extension table contains the entry by the time `sql.Open(..., "?mode=ro")` runs. Duplicate registrations are safe (sqlite-vec is idempotent).

### References

- Files: `core/pkg/archivestore/{archivestore,localfs}.go` + `_test.go`, `core/pkg/archivestore/contract_test.go`, `core/pkg/archivestore/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.3 → M2b.3.a. **M2b.3.b plugs** S3Compatible into the parameterised contract suite.

---

## 2026-05-03 — M2b.3.b: S3Compatible ArchiveStore via minio-go + testcontainers-go

**PR**: [#23](https://github.com/vadimtrunov/watchkeepers/pull/23)
**Merged**: 2026-05-03 (squash commit `fafb346`)

### Context

Implemented the S3Compatible backend for ArchiveStore using minio-go/v7 and testcontainers-go/modules/minio. Extracted tarball-streaming helpers (`writeTarballStream`, `openTarballStream`) from LocalFS into `internal_tar.go` so both backends share wire-compatible code. Phase 3 executor delivered 2 commits (refactor + S3 feat) with 7 files total; 17 S3-specific sub-tests passed against a real minio Docker container. Reviewer iteration 1 converged immediately (0 blocker, 0 important, 5 nits). PR squash-merged; cascade commit `b573a95` closed M2b.3 entirely.

### Pattern

**Tarball helper extraction is the right call on the second backend, not the first**: M2b.3.a kept tar/gzip helpers private to `localfs.go`. M2b.3.b refactored them into `internal_tar.go` (`writeTarballStream`, `openTarballStream`) so both LocalFS and S3Compatible call the same helpers, guaranteeing identical wire bytes. Pattern: premature extraction after a single implementation is YAGNI; on the second implementation it becomes necessary. Timing: refactor as its own commit (M2b.3.b iter 0) so reviews separate interface concerns from helper canonicalization.

**`testcontainers-go/modules/minio` + `sync.Once` singleton for integration tests**: Official testcontainers module is safer than hand-rolled GenericContainer. Pattern: `var (testMinioOnce sync.Once; testMinioContainer *minio.Container; testMinioErr error)` with a `sharedMinioContainer(t)` helper lazily initializing on first call. Per-test bucket isolation via UUID names prevents state leakage. Ryuk reaper terminates the container on process exit (no manual `Terminate`). Skip tests on Docker-unavailable via substring matcher (`"docker daemon"`, `"connection refused"`, `"Cannot connect"`) returning `t.Skip`; no build tags needed, single `go test ./...` invocation works everywhere.

**`minio.Client.GetObject()` → `obj.Stat()` pattern for `NoSuchKey` detection**: `GetObject` returns `*minio.Object` immediately; the actual fetch happens on first `Read`. To detect missing objects up-front (mapping to `ErrNotFound`), call `obj.Stat()` first and check `minio.ToErrorResponse(err).Code == "NoSuchKey"`. Without this, the error surfaces inside the tarball reader on first `Read`, where it's harder to map cleanly to a sentinel.

**Per-test bucket isolation without manual cleanup**: Contract tests create a fresh UUID-named bucket for each invocation. No explicit teardown needed — Ryuk reaper cleans up the whole container on exit, and bucket-creation latency (~10ms) is negligible against the container startup cost. Pattern is idempotent and test-friendly.

### References

- Files: `core/pkg/archivestore/{s3,internal_tar}.go` + `s3_test.go`, `core/pkg/archivestore/{localfs,README}.go` (refactored), `go.mod` (+ minio-go/v7 v7.1.0, testcontainers-go/modules/minio v0.42.0)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.3 (now `[x]`). **M2b.4 onward** (Archive on retire / Periodic backup / Import / Audit-log) plug into the stable ArchiveStore interface.

---

## 2026-05-03 — M2b.4: Notebook ArchiveOnRetire shutdown helper

**PR**: [#24](https://github.com/vadimtrunov/watchkeepers/pull/24)
**Merged**: 2026-05-03 (squash commit `6028b53`)

### Context

Implemented `notebook.ArchiveOnRetire` as a blocking orchestrator that chains Archive→Put→LogAppend during graceful shutdown. Three integration paths existed (literal `archivestore.ArchiveStore`, interface-based bridge, defer to caller). Planner decomposed to a library helper (this TASK) with harness-wiring deferred to M2b.4-successor. Phase 3 executor delivered 1 commit (+675 LOC, 8 tests). Phase 4 iter 1 surfaced 2 important items (real goroutine leak on producer-side unblock failure; test masking the leak via `io.Copy`-before-error). Fixer resolved both in 1 commit; Phase 4 iter 2 converged.

### Pattern

**`io.Pipe` Archive→Put bidirectional unblock required**: Streaming a producer into a consumer via `io.Pipe` works only if BOTH sides can unblock each other on failure. Producer side (Archive goroutine writing): if consumer aborts early, the next `pw.Write` blocks forever. Fix: after consumer's `Put(ctx, agentID, pr)` returns with non-nil error, call `pr.CloseWithError(putErr)` from the main goroutine BEFORE draining the producer error channel. Without this, real ArchiveStore impls (S3 auth failure, ECONNREFUSED before reading) leak the producer goroutine. Pattern: any `io.Pipe`-based streaming must terminate the producer explicitly on consumer failure, not just on context cancellation.

**Test fakes that drain before checking error mask producer-blocking bugs**: `fakeStore.Put` doing `io.Copy(&buf, src)` BEFORE checking injected error makes the test pass for the wrong reason — the pipe was fully drained, so the producer completed naturally. Real impls fail BEFORE reading. Pattern: when testing `(consumer, producer)` via `io.Pipe`, the fake consumer MUST fail without consuming. Add a `failBeforeRead bool` flag; test the early-fail path.

**Local interface to break import cycles**: M2b.4 was specced to take `archivestore.ArchiveStore`, but `archivestore/*_test.go` imports `notebook` for round-trips. Adopting `archivestore.ArchiveStore` in the consumer would cycle in the test build. Solution: define a local interface in the consumer (`notebook.Storer { Put(ctx, agentID, io.Reader) (string, error) }`). Go's structural typing lets concrete `archivestore` impls satisfy it without changes. Pattern: when a downstream package needs a type from an upstream package whose tests depend on the downstream, define a local interface and let structural typing bridge.

### References

- Files: `core/pkg/notebook/{archive_on_retire,archive_on_retire_test}.go`, `core/pkg/notebook/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.4. **Future M2b.4-successor** wires this helper into the actual harness once TS-vs-Go ambiguity is resolved.

---

## 2026-05-03 — M2b.5: Notebook PeriodicBackup helper

**PR**: [#25](https://github.com/vadimtrunov/watchkeepers/pull/25)
**Merged**: 2026-05-03 (squash commit `0c0bb82`)

### Context

Implemented `notebook.PeriodicBackup` as a best-effort periodic helper for archive-on-retire lifecycle. Uses `time.NewTicker` with `time.Duration` cadence (cron deferred to M3.3) and extracted a private `archiveAndAudit(ctx, db, agentID, store, logger, eventType)` helper from the original M2b.4 inline pipeline. Refactoring preserved the M2b.4 goroutine-leak fix via `pr.CloseWithError`. Phase 3 executor delivered 1 commit (+542 LOC, 7 tests passing under `-race`). Phase 4 iter 1 converged 0/0/0 (zero blocker/important/nit). Phase 6 CI green 9/9. Phase 7 PR squash-merged.

### Pattern

**Refactor-on-second-caller, not first**: M2b.4 shipped the Archive→Put→LogAppend pipeline inline in `ArchiveOnRetire`. M2b.5 needed the same pipeline with a different EventType — extracted a private `archiveAndAudit(ctx, db, agentID, store, logger, eventType)` helper. Pattern: when a second caller of the same logic appears, refactor THEN. Premature extraction on the first caller is YAGNI; on the second it is necessary. Same pattern successfully applied in M2b.3.b (tarball-streaming helpers extraction).

**`time.NewTicker` + `select { ctx.Done, ticker.C }` for periodic best-effort jobs**: Canonical Go pattern for "fire every N, exit on cancel". `time.NewTicker(cadence)` + `defer ticker.Stop()` to avoid goroutine leak. The `select` is two-way: `<-ctx.Done()` returns `ctx.Err()` cleanly; `<-ticker.C` runs the work. Per-tick failures DO NOT exit the loop — backups are best-effort; the next tick retries. The optional `onTick` callback is called synchronously (NOT spawned) — caller's responsibility to keep it fast. `time.Ticker` drops missed ticks rather than queueing, so a slow callback can cause skipped ticks but will not pile up.

**Polling deadline tests over fixed-sleep tests for time-driven loops**: `cadence=10ms` + `sleep(30ms)` is flaky on loaded CI (only 1 tick fires when the test expected ≥2). Replace with: `cadence=25ms` + poll until counter ≥ N OR deadline (e.g. 3s). The poll exits as soon as the assertion is met OR fails the test on timeout. Robust under jitter without slowing happy-path runs. Pattern reusable for any time-driven loop's tests.

**`flakyStore` pattern for fail-then-succeed test scenarios**: To test that a periodic loop survives transient failures, give the fake an internal counter that fails on odd-numbered Put calls and succeeds on even (or any other deterministic predicate). The test polls until BOTH at-least-one-error and at-least-one-success have been observed via the onTick callback. Avoids tying the assertion to a specific tick number.

### References

- Files: `core/pkg/notebook/{periodic_backup,archive_on_retire}.go` + `_test.go`, `core/pkg/notebook/{errors.go,README.md}`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.5. **Future**: cron-expression-driven scheduling lands in M3.3 (`robfig/cron`).

---

## 2026-05-04 — M2b.6: Notebook ImportFromArchive helper

**PR**: [#26](https://github.com/vadimtrunov/watchkeepers/pull/26)
**Merged**: 2026-05-04 (squash commit `534a5fe`)

### Context

Implemented `notebook.ImportFromArchive(ctx, db, agentID, fetcher)` as a library helper that streams an archive tarball into the live Notebook via a `Fetcher` interface abstraction. Complements M2b.4/M2b.5's `Storer` interface for archive orchestration. Phase 1 planner verdict: "fits" (~3–4 files, ≤ 1 day); executor (opus) delivered 1 commit (+714 LOC, 10 tests passing under `-race`). Phase 4 iter 1 converged 0/0/0. Phase 6 CI green 9/9. Phase 7 PR squash-merged (`534a5fe`); ROADMAP M2b.6 marked `[x]` (`0e88a44`).

### Pattern

**Single-method `Fetcher` interface complements `Storer` for cross-package consumption**: M2b.4/M2b.5 use `notebook.Storer { Put(...) }`; M2b.6 introduces `notebook.Fetcher { Get(...) }`. Both are one-method interfaces — Go's "accept interfaces, return structs" idiom. Concrete `archivestore.LocalFS` and `archivestore.S3Compatible` satisfy both via structural typing without explicit declaration. Pattern: when a downstream package needs DIFFERENT methods of an upstream package's type at different sites, define separate single-method interfaces in the downstream rather than extending one or accepting the wide concrete type.

**Defer LIFO ordering matters when one resource owns another**: `ImportFromArchive` does `defer rc.Close()` THEN `defer db.Close()`. Go runs defers in reverse, so `db.Close` runs FIRST when the function returns — that's correct because `db.Import(ctx, rc)` is a method on `db` that consumes `rc`; closing `rc` first while `db.Close` is still running could leave the database mid-flush with no source. Pattern: when X depends on Y for its operation, defer Y.Close BEFORE X.Close (so X.Close runs first via LIFO).

**Import-then-audit ordering with explicit data-presence test**: When orchestrating "side effect → audit emit", the data lands BEFORE the audit. If audit fails, the test must explicitly verify the data is still in place — not just that the function returned an error. M2b.6's `TestImportFromArchive_LogAppendFails` reopens the destination notebook AFTER the audit failure and runs `assertImportedSeed` for every seed; without this assertion, a regression where Import is also rolled back on audit failure would silently break the partial-failure contract.

**Cross-package `Fetcher` compile-time check via test-only import**: `var _ Fetcher = (*archivestore.LocalFS)(nil)` in `import_from_archive_test.go` confirms structural compatibility at build time. Production `notebook` code MUST NOT import `archivestore` (cycle: archivestore tests import notebook). But test files compile separately into a different binary — they CAN import archivestore. Pattern: when verifying interface compliance across packages with a one-way cycle, put the compile-time `var _ Iface = ...` in `_test.go`, not in production code.

### References

- Files: `core/pkg/notebook/import_from_archive.go` + `_test.go`, `core/pkg/notebook/README.md` (delta)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.6. **Future**: `wk notebook import <wk> <archive>` CLI lands in M10.3 on top of this helper; auto-inheritance policy is Phase 2.

---

## 2026-05-04 — M2b.7: Notebook mutating ops emit correlated audit events

**PR**: [#27](https://github.com/vadimtrunov/watchkeepers/pull/27)
**Merged**: 2026-05-04 (squash commit `fd3caeb`)

### Context

Wired `notebook.Remember` and `notebook.Forget` to emit audit events via the Keep audit log (`keepers_log` table) when the mutation succeeds. Phase 1 planner verdict: "fits" (~5 files, ≤ 1 day); executor (opus) delivered 1 commit (+545 LOC, 8 new tests passing under `-race`; 76 total tests in package — 68 legacy preserved). Phase 4 iter 1 converged 0/0/0. Phase 6 CI green 9/9, 0 review threads. Phase 7 PR squash-merged (`fd3caeb`); ROADMAP M2b.7 marked `[x]` (`c76052b`).

### Pattern

**Functional options preserve backward compatibility for cross-cutting concerns**: `Open(ctx, agentID, opts ...DBOption)` adds variadic options WITHOUT breaking existing callers — `Open(ctx, agentID)` compiles and runs unchanged. `WithLogger(Logger)` attaches a logger that all mutating ops automatically use. Pattern: when adding a cross-cutting concern (audit, metrics, tracing) to an existing API, use functional options to ship the new feature WITHOUT breaking callsites. Nil-default behavior preserves the prior contract; opt-in for the new contract.

**Audit emit AFTER commit, never before — partial-failure shape (id, err)**: For `Remember`/`Forget`, the audit emit fires only after `tx.Commit()` returns nil — data is durable BEFORE the audit attempt. If `LogAppend` fails, return `(id, fmt.Errorf("audit emit: %w", err))` (Forget returns just `error` since there's no id). This mirrors M2b.4's `ArchiveOnRetire` contract and gives callers two pieces of information: the data IS in the DB (so don't retry the mutation) and the audit isn't (so retry just the audit emit). Pre-commit failures (validation, tx error, `ErrNotFound`) skip the audit block entirely — auditing rolled-back operations would be incorrect.

**Audit payload excludes PII and large fields**: `notebook_entry_remembered` carries only `agent_id`, `entry_id`, `category`, `created_at`. NOT `content`, NOT `embedding`, NOT `subject`. Audit log answers "what happened" not "what was stored" — the actual data is recoverable from the DB. Including 1536-float embeddings (~6 KiB each) or arbitrary user content would bloat the keepers_log table and create a PII surface. Tests carry explicit banned-field assertions to prevent regressions ("payload does NOT contain `content`").

### References

- Files: `core/pkg/notebook/{db,remember,forget}.go`, `core/pkg/notebook/mutation_audit_test.go` (new), `core/pkg/notebook/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.7. Keep is the single audit authority; Notebook has no audit surface of its own.

---

## 2026-05-04 — M2b.8: promote_to_keep helper for Watchmaster proposal flow

**PR**: [#28](https://github.com/vadimtrunov/watchkeepers/pull/28)
**Merged**: 2026-05-04 (squash commit `5bcaa80`)

### Context

Implemented `keep.PromoteToKeep(ctx, db, proposal)` as a read-only helper that emits a `notebook_promotion_proposed` audit event when a Watchmaster user proposes promoting a Notebook entry to Keep status. No side effects on the Keep database — the helper reads the entry, validates it meets Promote criteria (category, language, embedding present), and emits an audit event for orchestration downstream (M6.2 final-write). Phase 1 planner verdict: "fits" (~4–6 files, ≤ 1 day, deps M2b.1–M2b.7 closed); executor (opus) delivered 2 commits (+264 LOC in promote.go, +471 LOC in promote_test.go, +82 lines in README.md). Phase 4 iter 1 converged 0/0/5 nits. Phase 6 CI green 9/9, 0 blocking review threads. Phase 7 PR squash-merged (`5bcaa80`); ROADMAP M2b.8 marked `[x]` (`5c75bce`); entire M2b milestone now complete (`[x]`).

### Pattern

**Proposal struct shape mirrors upstream schema columns without importing upstream**: M2b.8 defines `keep.Proposal` with `AgentID`, `EntryID`, `Category`, `EmbeddingVector` — the same columns as `notebook.Entry` in `deploy/migrations/004_knowledge_chunk.sql`. Rather than importing `notebook.Entry` (which would create the same archivestore→notebook one-way-import cycle), the consumer (keep) mirrors the minimal shape it needs. Pattern: when a downstream package interfaces with an upstream schema, define a struct locally that captures only the columns you read or validate — avoid the upstream import; let the caller do the mapping if they own both.

**Two-stage promotion event taxonomy: proposal vs final-write**: M2b.8 emits `notebook_promotion_proposed` (proposal stage); M6.2 will emit `notebook_promoted_to_keep` (final-write stage). When a workflow has "request → approval → action" shape, give each stage its own event type. Pattern: audit consumers can build state machines without inferring intent from context ("did the proposal succeed? check if there's a final-write event yet") — each event type is a terminal fact at its stage.

**Embedding round-trip via binary.LittleEndian mirrors sqlitevec.SerializeFloat32**: The keep binding does not expose a public Deserialize helper. Consumer-side decoder reads 4-byte LE floats directly via `binary.LittleEndian.Uint32()` cast to `float32`. Doc-comment cites the encoding contract (`sqlitevec.SerializeFloat32` uses LE) so a future sqlite-vec major version bump triggers a visible test failure rather than silent corruption. Pattern: when a dependency's serialization contract is not explicitly public-API, doc-comment the consumer's decoding logic with a citation so the coupling is visible.

**Read-only audit emit (no transaction required)**: M2b.7 emit-after-tx-commit was about durability; M2b.8 emits-after-read because PromoteToKeep has no write side-effect. The return shape and error-handling remain identical: `(populated, fmt.Errorf("audit emit: %w", err))` vs `(fmt.Errorf("audit emit: %w", err))` per whether there's an id to return. Only the gate condition changes (post-Commit vs post-SELECT). Pattern: audit emit shape and error contract are the same regardless of write/read; only the pre-condition changes.

### References

- Files: `core/pkg/keep/promote.go`, `core/pkg/keep/promote_test.go`, `core/pkg/keep/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.8. **M2b complete**: all 8 leaves now `[x]`. Phase 1 Notebook surface is feature-complete.

---

## 2026-05-04 — M3.1: In-process event bus (pub/sub) with handler registration, ordered per-topic delivery, and backpressure

**PR**: [#29](https://github.com/vadimtrunov/watchkeepers/pull/29)
**Merged**: 2026-05-04 (squash commit `4bcb955`)

### Context

Implemented `core/pkg/eventbus/Bus` — a minimal in-process pub/sub system with per-topic worker goroutines, bounded buffered channels, sequential handler dispatch within a topic, and backpressure via blocking Publish that honors context cancellation. Phase 1 planner verdict: "fits" (handler registration + ordered delivery + backpressure are co-designed properties of one minimal Bus API; splitting would ship a half-built bus); executor (opus) delivered 1 commit (+1304 LOC, 8 ACs, 16 test cases). Phase 4 converged at iteration 2 (critical race conditions found and fixed in iter 1). Phase 6 CI green 9/9, converged iter 2 with 0 unresolved review threads. Phase 7 PR squash-merged (`4bcb955`); ROADMAP M3.1 marked `[x]` (`693d69b`).

### Pattern

**Pub/sub bus shape: per-topic worker goroutine + bounded channel = sequential dispatch with backpressure**: `Bus.Subscribe(topic, handler) (unsub, err)`, `Publish(ctx, topic, event) error`, `Close() error`. Each topic has ONE dedicated worker goroutine reading from a bounded buffered channel; it calls each handler sequentially. Publish blocks if the channel is full, respecting ctx cancellation. Close stops accepting new publishes, drains the channel, and returns errors only after all handlers have finished. Pattern: per-topic workers avoid a global lock on the dispatch path while preserving ordering within each topic; topics are independent.

**Subscribe atomicity under concurrent Close**: Subscribe must hold `b.mu` across `closed-check + topic install/lookup + ts.mu.Lock acquisition`, releasing `b.mu` only AFTER `ts.mu` is held. Anything less leaves a TOCTOU window where Close can race past the check, store `true` in `closed`, and tear down the topic — leaving a dead subscription with the contract violated. The `getOrCreateTopic` helper was insufficient; Subscribe must inline the closed-check and re-check inside the lock after acquiring `ts.mu`.

**WaitGroup.Add race-instrumentation serialization**: `sync.WaitGroup` race-instrumentation reads sema on `Add(0→1)` and writes on `Wait(0→first-waiter)`. If a goroutine calls `Add(0→1)` while another is in `Wait`, the race detector flags it even though WaitGroup operations are themselves atomic. Fix: serialize the Add side (in Publish) and the closed-flag store (in Close) through a shared mutex so both see consistent state. Without this, `-race` fails on a tight loop without any actual data races.

**Late-subscriber dispatch-time-snapshot semantics**: Snapshotting `ts.subs` at DISPATCH time (the worker reads the slice header once per envelope) gives a WEAKER guarantee than callers may assume — a subscriber added between Publish-return and dispatch-start WILL receive events whose Publish completed before Subscribe returned. To express the stronger guarantee in tests, gate the late-subscribe behind a sentinel handler that fires AFTER all prior envelopes finished dispatching (AC4 `TestBus_LateSubscriberMissesPriorEvents` uses Path A: subscribe drain BEFORE publish "A" so `drained=true` proves worker finished).

**Aggressive race-regression tests with polling-deadline assertions**: 50 goroutines × 5 publishes vs Close needs polling-deadline assertions on `runtime.NumGoroutine()` (not fixed sleeps), with explicit slack (e.g. `+4`) to absorb framework-spawned goroutines. Channel-based error collection (NOT `t.Errorf` from racer goroutines) avoids racing testing.T's internal sync.Once.

### Anti-pattern

Embedding `sync.WaitGroup` directly in the bus struct and calling `Add` across subsystem boundaries (Publish, Close) without holding a lock. Race-instrumentation will flag benign concurrency if the operations serialize against a different mutex. Solution: serialize all side effects of one subsystem through one lock.

### References

- Files: `core/pkg/eventbus/{bus,errors,doc,README}.go` + `_test.go`
- Docs: `docs/ROADMAP-phase1.md` §M3 → M3.1. Mirrors foundational pattern from `core/pkg/{notebook,archivestore,keepclient}`.

---
