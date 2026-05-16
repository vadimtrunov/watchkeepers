SHELL := /bin/bash
.DEFAULT_GOAL := help
.ONESHELL:
.SHELLFLAGS := -eu -o pipefail -c

# ---------------------------------------------------------------------------
# Watchkeeper — Makefile is the only supported operator and dev entry point.
# Every operational and dev workflow is a `make <target>`.
# ---------------------------------------------------------------------------

GO                ?= go
PNPM              ?= pnpm
GOLANGCI_LINT     ?= golangci-lint
GOVULNCHECK       ?= govulncheck
GITLEAKS          ?= gitleaks
LEFTHOOK          ?= lefthook
HADOLINT          ?= hadolint
MARKDOWNLINT      ?= pnpm exec markdownlint-cli2
YAMLLINT          ?= yamllint
SHELLCHECK        ?= shellcheck
SQLFLUFF          ?= sqlfluff
COMMITLINT        ?= pnpm exec commitlint

# Pinned migration tool — see docs/DEVELOPING.md "Migrations".
# Invoked via `go run ...@$(GOOSE_VERSION)` so no global install is required
# and the version stays reproducible without polluting $GOBIN.
GOOSE_VERSION     ?= v3.27.0
GOOSE             ?= $(GO) run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)
MIGRATIONS_DIR    ?= deploy/migrations

GO_COVERAGE_MIN   ?= 60

HAS_GO_PACKAGES   := $(shell test -f go.mod && echo yes || echo no)
HAS_PNPM          := $(shell command -v $(PNPM) >/dev/null 2>&1 && echo yes || echo no)

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z0-9_.\/-]+:.*## / { printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Bootstrap / env
# ---------------------------------------------------------------------------

.PHONY: bootstrap
bootstrap: ## Install dev dependencies and git hooks
	@echo ">> bootstrap: installing pnpm deps"
	@if [ "$(HAS_PNPM)" = "yes" ]; then $(PNPM) install --frozen-lockfile=false; fi
	@echo ">> bootstrap: installing lefthook hooks"
	@$(LEFTHOOK) install
	@echo ">> bootstrap: downloading Go modules"
	@if [ "$(HAS_GO_PACKAGES)" = "yes" ]; then $(GO) mod download; fi

.PHONY: tools-check
tools-check: ## Verify required toolchain is installed
	@missing=0; \
	for tool in $(GO) $(PNPM) $(GOLANGCI_LINT) $(GITLEAKS) $(LEFTHOOK); do \
	  if ! command -v $$tool >/dev/null 2>&1; then \
	    echo "MISSING: $$tool"; missing=1; \
	  fi; \
	done; \
	if [ $$missing -ne 0 ]; then echo "Install missing tools, then re-run."; exit 1; fi

# ---------------------------------------------------------------------------
# Composite targets
# ---------------------------------------------------------------------------

.PHONY: ci
ci: fmt-check lint build test secrets-scan deps-scan license-scan ## Run the full CI matrix locally (mirrors GitHub Actions)

.PHONY: fmt-check
fmt-check: ## Run prettier --check (CI parity for the ts-ci format gate)
	@$(PNPM) exec prettier --check .

.PHONY: lint
lint: go-vet go-lint ts-lint sql-lint docker-lint md-lint yaml-lint shell-lint ## Run every linter

.PHONY: fmt
fmt: go-fmt ts-fmt ## Format Go and TypeScript sources

.PHONY: test
test: go-test ts-test ## Run all test suites

.PHONY: build
build: go-build ts-build ## Build all artifacts

.PHONY: clean
clean: ## Remove build artifacts and caches
	@rm -rf bin dist coverage coverage.out coverage.html node_modules/.cache .turbo .vitest-cache

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

.PHONY: go-fmt
go-fmt: ## Format Go sources with gofumpt
	@$(GO) run mvdan.cc/gofumpt@latest -l -w .

.PHONY: go-lint
go-lint: ## Run golangci-lint
	@$(GOLANGCI_LINT) run ./...

