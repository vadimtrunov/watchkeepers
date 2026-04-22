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
