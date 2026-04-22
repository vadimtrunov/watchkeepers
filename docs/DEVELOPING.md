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
- `docker-ci` — `hadolint`
- `security-ci` — `gitleaks detect`
- `meta-ci` — `markdownlint`, `yamllint`, `commitlint`

`make help` lists every target.

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

The cross-cutting constraint from the roadmap: **any check that runs in CI
must also run in pre-commit** — no local-only or CI-only gates.

## Troubleshooting

- `golangci-lint: go1.26 is higher than the tool's Go version` — upgrade
  `golangci-lint` to v2.11+ (`brew upgrade golangci-lint`).
- `pnpm: command not found` — run `corepack enable && corepack prepare pnpm@10.33.0 --activate`.
- `lefthook: command not found` — `brew install lefthook` then
  re-run `make bootstrap`.
- `gitleaks: secret detected` — either fix the committed secret or, if it is
  a false positive, add a targeted allowlist entry in `.gitleaks.toml` with a
  comment explaining why.