.PHONY: go-vet
go-vet: ## Run go vet
	@$(GO) vet ./...

.PHONY: go-test
go-test: ## Run Go tests with race detector and coverage
	@$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	@coverage=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {print substr($$3, 1, length($$3)-1)}'); \
	printf ">> Go coverage: %s%% (minimum %s%%)\n" "$$coverage" "$(GO_COVERAGE_MIN)"; \
	awk -v c="$$coverage" -v m="$(GO_COVERAGE_MIN)" 'BEGIN { if (c+0 < m+0) { print "coverage below threshold"; exit 1 } }'

# Gated behind the `benchmark` build tag (see core/pkg/notebook/recall_bench_test.go)
# so the default test path NEVER seeds 10k rows. Runs both the latency-budget
# assertion (TestRecallLatencyP99WithinBudget) and the raw ns/op bench
# (BenchmarkRecallAt10k). Implements ROADMAP-phase1.md §M2b bullet 216.
#
# `-race` is intentionally omitted from this target: the race detector adds
# ~5× wall-clock overhead and would invalidate the latency measurement that
# is the entire point of the benchmark. Race coverage for the same Recall
# code path lives in the default `go-test` target (unit tests, `-race`).
.PHONY: notebook-bench
notebook-bench: ## Run the gated 10k-entry Notebook recall latency benchmark (M2b bullet 216)
	@$(GO) test -tags=benchmark -bench=BenchmarkRecallAt10k -run=TestRecallLatencyP99WithinBudget -benchmem ./core/pkg/notebook/...

# Gated behind the `loadtest` build tag (see
# core/pkg/messenger/slack/ratelimiter_load_test.go) so the default test
# path NEVER spins up the 128-goroutine fleet or pays the ~3 s wall-clock
# budget the real-clock Wait assertion needs. Runs the three
# TestRateLimiterLoad_* budget assertions plus the two
# BenchmarkRateLimiter_Tier2_* trend benchmarks. Implements
# ROADMAP-phase1.md §10 DoD Closure Plan item B3.
#
# `-race` is omitted for the same reason as notebook-bench: the race
# detector adds ~5× wall-clock overhead and would invalidate the
# throughput projection the real-clock Wait test asserts against. Race
# coverage for the same RateLimiter code path lives in the default
# `go-test` target via ratelimiter_test.go's TestRateLimiter_ConcurrentAllow.
.PHONY: ratelimiter-load
ratelimiter-load: ## Run the gated Slack rate-limiter tier-2 load test (B3)
	@$(GO) test -tags=loadtest -run=TestRateLimiterLoad -bench=BenchmarkRateLimiter_Tier2 -benchmem ./core/pkg/messenger/slack/...

.PHONY: go-build
go-build: ## Build all Go binaries into ./bin
	@mkdir -p bin
	@$(GO) build -o bin/ ./...

.PHONY: keep-build
keep-build: ## Build the Keep service binary into ./bin/keep
	@mkdir -p bin
	@$(GO) build -trimpath -o bin/keep ./core/cmd/keep

# Keep service runtime env (see core/internal/keep/config). KEEP_DATABASE_URL
# is required; other values fall back to documented defaults. Passed via
# per-target `export` so user-supplied strings reach the shell as env
# literals, never as Make variable expansions (LESSON M2.6).
.PHONY: keep-run
keep-run: export KEEP_DATABASE_URL := $(KEEP_DATABASE_URL)
keep-run: export KEEP_HTTP_ADDR := $(KEEP_HTTP_ADDR)
keep-run: export KEEP_SHUTDOWN_TIMEOUT := $(KEEP_SHUTDOWN_TIMEOUT)
keep-run: export KEEP_TOKEN_SIGNING_KEY := $(KEEP_TOKEN_SIGNING_KEY)
keep-run: export KEEP_TOKEN_ISSUER := $(KEEP_TOKEN_ISSUER)
keep-run: keep-build ## Run the Keep service locally (requires KEEP_DATABASE_URL + token env)
	@: "$${KEEP_DATABASE_URL:?ERROR: KEEP_DATABASE_URL required (e.g. postgres://user:pass@localhost:5432/db?sslmode=disable)}"
	@: "$${KEEP_TOKEN_SIGNING_KEY:?ERROR: KEEP_TOKEN_SIGNING_KEY required (base64-encoded, >= 32 bytes decoded)}"
	@: "$${KEEP_TOKEN_ISSUER:?ERROR: KEEP_TOKEN_ISSUER required (expected iss claim on verified tokens)}"
	@./bin/keep

