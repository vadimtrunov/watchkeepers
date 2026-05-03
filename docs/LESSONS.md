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
