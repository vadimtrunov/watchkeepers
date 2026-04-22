SHELL := /usr/bin/env bash
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
ci: lint test secrets-scan deps-scan license-scan ## Run the full CI matrix locally (mirrors GitHub Actions)

.PHONY: lint
lint: go-lint ts-lint sql-lint docker-lint md-lint yaml-lint shell-lint ## Run every linter

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

.PHONY: go-build
go-build: ## Build all Go binaries into ./bin
	@mkdir -p bin
	@$(GO) build -o bin/ ./...

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
	  echo "osv-scanner not installed — install via 'brew install osv-scanner' or 'go install github.com/google/osv-scanner/v2/cmd/osv-scanner@$(OSV_SCANNER_VERSION)' to run this scan locally; CI installs it via the google/osv-scanner action"; \
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
# Smoke (wired in later milestones)
# ---------------------------------------------------------------------------

.PHONY: smoke
smoke: ## Run end-to-end smoke test (scaffolded; full wiring in M10)
	@echo "smoke: placeholder — see docs/ROADMAP-phase1.md §4 M10"
	@exit 0