# Keep integration-test runtime env. Both token values are required because
# the spawned binary now enforces them at config load; KEEP_INTEGRATION_DB_URL
# must point at a Postgres 16 with pgvector and every migration (001..008)
# already applied (CI runs `make migrate-up` before invoking this target).
.PHONY: keep-integration-test
keep-integration-test: export KEEP_INTEGRATION_DB_URL := $(KEEP_INTEGRATION_DB_URL)
keep-integration-test: ## Run integration tests for the Keep binary (requires KEEP_INTEGRATION_DB_URL)
	@: "$${KEEP_INTEGRATION_DB_URL:?ERROR: KEEP_INTEGRATION_DB_URL required (e.g. postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable)}"
	@$(GO) test -tags=integration -race -v ./core/cmd/keep/...

# ---------------------------------------------------------------------------
# Slack dev workspace bootstrap (M4.3)
#
# `make spawn-dev-bot` provisions a parent Slack app from a YAML manifest,
# resolving the configuration token from the secrets interface (env var
# WATCHKEEPER_SLACK_CONFIG_TOKEN by default) and writing the returned
# credentials to a JSON file the operator ingests into their secret store.
#
# Required env:
#   WATCHKEEPER_SLACK_CONFIG_TOKEN — Slack `xoxe-*` app configuration token
#
# Optional env (with defaults):
#   SPAWN_DEV_BOT_MANIFEST          (default: deploy/slack/dev-bot-manifest.yaml)
#   SPAWN_DEV_BOT_CREDENTIALS_OUT   (default: .omc/secrets/dev-bot-credentials.json)
#
# Operators provision the dev workspace separately (see
# docs/DEVELOPING.md "Provisioning the dev Slack workspace"); this target
# does NOT mediate the admin.apps.approve / OAuth-install steps.
# ---------------------------------------------------------------------------

SPAWN_DEV_BOT_MANIFEST        ?= deploy/slack/dev-bot-manifest.yaml
SPAWN_DEV_BOT_CREDENTIALS_OUT ?= .omc/secrets/dev-bot-credentials.json

.PHONY: spawn-dev-bot-build
spawn-dev-bot-build: ## Build the spawn-dev-bot binary into ./bin/spawn-dev-bot
	@mkdir -p bin
	@$(GO) build -trimpath -o bin/spawn-dev-bot ./core/cmd/spawn-dev-bot

.PHONY: spawn-dev-bot
spawn-dev-bot: export WATCHKEEPER_SLACK_CONFIG_TOKEN := $(WATCHKEEPER_SLACK_CONFIG_TOKEN)
spawn-dev-bot: spawn-dev-bot-build ## Provision the parent dev-workspace Slack app from a manifest (M4.3)
	@: "$${WATCHKEEPER_SLACK_CONFIG_TOKEN:?ERROR: WATCHKEEPER_SLACK_CONFIG_TOKEN required (Slack xoxe-* app configuration token; see docs/DEVELOPING.md)}"
	@mkdir -p $$(dirname "$(SPAWN_DEV_BOT_CREDENTIALS_OUT)")
	@./bin/spawn-dev-bot \
	  --manifest "$(SPAWN_DEV_BOT_MANIFEST)" \
	  --credentials-out "$(SPAWN_DEV_BOT_CREDENTIALS_OUT)"

