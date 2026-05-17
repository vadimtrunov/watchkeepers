# Developing Watchkeeper

This guide covers local setup, the verification matrix, and how main branch
protection is configured. Source of truth for the CI gates is
`.github/workflows/ci.yml` mirrored locally via `lefthook.yml` and `make ci`.

## Prerequisites

Pinned toolchain (see `.tool-versions`). Install via
[`mise`](https://mise.jdx.dev/) or [`asdf`](https://asdf-vm.com/), or install
each one directly.

| Tool          | Version  | Why                          |
| ------------- | -------- | ---------------------------- |
| Go            | 1.26.x   | Core services                |
| Node.js       | 24.x LTS | TS harness + tooling         |
| pnpm          | 10.33.x  | Workspace package manager    |
| golangci-lint | v2.11+   | Go linter (v2 config schema) |
| govulncheck   | latest   | Go vulnerability scan        |
| lefthook      | v2.1+    | Pre-commit hook runner       |
| gitleaks      | v8.30+   | Secret scanner               |
| hadolint      | v2.12+   | Dockerfile linter            |
| yamllint      | v1.35+   | YAML linter                  |
| shellcheck    | v0.10+   | Shell script linter          |
| sqlfluff      | v3.x     | SQL linter                   |
| goose         | v3.27.0  | Postgres migration tool      |

### macOS quickstart

```sh
brew install go node pnpm golangci-lint govulncheck lefthook gitleaks hadolint yamllint shellcheck sqlfluff
corepack enable
corepack prepare pnpm@10.33.0 --activate
```

### Linux quickstart (Debian/Ubuntu)

```sh
# Go and Node via your preferred version manager (mise, asdf, or distro package)
brew install golangci-lint govulncheck lefthook gitleaks hadolint yamllint shellcheck sqlfluff   # with Homebrew on Linux
```

## Bootstrap

```sh
make bootstrap
```

This installs pnpm dependencies, wires `lefthook` git hooks, and downloads Go
modules.

## Verifying a change locally

```sh
make ci
```

`make ci` runs the exact same gates as GitHub Actions:

- `go-ci` — `go vet`, `golangci-lint`, `go test -race -cover`, `govulncheck`
- `ts-ci` — `prettier --check`, `tsc --noEmit`, `eslint`, `vitest --coverage`, `osv-scanner`
- `sql-ci` — `sqlfluff lint`
- `migrate-ci` — `goose` up / down / round-trip against Postgres 16 (see
  **Migrations** below). Not in the `make ci` composite because it needs a
  reachable database; run it manually with `WATCHKEEPER_DB_URL` set.
- `docker-ci` — `hadolint`
- `security-ci` — `gitleaks detect`
- `meta-ci` — `markdownlint`, `yamllint`, `commitlint`

`make help` lists every target.

## Migrations

Database migrations are driven by [`goose`](https://github.com/pressly/goose),
pinned via `GOOSE_VERSION` in the `Makefile` (currently `v3.27.0`). The tool
is invoked through `go run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)`,
so no global install is required — the pinned version is the source of truth.

Targets that connect to Postgres (`migrate-up`, `migrate-down`,
`migrate-status`, and `migrate-round-trip`) read the connection string from a
single env var, `WATCHKEEPER_DB_URL`, and error out with a helpful message when
it is unset. `migrate-create` only scaffolds a file and does not require a
database connection. Never hard-code credentials in the repo; set the URL from
your shell or a local `.envrc` (git-ignored):

```sh
export WATCHKEEPER_DB_URL='postgres://watchkeeper:<password>@localhost:5432/watchkeeper?sslmode=disable'
```

### Make surface

| Target                            | Purpose                                                    |
| --------------------------------- | ---------------------------------------------------------- |
| `make migrate-up`                 | Apply all pending migrations. Idempotent (no-op when up).  |
| `make migrate-down`               | Roll back the most recent applied migration.               |
| `make migrate-status`             | Show applied / pending migrations.                         |
| `make migrate-create NAME=<slug>` | Scaffold a new timestamped SQL migration file.             |
| `make migrate-round-trip`         | Up -> down-to-0 -> up; assert schema dumps match.          |
| `make migrate-test`               | Run schema smoke assertions against the migrated database. |

### Authoring a migration

1. `make migrate-create NAME=add_keepers_table` — generates
   `deploy/migrations/<timestamp>_add_keepers_table.sql` with `-- +goose Up`
   and `-- +goose Down` sections scaffolded.
2. Fill in both sections. Prefer narrow, reversible changes; irreversible
   steps (e.g. data backfills) must be called out in the PR description.
3. Run `sqlfluff lint deploy/migrations` — or `make sql-lint` — locally.
4. Apply against a disposable Postgres 16, then run
   `make migrate-round-trip` to exercise both `Up` and `Down`:

   ```sh
   docker run --rm -d \
     -e POSTGRES_PASSWORD=postgres \
     -p 5432:5432 postgres:16-alpine
   export WATCHKEEPER_DB_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
   make migrate-round-trip
   ```

5. Commit the migration alongside the code that requires it.

### CI gate

The `Migrate CI` job in `.github/workflows/ci.yml` stands up
`postgres:16-alpine` as a service container, sets `WATCHKEEPER_DB_URL` to a
throwaway value, and runs `scripts/test-migrate.sh` (happy path, idempotency,
round-trip, broken-SQL negative case) followed by `make migrate-test` (schema
smoke: happy-path insert chain, UNIQUE rejection, FK rejection). This job
must be added to the required-checks list (see **Branch protection** below).

### Schema overview

The `watchkeeper` schema holds the Phase 1 Keep foundation. `organization`
is the tenant container; `human` rows are members and fill "lead" roles
relationally. A `manifest` owns `manifest_version` snapshots (`system_prompt`,
`tools`, `authority_matrix`, `knowledge_sources`, `personality`, `language`,
uniquely keyed on `(manifest_id, version_no)`). A `watchkeeper` is the
runtime agent tied to a manifest, a lead human, and an active
`manifest_version`. A `watch_order` is a task with `priority` and lifecycle
`status`. `keepers_log` is the append-only audit log (trigger-enforced).
`knowledge_chunk` stores `vector(1536)` embeddings behind an HNSW
`vector_cosine_ops` index. `outbox` stages events for the future publisher
worker. The scoped tables (`watch_order`, `knowledge_chunk`) carry a `scope`
column and run under FORCE ROW LEVEL SECURITY; policies evaluate
`current_setting('watchkeeper.scope', true)`.

## Keep service

`core/cmd/keep` is the Keep HTTP service process introduced in M2.7.a. It
loads config from env vars, opens a `pgxpool.Pool` against
`KEEP_DATABASE_URL`, and exposes `GET /health` until it receives
`SIGINT` / `SIGTERM`, at which point it drains in-flight requests with a
configurable timeout and closes the pool.

### Security posture (Phase 1)

Per-tenant authorization in the keep service is enforced at the handler
layer via `claim.OrganizationID`, plumbed through `auth.Claim` and the
capability broker by **M3.5.a** (foundation in **M3.5.a.1**, handler
wire-up in **M3.5.a.2** and **M3.5.a.3.2**). Five mutating handlers
carry the wire-up: `handleInsertHuman`, `handleSetWatchkeeperLead`,
`handleUpdateWatchkeeperStatus`, `handleInsertWatchkeeper`, and
`handlePutManifestVersion`. Concretely:

- Token-bearing requests carry `claim.OrganizationID` extracted from the
  JWT `org_id` field. The verifier still accepts pre-M3.5.a.1 tokens
  with no `org_id` for rolling-deploy safety, but each of the five keep
  handlers above rejects an empty value with
  `403 organization_required` before any DB transaction opens.
- `POST /v1/humans` cross-checks the request-body `organization_id`
  against `claim.OrganizationID`; a mismatch returns
  `403 organization_mismatch` before any DB work runs.
- `PATCH /v1/watchkeepers/{id}/lead` filters the target row through a
  subquery on `watchkeeper.human` keyed by the claim's tenant
  (`watchkeeper.watchkeeper` carries no `organization_id` column of its
  own — see migration 002), so a cross-tenant caller's UPDATE matches
  zero rows and surfaces as `404 not_found`.
- `PATCH /v1/watchkeepers/{id}/status` filters the
  `SELECT … FOR UPDATE` step through a JOIN on `watchkeeper.human`
  keyed by the claim's tenant; a cross-tenant caller's SELECT returns
  no rows and surfaces as `404 not_found` without ever reaching the
  UPDATE branch.
- `POST /v1/watchkeepers` wraps the INSERT in
  `INSERT … SELECT … WHERE EXISTS (SELECT 1 FROM watchkeeper.human
WHERE id = $lead_human_id AND organization_id = $claim_org)`, so a
  caller cannot anchor a new watchkeeper at another tenant's human;
  the cross-tenant case produces no row through `RETURNING` and
  surfaces as `404 not_found`.
- `PUT /v1/manifests/{manifest_id}/versions` wraps the INSERT in
  `INSERT … SELECT … WHERE EXISTS (SELECT 1 FROM watchkeeper.manifest
WHERE id = $manifest_id AND organization_id = $claim_org)` so a
  caller cannot append a version to another tenant's manifest; the
  cross-tenant case produces no row through `RETURNING` and surfaces
  as `404 not_found`.

**Defense-in-depth notes.** RLS on `manifest` and `manifest_version`
landed in **M3.5.a.3.1** (migration 013): per-role `ENABLE + FORCE
ROW LEVEL SECURITY` keyed off a `watchkeeper.org` session GUC.
`db.WithScope` sets the GUC from `claim.OrganizationID`, so even if
the `handlePutManifestVersion` `WHERE EXISTS` filter regressed,
Postgres would still refuse to insert across tenants (the `WITH
CHECK` clause rejects, `nullif(..., '')::uuid` makes empty GUC
fail-closed). The application-layer filter exists alongside RLS to
keep the 404 surface deterministic (instead of an RLS-level error
path) and to short-circuit the round-trip on legacy claims.

A network-boundary that admits only authenticated operators is no
longer required for tenant safety on any of the five wired handlers
above, but is still **recommended** as defense-in-depth alongside
the existing `KEEP_TOKEN_*` controls. The per-org RLS policies on
`human` / `watchkeeper` mentioned in ROADMAP-phase1 §M3 M3.5.a remain
a follow-up backstop; the handler-layer enforcement above closes the
M4.4 cross-tenant gap on all five mutating write handlers.

### Protocol choice: HTTP over gRPC

M2.7.a records the Keep protocol decision that
[LESSON M2.1.a](LESSONS.md) left open ("protocol choice open"). We chose
**HTTP + JSON** for Phase 1 because:

- **Zero extra runtime deps for Phase 1.** The stdlib `net/http` mux is
  enough for `/health` and the handful of endpoints that land in
  M2.7.c–e (`search`, `store`, `subscribe`, `log_*`, `*_manifest`).
  gRPC would add a protobuf toolchain and a second wire format without
  a Phase 1 consumer asking for it.
- **Protocol-neutral DDL already in place.** M2.1.a intentionally
  shipped portable column types (`uuid`, `text`, `timestamptz`, `jsonb`)
  so either protocol could land later. HTTP lets us honour that
  openness without blocking on gRPC tooling.
- **Ops parity.** Watchkeeper operators are expected to curl, trace,
  and port-forward Keep during rollout. HTTP is inspectable without a
  bespoke client.
- **Reversible.** If a gRPC requirement shows up later (e.g. typed
  streaming manifests), we can co-host a gRPC server on a second
  listener; the `pgxpool.Pool` + config surface do not care.

When the trade-off flips (schema-owned streaming, aggressive cross-service
typing), revisit and either add gRPC alongside HTTP or migrate. Any change
goes back through the `rdd` loop with a new LESSON entry.

### Env vars

| Variable                    | Required | Default | Notes                                                                                                |
| --------------------------- | -------- | ------- | ---------------------------------------------------------------------------------------------------- |
| `KEEP_DATABASE_URL`         | yes      | —       | pgx-compatible Postgres DSN. Boot exits non-zero if unset or bad.                                    |
| `KEEP_HTTP_ADDR`            | no       | `:8080` | Listen address passed to `http.Server`.                                                              |
| `KEEP_SHUTDOWN_TIMEOUT`     | no       | `10s`   | Go duration; bounds `http.Server.Shutdown` on SIGINT / SIGTERM.                                      |
| `KEEP_TOKEN_SIGNING_KEY`    | yes      | —       | Base64-encoded HMAC-SHA256 key. Decoded bytes must be ≥ 32 bytes long.                               |
| `KEEP_TOKEN_ISSUER`         | yes      | —       | Expected `iss` claim on every verified capability token.                                             |
| `KEEP_SUBSCRIBE_BUFFER`     | no       | `64`    | Per-subscriber event channel capacity for `GET /v1/subscribe`.                                       |
| `KEEP_SUBSCRIBE_HEARTBEAT`  | no       | `15s`   | Interval between SSE heartbeat comments on an idle subscribe stream.                                 |
| `KEEP_OUTBOX_POLL_INTERVAL` | no       | `1s`    | How often the outbox worker polls `watchkeeper.outbox` for unpublished rows. Min `100ms`, max `60s`. |

### Build and run locally

```sh
# Compile into ./bin/keep
make keep-build

# Generate a 32-byte random signing key (one-shot; rotate by replacing the
# env var and restarting the binary — no mid-flight rotation yet).
export KEEP_TOKEN_SIGNING_KEY="$(openssl rand -base64 32)"
export KEEP_TOKEN_ISSUER='keep-dev'

# Run against a local Postgres (migrations applied via `make migrate-up`).
export KEEP_DATABASE_URL='postgres://watchkeeper:<password>@localhost:5432/watchkeeper?sslmode=disable'
make keep-run
```

`make keep-run` passes `KEEP_*` through per-target `export` (see
LESSON M2.6) so user-supplied values reach the shell as env literals, never
as Makefile variable expansions. In another terminal:

```sh
curl -fsS http://localhost:8080/health
# {"status":"ok"}
```

Send `SIGTERM` (or hit `Ctrl+C`) to trigger graceful shutdown.

### Capability-token contract

Every `/v1/*` route requires `Authorization: Bearer <token>`; `/health`
stays open. Tokens are compact JWT-like strings signed with HS256:

```text
base64url(header) . base64url(payload) . base64url(hmac-sha256(key, header+"."+payload))
```

Fixed header: `{"alg":"HS256","typ":"JWT"}`. Payload is the verified
`auth.Claim` plus standard `exp` / `iat`:

```json
{
  "sub": "watchkeeper-or-human-uuid",
  "scope": "org | user:<uuid> | agent:<uuid>",
  "iss": "<matches KEEP_TOKEN_ISSUER>",
  "exp": 1714000000,
  "iat": 1713999700
}
```

The `scope` claim drives the RLS role switch. `org` uses `wk_org_role`;
the `user:` and `agent:` prefixes route to `wk_user_role` and
`wk_agent_role` respectively. Any other shape returns
`401 {"error":"unauthorized","reason":"bad_scope"}`.

Tokens are minted by the core capability broker (M3.5). For local
development and tests, `core/internal/keep/auth.TestIssuer` issues tokens
against the same signing key — never use it outside tests.

To rotate the signing key, replace `KEEP_TOKEN_SIGNING_KEY` and restart
the process; existing tokens will fail signature verification immediately.

### Endpoint contracts

All three read endpoints share the same auth envelope. Errors are always
JSON. Responses mirror the column names from the `watchkeeper` schema.

#### `POST /v1/search`

Cosine-distance KNN over `watchkeeper.knowledge_chunk`. Row visibility is
RLS-filtered by the token's scope.

```sh
curl -fsS -X POST http://localhost:8080/v1/search \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"embedding": [0.10, 0.20, 0.30], "top_k": 10}'
```

> **Note:** production embeddings are 1536-dimensional; the example above uses three
> values for readability only.

`top_k` is clamped to `[1, 50]`; zero or negative values return `400`.

#### `GET /v1/manifests/{manifest_id}`

Returns the `manifest_version` row with the highest `version_no`. `404`
with body `{"error":"not_found"}` when no version exists.

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/manifests/<manifest-uuid>
```

#### `GET /v1/keepers-log?limit=<n>`

Tail the append-only audit log in `created_at DESC` order. `limit`
defaults to `50` and is capped at `200`; zero or non-numeric values
return `400`.

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" \
  'http://localhost:8080/v1/keepers-log?limit=100'
```

#### `POST /v1/knowledge-chunks`

Insert a `knowledge_chunk` row under the scope of the token. `scope` is
token-bound — clients cannot override it. `subject` is optional;
`content` and `embedding` are required. `embedding` must be exactly 1536
floats (matching the `vector(1536)` column); any other dimension is
rejected. Body is capped at 1 MiB.

```sh
curl -fsS -X POST http://localhost:8080/v1/knowledge-chunks \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"subject":"doc-42","content":"agent handover notes","embedding":[0.10,0.20,0.30]}'
# -> 201 {"id":"<uuid>"}
```

Status codes: `201` on insert, `400` with `missing_content` /
`missing_embedding` / `invalid_embedding` (not exactly 1536 floats) /
`invalid_body`, `401` as above, `413 request_too_large`,
`415 unsupported_media_type`, `500 store_failed`.

#### `POST /v1/keepers-log`

Append one event to the audit log. `event_type` is required;
`correlation_id` and `payload` are optional. Actor columns
(`actor_watchkeeper_id` / `actor_human_id`) are stamped server-side from
the token's scope — `agent:<uuid>` fills `actor_watchkeeper_id`,
`user:<uuid>` fills `actor_human_id`, `org` leaves both NULL. Unknown
JSON fields (including `actor_*`) are rejected with 400. Body is capped
at 1 MiB.

```sh
curl -fsS -X POST http://localhost:8080/v1/keepers-log \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"event_type":"watchkeeper_spawned","payload":{"note":"ready"}}'
# -> 201 {"id":"<uuid>"}
```

Status codes: `201` on append, `400` with `missing_event_type` /
`invalid_body` / `invalid_scope_uuid` / `invalid_correlation_id`
(malformed UUID in `correlation_id`), `401` as above,
`413 request_too_large`, `415 unsupported_media_type`,
`500 log_append_failed`.

#### `PUT /v1/manifests/{manifest_id}/versions`

Insert a new `manifest_version` row for the target manifest.
`version_no` must be `> 0`; `system_prompt` is required. `tools`,
`authority_matrix`, `knowledge_sources`, `personality`, `language` are
optional. A duplicate `(manifest_id, version_no)` returns 409
`version_conflict` without leaking raw Postgres text. Body is capped at
1 MiB.

```sh
curl -fsS -X PUT "http://localhost:8080/v1/manifests/<manifest-uuid>/versions" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"version_no":3,"system_prompt":"you are a watchkeeper","personality":"curious","language":"en"}'
# -> 201 {"id":"<uuid>"}
```

Status codes: `201` on insert, `400` with `missing_manifest_id` /
`invalid_manifest_id` (malformed UUID in path) / `invalid_version_no` /
`missing_system_prompt` / `invalid_body`, `401` as above, `409
version_conflict`, `413 request_too_large`, `415 unsupported_media_type`,
`500 put_manifest_version_failed`.

#### `GET /v1/subscribe`

Server-Sent Events stream of capability-scoped events. The endpoint runs
under the same `AuthMiddleware` as the rest of `/v1/*`; missing or
invalid tokens return `401` with the stable reason codes documented
above, and non-`GET` methods return `405`.

Response headers on a successful stream:

- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `Connection: keep-alive`

Scope semantics are **strict equality** against the token's verified
`claim.Scope`: a subscriber with `org` receives only events whose
`scope` is `org`; a subscriber with `user:<uuidA>` receives only events
whose `scope` is `user:<uuidA>` (never `user:<uuidB>`, never `org`,
never `agent:*`). No hierarchy widening.

Each event is framed as

```text
id: <uuid>
event: <event_type>
data: <payload-json>

```

(blank line after `data:`). An idle stream emits an SSE comment
heartbeat (`:\n\n`) every `KEEP_SUBSCRIBE_HEARTBEAT` (default `15s`) so
intermediaries and the TCP keepalive engine stay honest.

```sh
curl -N -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/subscribe
```

The endpoint is idempotent on reconnect: a client that drops and
re-subscribes simply receives new events after the reconnect. A slow
subscriber whose per-connection buffer fills is dropped (stream closed)
so other subscribers are never held up; the standard client response is
to reconnect.

Event **producers** are the outbox worker (`core/internal/keep/publish/outbox.go`)
introduced in M2.7.e.b. The worker polls `watchkeeper.outbox` every
`KEEP_OUTBOX_POLL_INTERVAL` (default `1s`) for rows where `published_at IS NULL`,
converts each row to a `publish.Event`, calls `reg.Publish` for SSE fan-out to
matching subscribers (exact `scope` match), and stamps `published_at = now()` in
the same transaction on success. A failed publish leaves the row for the next tick.

The worker starts in a goroutine managed by `server.Run` and is cancelled before
`reg.Close()` during graceful shutdown, preserving the ordering:

```text
cancel(workerCtx) → workerDone wait → reg.Close() → httpSrv.Shutdown
```

To insert an outbox row for testing (owner role, migrations 001–009 applied):

```sql
INSERT INTO watchkeeper.outbox (aggregate_type, aggregate_id, event_type, payload, scope)
VALUES ('watchkeeper', gen_random_uuid(), 'watchkeeper.spawned', '{}', 'org');
```

Subscribers with `scope = 'org'` will receive the event within one poll interval.

### Docker image

```sh
docker build -f deploy/Dockerfile.keep -t keep:local .
docker run --rm -p 8080:8080 \
  -e KEEP_DATABASE_URL='postgres://postgres:postgres@host.docker.internal:5432/postgres?sslmode=disable' \
  keep:local
```

The image is multi-stage
(`golang:1.26-alpine` → `gcr.io/distroless/static-debian12:nonroot`),
runs as `nonroot:nonroot`, and is covered by the same hadolint and
license-scan CI gates as `deploy/Dockerfile`.

### Integration tests

The binary-boot suite lives in `core/cmd/keep/integration_test.go`,
`core/cmd/keep/read_integration_test.go`,
`core/cmd/keep/write_integration_test.go`, and
`core/cmd/keep/subscribe_integration_test.go` (all four build tag
`integration`) and requires a reachable Postgres 16 with every
migration (001..008) applied. Use `make keep-integration-test`:

```sh
export KEEP_INTEGRATION_DB_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
make migrate-up
make keep-integration-test
```

The Make target enforces the env guard before invoking
`go test -tags=integration -race -v ./core/cmd/keep/...` so a mistyped
DSN fails loudly. CI runs the same target under the `Keep Integration
CI` job.

## LLM provider

The harness ships two concrete `LLMProvider` implementations plus a
`FakeProvider` for tests. `ClaudeCodeProvider` talks to the Anthropic
Messages API directly (API key required). `ClaudeAgentProvider` wraps
`@anthropic-ai/claude-agent-sdk` and can run against either an API key
OR a local `claude` CLI Pro/Max subscription — the SDK auto-detects
whichever credential is present. `FakeProvider` is synchronous,
deterministic, and has no network dependency; the full test suite uses
it exclusively. Adding a fourth provider means implementing the four
`LLMProvider` methods, registering an entry in `ProviderCase[]` in the
conformance suite, and wiring the new provider into the boot-time
selection path (`harness/src/index.ts` `buildProviderFromSecrets`).
Documentation and any provider-specific test fixtures usually follow.

### Selecting a provider

The env var `WATCHKEEPER_LLM_PROVIDER` selects which concrete
implementation `buildProviderFromSecrets` in `harness/src/index.ts`
constructs at boot.

| Value                     | Provider              | Auth modes supported                       |
| ------------------------- | --------------------- | ------------------------------------------ |
| `anthropic-api` (default) | `ClaudeCodeProvider`  | API key only                               |
| `claude-agent`            | `ClaudeAgentProvider` | API key OR local `claude` CLI subscription |

When the variable is unset or empty the harness falls back to
`anthropic-api`. An unrecognised value falls back to `anthropic-api`
and emits a `WARN` line on stderr so a configuration typo surfaces in
the boot log rather than silently picking the wrong provider.

The API-key path (`anthropic-api`) calls the raw Anthropic Messages
API; `ANTHROPIC_API_KEY` (read through the secrets seam — see
`harness/src/secrets/env.ts`) must be present or the harness boots in
degraded mode with LLM methods unavailable. The subscription path
(`claude-agent`) does not require an API key: when `ANTHROPIC_API_KEY`
is absent the Agent SDK looks for a locally authenticated `claude` CLI
session. API key and subscription can coexist — the key wins when both
are present.

### Tool bridge

`ClaudeAgentProvider` preserves the outbound-tool contract of the
`LLMProvider` interface via an in-process MCP stub server combined with
an iterator interceptor: when the SDK yields an assistant message
containing `tool_use` blocks the interceptor captures them, calls
`iter.interrupt()`, and returns a single-turn result. The runtime
still owns tool execution; the harness never routes a real tool call
into the SDK's own handler dispatch. A sentinel-tagged error in the stub
MCP handler acts as belt-and-suspenders detection for any race between
the assistant message arrival and the interrupt. Full design rationale
is in
`docs/superpowers/specs/2026-05-17-m5-7-c-tool-bridge-design.md`.

### Cost metadata

`ClaudeAgentProvider` surfaces the following keys in `Usage.metadata`
for cost analysis:

- `cacheReadInputTokens` — sum of cache-read tokens across all SDK
  sub-turns (omitted when zero).
- `cacheCreationInputTokens` — sum of cache-creation tokens across all
  SDK sub-turns (omitted when zero).
- `model:<name>` — per-model breakdown as
  `"<inputTokens>/<outputTokens>/<costUSD>"`. The cost value uses
  `toFixed(9)` (nine decimal places) to prevent numbers like `1e-7`
  collapsing to scientific notation in the JSON wire format.

### Known limitations

- `ClaudeAgentProvider.countTokens` is not implemented. Calling it
  raises a `providerUnavailable` error. The conformance suite marks it
  via `skipMethods: ["countTokens"]` so the gap is visible to
  maintainers without flaking the test run.
- Inbound `role=tool` message folding (feeding tool results back to the
  model in a multi-turn conversation) is currently skipped in both
  `ClaudeCodeProvider` and `ClaudeAgentProvider` — the cross-cutting fix
  is tracked in the source via an in-code comment marker, not exposed at
  the provider surface yet.

## Provisioning the dev Slack workspace

ROADMAP §M4.3 wires a parent Slack app via Slack's Manifest API. The
parent app is the long-lived configuration root; child apps that
individual Watchkeepers run as land in M4.4+ via the same adapter
surface.

### One-shot operator prerequisites

Performed manually in the Slack admin console — the bootstrap script
does NOT mediate these:

1. Create a dev Slack workspace (free tier suffices) and admin yourself
   into it.
2. From <https://api.slack.com/apps> → "Your Apps" → "New App" → "From
   an app manifest", create a TINY placeholder app whose only purpose
   is to host the **app configuration token** (see step 4). Slack's
   bootstrap is chicken-and-egg here: you need an app to mint a config
   token, and you need a config token to call `apps.manifest.create`.
3. In the placeholder app, navigate to "Settings" → "Manage app
   configuration tokens" → "Generate Token". Slack returns an
   `xoxe-*`-prefixed token. This is the single secret the bootstrap
   script consumes.
4. Export the token in your shell:

   ```sh
   export WATCHKEEPER_SLACK_CONFIG_TOKEN='xoxe-...'
   ```

   The script reads this env var via the secrets interface stub
   (`core/pkg/secrets`); future milestones will swap the env-backed
   `SecretSource` for a vault-backed implementation without changing
   the script.

### Running the bootstrap

```sh
make spawn-dev-bot
```

The target builds `bin/spawn-dev-bot`, validates the
`WATCHKEEPER_SLACK_CONFIG_TOKEN` env var, calls Slack's
`apps.manifest.create` with the YAML at
`deploy/slack/dev-bot-manifest.yaml`, and writes the returned
credentials (client_id, client_secret, signing_secret,
verification_token) to a JSON file at
`.omc/secrets/dev-bot-credentials.json` (mode `0600`). The credentials
are NEVER printed to stdout / stderr / logs — the file at that path is
the single observation point.

Override the manifest or output path via env:

```sh
SPAWN_DEV_BOT_MANIFEST=path/to/manifest.yaml \
SPAWN_DEV_BOT_CREDENTIALS_OUT=path/to/credentials.json \
make spawn-dev-bot
```

### Dry-run

Validate a manifest WITHOUT contacting Slack or reading the
configuration token:

```sh
make spawn-dev-bot-dry-run
```

The script prints the JSON body it would send to
`apps.manifest.create` to stdout and exits 0. Useful as a CI gate
against malformed manifests.

### Ingesting the credentials

Phase 1's secrets interface is read-only (`secrets.SecretSource.Get`).
The credentials file is therefore a structured JSON document the
operator pipes into their own secrets store (vault, AWS SSM,
1Password CLI):

```sh
jq -r .signing_secret < .omc/secrets/dev-bot-credentials.json | \
  vault kv put secret/watchkeeper/slack/parent signing_secret=-
```

When Phase 2+ adds a vault-backed `SecretSink`, the bootstrap script
will route credentials directly via the new interface and the JSON
file will become an optional debug affordance.

### Verification gates

- `make spawn-dev-bot-dry-run` validates the manifest in CI without
  any external dependency (ROADMAP §M4 → M4.3 verification slot —
  partial; live `make spawn-dev-bot` requires a provisioned dev
  workspace).
- `go test -race ./core/cmd/spawn-dev-bot/...` includes a binary-grep
  test (`TestSpawnDevBot_BinaryHasNoTokenLeaks`) that builds the
  binary and asserts no `xoxe-* / xoxb-* / xoxp-* / xapp-*` token
  prefixes appear in the resulting bytes — ROADMAP bullet 279
  (parent-app credentials never leave the secrets interface).

## Pre-commit hooks

`lefthook` runs a subset of the above on staged files only, so local feedback
is fast but parity-preserving. Disable temporarily with `LEFTHOOK=0 git commit`.

## Coverage thresholds

- Go: 60% (env `GO_COVERAGE_MIN` overrides locally).
- TypeScript: 60% lines/functions/branches/statements (`vitest.config.ts`).

Thresholds ratchet up as real code lands per roadmap milestones.

## Branch protection (`main`)

Apply these settings manually via GitHub → Settings → Rules → Rulesets (admin
required):

- **Require a pull request before merging**
  - Require at least 1 approval
  - Dismiss stale approvals on new commits
  - Require review from Code Owners (once `CODEOWNERS` lands)
- **Require status checks to pass before merging** — mark each as required:
  - `Go CI`
  - `TypeScript CI`
  - `SQL CI`
  - `Migrate CI`
  - `Docker CI`
  - `Security CI`
  - `Meta CI`
  - Require branches to be up to date before merging
- **Require linear history** (disallows merge commits; use squash or rebase)
- **Require conversation resolution before merging**
- **Block force pushes** on `main`
- **Restrict deletions** of `main`
- **Require signed commits** (recommended, optional for Phase 1)

Renovate and Dependabot PRs must also satisfy these checks before auto-merge.

## Adding a new lint / gate

1. Add the tool and its config to the repository.
2. Wire it into `Makefile` under the appropriate composite target
   (`lint`, `test`, `secrets-scan`, `deps-scan`).
3. Mirror the invocation in `lefthook.yml` (staged-files only) and in
   `.github/workflows/ci.yml` (full repository).
4. Add the job name to the required-checks list above.
5. Document any developer-facing prerequisites in this file.

The cross-cutting constraint from the roadmap: **every CI gate must have a
local equivalent via `lefthook` (staged-files subset) and/or `make ci`
(full-repo)** — no undocumented local-only or CI-only gates.

## Troubleshooting

- `golangci-lint: go1.26 is higher than the tool's Go version` — upgrade
  `golangci-lint` to v2.11+ (`brew upgrade golangci-lint`).
- `pnpm: command not found` — run `corepack enable && corepack prepare pnpm@10.33.0 --activate`.
- `lefthook: command not found` — `brew install lefthook` then
  re-run `make bootstrap`.
- `gitleaks: secret detected` — either fix the committed secret or, if it is
  a false positive, add a targeted allowlist entry in `.gitleaks.toml` with a
  comment explaining why.
