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

| Variable                   | Required | Default | Notes                                                                  |
| -------------------------- | -------- | ------- | ---------------------------------------------------------------------- |
| `KEEP_DATABASE_URL`        | yes      | —       | pgx-compatible Postgres DSN. Boot exits non-zero if unset or bad.      |
| `KEEP_HTTP_ADDR`           | no       | `:8080` | Listen address passed to `http.Server`.                                |
| `KEEP_SHUTDOWN_TIMEOUT`    | no       | `10s`   | Go duration; bounds `http.Server.Shutdown` on SIGINT / SIGTERM.        |
| `KEEP_TOKEN_SIGNING_KEY`   | yes      | —       | Base64-encoded HMAC-SHA256 key. Decoded bytes must be ≥ 32 bytes long. |
| `KEEP_TOKEN_ISSUER`        | yes      | —       | Expected `iss` claim on every verified capability token.               |
| `KEEP_SUBSCRIBE_BUFFER`    | no       | `64`    | Per-subscriber event channel capacity for `GET /v1/subscribe`.         |
| `KEEP_SUBSCRIBE_HEARTBEAT` | no       | `15s`   | Interval between SSE heartbeat comments on an idle subscribe stream.   |

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

Event **producers** (the outbox worker that turns
`watchkeeper.outbox` rows into `publish.Event` fan-outs) are out of
scope for M2.7.e.a and land in **M2.7.e.b**. In M2.7.e.a the stream is
driven exclusively through the in-process `publish.Publisher` API; any
event you emit through that API is delivered to matching subscribers.

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