.PHONY: spawn-dev-bot-dry-run
spawn-dev-bot-dry-run: spawn-dev-bot-build ## Validate the manifest WITHOUT contacting Slack (CI gate)
	@./bin/spawn-dev-bot \
	  --manifest "$(SPAWN_DEV_BOT_MANIFEST)" \
	  --credentials-out "$(SPAWN_DEV_BOT_CREDENTIALS_OUT)" \
	  --dry-run

# ---------------------------------------------------------------------------
# wk — unified Watchkeeper operator CLI (ROADMAP §M10.2).
#
# `make wk CMD="<noun-group> <subcommand> [flags]"` is the composite
# shortcut every operator workflow flows through. CMD is passed verbatim
# to ./bin/wk; the binary is built on demand. This is the SOLE general
# entry point — per-noun-group make targets are not defined because the
# composite shortcut already covers every present and future verb (the
# `tools-local-install` target is preserved separately for M9.5 runbook
# compatibility ONLY).
#
# CMD-with-spaces caveat (iter-1 critic m1): Make word-splits `$(CMD)`.
# Flag values containing literal spaces (e.g. `--reason "with space"`)
# are not preserved through the wrapper. Operators who need a
# space-bearing reason should invoke `./bin/wk` directly OR set the
# field via a single-token value (`--reason graduating-tool`). The
# wrapper is a thin convenience; the binary is the contract.
#
# Required env (per-subcommand; see `make wk CMD=help` for the full
# matrix):
#
#   WATCHKEEPER_KEEP_BASE_URL   Keep base URL (Keep-backed subcommands)
#   WATCHKEEPER_OPERATOR_TOKEN  bearer token (Keep-backed subcommands)
#   WATCHKEEPER_DATA_DIR        deployment data root (`wk tool *`)
#   WATCHKEEPER_DATA            notebook data root (`wk notebook *`)
#
# Audit hygiene: secrets (operator token, GitHub PAT) reach the binary
# via ENV only; never argv (which `ps -ef` would leak).
# ---------------------------------------------------------------------------

WK_TOOL_SOURCE ?= local

.PHONY: wk-build
wk-build: ## Build the unified wk operator CLI into ./bin/wk (M10.2)
	@mkdir -p bin
	@$(GO) build -trimpath -o bin/wk ./core/cmd/wk

.PHONY: wk
wk: wk-build ## Forward CMD to ./bin/wk (e.g. make wk CMD="spawn --manifest <id> --lead <id>")
	@./bin/wk $(CMD)

# Tool-source local-install — preserved make-target name for runbook
# compatibility; routes through `wk tool local-install` under the hood
# now that wk-tool has been subsumed.
.PHONY: tools-local-install
tools-local-install: wk-build ## Install an operator-supplied tool folder into a kind=local source (M9.5; --reason REQUIRED)
	@: "$${FOLDER:?ERROR: FOLDER required (path to the new tool folder containing manifest.json)}"
	@: "$${REASON:?ERROR: REASON required (operator-supplied audit text — M9.5 contract)}"
	@: "$${OPERATOR:?ERROR: OPERATOR required (operator identity)}"
	@: "$${WATCHKEEPER_DATA_DIR:?ERROR: WATCHKEEPER_DATA_DIR required (deployment data root)}"
	@./bin/wk tool local-install \
	  --folder "$(FOLDER)" \
	  --source "$(WK_TOOL_SOURCE)" \
	  --reason "$(REASON)" \
	  --operator "$(OPERATOR)" \
	  --data-dir "$(WATCHKEEPER_DATA_DIR)"

.PHONY: govulncheck
govulncheck: ## Scan Go dependencies for known vulnerabilities
	@$(GOVULNCHECK) ./...

# ---------------------------------------------------------------------------
# TypeScript
# ---------------------------------------------------------------------------

.PHONY: ts-install
ts-install:
	@$(PNPM) install --frozen-lockfile

.PHONY: ts-fmt
ts-fmt: ## Format TypeScript sources with prettier
	@$(PNPM) run -r --if-present fmt

.PHONY: ts-lint
ts-lint: ## Type-check and lint TypeScript sources
	@$(PNPM) run -r --if-present typecheck
	@$(PNPM) run -r --if-present lint

.PHONY: ts-test
ts-test: ## Run TypeScript tests with coverage
	@$(PNPM) run -r --if-present test

.PHONY: ts-build
ts-build: ## Build TypeScript packages
	@$(PNPM) run -r --if-present build

OSV_SCANNER_VERSION ?= v2.2.4

.PHONY: ts-audit
ts-audit: ## Scan TypeScript dependencies for known vulnerabilities (needs osv-scanner on PATH)
	@$(PNPM) audit --audit-level=high
	@if command -v osv-scanner >/dev/null 2>&1; then \
	  osv-scanner scan --lockfile=pnpm-lock.yaml .; \
	else \
	  echo "ERROR: osv-scanner is required for 'make ci'. Install via 'brew install osv-scanner' or 'go install github.com/google/osv-scanner/v2/cmd/osv-scanner@$(OSV_SCANNER_VERSION)'. CI installs it from the official release tarball." >&2; \
	  exit 1; \
	fi

# ---------------------------------------------------------------------------
# Cross-cutting linters
# ---------------------------------------------------------------------------

.PHONY: sql-lint
sql-lint: ## Lint SQL migrations
	@if compgen -G "deploy/migrations/*.sql" >/dev/null; then $(SQLFLUFF) lint deploy/migrations; fi

.PHONY: docker-lint
docker-lint: ## Lint Dockerfiles
	@if compgen -G "deploy/**/Dockerfile*" >/dev/null || compgen -G "Dockerfile*" >/dev/null; then \
	  find . -name 'Dockerfile*' -not -path './node_modules/*' -print0 | xargs -0 -r $(HADOLINT); \
	fi

.PHONY: md-lint
md-lint: ## Lint markdown
	@$(MARKDOWNLINT)

.PHONY: yaml-lint
yaml-lint: ## Lint YAML
	@$(YAMLLINT) .

.PHONY: shell-lint
shell-lint: ## Lint shell scripts
	@if compgen -G "scripts/*.sh" >/dev/null; then $(SHELLCHECK) scripts/*.sh; fi

# ---------------------------------------------------------------------------
# Database migrations (goose; see docs/DEVELOPING.md "Migrations")
#
# Targets require WATCHKEEPER_DB_URL to point at a reachable Postgres 16:
#   export WATCHKEEPER_DB_URL='postgres://user:pass@host:5432/db?sslmode=disable'
# The connection string is read from the environment only — never hard-coded
# in the repo or CI logs (AC8). CI uses a throwaway service-container value.
# ---------------------------------------------------------------------------

define require_db_url
@test -n "$$WATCHKEEPER_DB_URL" || { echo "ERROR: WATCHKEEPER_DB_URL not set (e.g. postgres://user:pass@host:5432/db?sslmode=disable)" >&2; exit 2; }
endef

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations (idempotent: no-op when up to date)
	$(require_db_url)
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$$WATCHKEEPER_DB_URL" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	$(require_db_url)
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$$WATCHKEEPER_DB_URL" down

.PHONY: migrate-status
migrate-status: ## Show applied / pending migrations
	$(require_db_url)
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$$WATCHKEEPER_DB_URL" status

.PHONY: migrate-create
migrate-create: export MIGRATION_NAME := $(NAME)
migrate-create: ## Create a new SQL migration: make migrate-create NAME=<slug>
	@: "$${MIGRATION_NAME:?ERROR: NAME=<slug> required (e.g. NAME=add_users_table)}"
	@printf '%s' "$$MIGRATION_NAME" | grep -Eq '^[a-z0-9_]+$$' || { echo "ERROR: NAME must match ^[a-z0-9_]+\$$" >&2; exit 2; }
	@$(GOOSE) -dir $(MIGRATIONS_DIR) create "$$MIGRATION_NAME" sql

.PHONY: migrate-round-trip
migrate-round-trip: ## Up -> down-to-0 -> up; assert schema dump is identical (AC6)
	$(require_db_url)
	@scripts/migrate-round-trip.sh

.PHONY: migrate-test
migrate-test: migrate-up ## Run schema smoke assertions against the migrated database
	$(require_db_url)
	@scripts/migrate-schema-test.sh

# ---------------------------------------------------------------------------
# Security / commit quality / dependencies
# ---------------------------------------------------------------------------

.PHONY: secrets-scan
secrets-scan: ## Scan the repo for leaked secrets
	@$(GITLEAKS) detect --no-banner --redact --config .gitleaks.toml

.PHONY: deps-scan
deps-scan: govulncheck ts-audit ## Scan dependencies for known vulnerabilities

.PHONY: license-scan
license-scan: ## Check third-party license compliance
	@$(GO) run github.com/google/go-licenses@v1.6.0 check ./... --disallowed_types=forbidden,restricted
	@$(PNPM) dlx license-checker-rseidelsohn@4.3.0 --onlyAllow "$$(tr '\n' ';' < .license-allowlist.txt | sed 's/;$$//')" --excludePrivatePackages

.PHONY: commit-lint
commit-lint: ## Lint the most recent commit message
	@$(COMMITLINT) --from=HEAD~1 --to=HEAD

# ---------------------------------------------------------------------------
# Smoke (M10.4)
#
# `make smoke` reproduces the M7 + M8 + M9 success scenarios against an
# isolated dev environment. The script lives at `scripts/smoke.sh` and
# runs `go build ./...` followed by `go test -race -count=1` against the
# curated package set covering each milestone family. Host-only — no
# docker compose, no Postgres, no real Slack required. See the smoke
# section of `docs/operator-runbook.md` for the contract and the
# matching contract test at `core/internal/deploy/smoke_test.go`.
# ---------------------------------------------------------------------------

.PHONY: smoke
smoke: ## Run the Phase 1 smoke test (M7 + M8 + M9 success paths + wk CLI seam against in-memory fakes)
	@scripts/smoke.sh

# ---------------------------------------------------------------------------
# Compose stack (M10.3)
#
# Phase 1 compose stack — postgres + migrate + keep + prometheus +
# grafana + agent contract stubs. The data plane is fully wired and
# bootable; the agent plane prints loud follow-up M-id messages and
# exits 0 (same pattern as the M10.2 `wk` CLI's exit-3 stubs).
#
# Secrets are env-driven via `.env` at repo root (template at
# `.env.example`). Defaults to docker compose v2 plugin syntax.
# ---------------------------------------------------------------------------

# Docker Compose v2 plugin (`docker compose ...`) is the supported invocation.
# Operators on legacy engines without the v2 plugin can override to the
# hyphenated v1 binary via `make compose-up DOCKER_COMPOSE="docker-compose"`
# but the compose-file syntax assumes v2.10+ semantics (notably the
# `condition: service_completed_successfully` gate used by `keep` and the
# `secrets:` mount shape used for the Postgres password). iter-1 #20.
DOCKER_COMPOSE ?= docker compose

.PHONY: compose-up
compose-up: ## Start the Phase 1 compose stack in the foreground
	@$(DOCKER_COMPOSE) up

.PHONY: compose-up-d
compose-up-d: ## Start the Phase 1 compose stack detached
	@$(DOCKER_COMPOSE) up -d

.PHONY: compose-build
compose-build: ## Build / rebuild all locally-built compose images
	@$(DOCKER_COMPOSE) build

.PHONY: compose-down
compose-down: ## Stop and remove compose containers (keeps named volumes)
	@$(DOCKER_COMPOSE) down

.PHONY: compose-down-volumes
compose-down-volumes: ## Stop and remove compose containers AND named volumes (full reset)
	@$(DOCKER_COMPOSE) down --volumes

.PHONY: compose-logs
compose-logs: ## Tail compose logs (all services)
	@$(DOCKER_COMPOSE) logs -f

.PHONY: compose-ps
compose-ps: ## List compose service status
	@$(DOCKER_COMPOSE) ps
