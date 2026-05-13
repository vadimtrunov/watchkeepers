# Operator runbook

Operator-facing reference for running and extending a Watchkeeper
deployment. The runbook is task-oriented: every section answers a
single concrete operator question.

The first iteration (M9.4.d) covers adoption of the shared Tool CI
workflow template. Future milestones append sections (Keep backups,
secrets rotation, federation peers, etc.) as those surfaces ship.

## Adopting the shared Tool CI workflow template

The Watchkeeper platform publishes one GitHub Actions workflow template
that every tool repo — the platform `watchkeeper-tools` repo AND every
customer-private tool repo — consumes via a single `workflow_call`
reference. The template runs the four gates the M9.4.b in-process AI
reviewer also runs (`typecheck`, `undeclared_fs_net`, `vitest`,
`capability_declaration`) and an optional signing step on merge.

### One-line adoption

In a tool repo (`watchkeeper-tools` or a customer-private repo), drop
the following file at `.github/workflows/ci.yml`:

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read

jobs:
  tool-ci:
    uses: watchkeeper-tools/watchkeeper-tools/.github/workflows/tool-ci.yml@v1.0.0
    with:
      coverage_threshold: 80
      enable_signing: false
```

Replace `@v1.0.0` with the latest tagged release of the
`watchkeeper-tools` repo. The full reusable workflow ships in this
repo under `tools-builtin/ci-template/tool-ci.yml`; the platform repo
mirrors it on each release.

### Inputs

| Input                | Type    | Default     | Description                                                                                  |
| -------------------- | ------- | ----------- | -------------------------------------------------------------------------------------------- |
| `node_version`       | string  | `"24.15.0"` | Node.js version pinned for the run.                                                          |
| `pnpm_version`       | string  | `"10.33.0"` | pnpm version pinned for the run.                                                             |
| `coverage_threshold` | number  | `80`        | Vitest coverage floor (percent), applied to lines / functions / branches / statements alike. |
| `enable_signing`     | boolean | `false`     | If true, run the signing step on merge to `main` or on a `v*` release tag.                   |

### Tool repo layout expectations

The template assumes the consumer repo's working tree contains:

- `manifest.json` at the repo root, matching
  `core/pkg/toolregistry/manifest.go:Manifest` (required:
  `name`, `version`, `capabilities`, `schema`, `dry_run_mode`).
  Unknown top-level keys fail the `capability_declaration` gate
  (mirrors the Go decoder's `DisallowUnknownFields`).
- `package.json` with `tsc` and `vitest` resolvable under the pinned
  pnpm version.
- `pnpm-lock.yaml` at the repo root. The template runs
  `pnpm install --frozen-lockfile` which fails without a lockfile.
- `tsconfig.json` covering the source tree.
- `src/**/*.ts` source files (non-test). Symlinks under `src/` ARE
  followed by both the `undeclared_fs_net` walker and the `sign`
  step's bundle hash; the walker detects cycles via a realpath
  visited-set.
- `src/**/*.test.ts` vitest test files.

### Per-gate semantics + remediation

The gate vocabulary mirrors `core/pkg/approval/reviewer.go:GateName`
verbatim. A failure in any gate fails the CI run; the human reviewer
on the PR sees the same gate name on the approval card the
in-process M9.4.b reviewer renders for `slack-native` mode.

#### `typecheck`

- **What it runs**: `pnpm exec tsc --noEmit` against the consumer's
  `tsconfig.json`.
- **Why it fails**: TypeScript reports at least one error.
- **Remediation**: run `pnpm exec tsc --noEmit` locally, fix every
  reported diagnostic, push.

#### `undeclared_fs_net`

- **What it runs**: an inlined Node script that walks `src/**/*.ts`
  (skipping `*.test.ts`) and matches each file against the same
  needle table the M9.4.b reviewer uses
  (`core/pkg/approval/reviewer.go:fsNetPattern`). When a needle
  matches but the corresponding capability id is absent from
  `manifest.json#capabilities`, the gate fails.
- **Why it fails**: `src/foo.ts` imports `node:fs` without
  `filesystem:read` in `capabilities`, or calls `fetch(` without
  `network:http`.
- **Remediation**: either add the missing capability to
  `manifest.json` (which forces the M9.4.b approval card to surface
  it to the lead — the desired outcome when the source legitimately
  needs the capability) OR remove the import / call from the source
  (when the usage was unintentional).

#### `vitest`

- **What it runs**: `pnpm exec vitest run --coverage` with
  `--coverage.thresholds.{lines,functions,branches,statements}`
  forced to the `coverage_threshold` input (default `80`).
- **Why it fails**: a test fails OR coverage drops below the floor
  on any of the four metrics.
- **Remediation**: run the SAME command the gate runs to avoid the
  local-green / CI-red trap — a bare `pnpm exec vitest run
--coverage` uses the consumer's local `vitest.config.ts`
  thresholds (typically `60` in the `wathkeepers` core repo)
  rather than the gate's `80`. Mirror the gate locally:
  `pnpm exec vitest run --coverage
--coverage.thresholds.lines=80 --coverage.thresholds.functions=80
--coverage.thresholds.branches=80 --coverage.thresholds.statements=80`.

#### `capability_declaration`

- **What it runs**: an inlined Node script that loads `manifest.json`
  and asserts:
  - Decoded value is an object (not null / array / scalar).
  - No unknown top-level keys (mirrors the Go decoder's
    `DisallowUnknownFields`); the allowed set is `{name, version,
capabilities, schema, source, signature, dry_run_mode}`.
  - `name` matches `^[a-z][a-z0-9_]*$` (lower_snake_case). The
    name regex is stricter than the Go decoder's
    `strings.TrimSpace(name) == ""` check — the decoder defers
    convention enforcement to "a linter rule" (manifest.go:Name
    godoc) and this gate IS that rule.
  - `version` is a non-empty string AFTER trimming whitespace
    (mirrors the Go decoder's `strings.TrimSpace` discipline).
  - `capabilities` is a non-empty array of strings, each matching
    `^[a-z][a-z0-9_:.-]*$` (no duplicates).
  - `dry_run_mode` ∈ `{ghost, scoped, none}` (the M9.4.a strict-
    decode contract).
- **Why it fails**: any of the above is violated.
- **Remediation**: fix `manifest.json` per the failure message. The
  gate emits one line per violation so a tool repo with multiple
  issues sees the full list in one run.

#### `sign` (optional)

- **When it runs**: only when `enable_signing: true` AND the workflow
  fires on `refs/heads/main` (post-merge) or `refs/tags/v*` (release
  tag).
- **`github.ref` semantics for reusable workflows**: GitHub Actions
  evaluates `github.ref` in the CALLER's event context. A consumer
  manually invoking the reusable workflow via `workflow_dispatch`
  against a `v*` tag DOES match the `refs/tags/v` predicate and
  triggers signing; an invocation from a `pull_request` event sets
  `github.ref` to `refs/pull/N/merge` and signing skips even if the
  PR targets `main`. Consumers wanting signing on PR merges should
  rely on the post-merge `push` event to `main`, not on the PR
  event itself.
- **What it runs**: a deterministic SHA-256 over (`manifest.json` +
  every non-test `*.ts` file reachable under `src/`, following
  symlinks) is computed and emitted as the `tool_bundle_sha256`
  output + a GitHub Actions notice.
- **Consuming the output**: the reusable workflow declares
  `outputs.tool_bundle_sha256` at the `workflow_call` level so a
  consumer can chain a downstream job:

  ```yaml
  jobs:
    tool-ci:
      uses: watchkeeper-tools/watchkeeper-tools/.github/workflows/tool-ci.yml@v1.0.0
      with:
        enable_signing: true
    publish:
      needs: tool-ci
      runs-on: ubuntu-24.04
      steps:
        - run: echo "bundle SHA-256 was ${{ needs.tool-ci.outputs.tool_bundle_sha256 }}"
  ```

  The output is empty on runs where signing skipped
  (`enable_signing: false` or non-release ref).

- **Real cosign integration**: deferred to M9.5 (signing keys + kms
  wiring). Today's step pins the bundle SHAPE without depending on a
  key; M9.6 (hosted ↔ git migration) tooling can already consume the
  hash as a bundle identity.

### Versioning policy

Semver across two axes; full description in
`tools-builtin/ci-template/README.md`. Customer-private repos SHOULD
pin to a specific tag (`@v1.2.3`) rather than a moving ref
(`@main`) — a platform-side template change should never silently
flow into a customer repo's CI surface.

### Local validation before push

The `typecheck` and `vitest` gates run standard pnpm scripts that a
tool author can reproduce directly:

```sh
pnpm install --frozen-lockfile
pnpm exec tsc --noEmit                        # typecheck gate
pnpm exec vitest run --coverage               # vitest gate
```

The `undeclared_fs_net` and `capability_declaration` gates run
inlined Node scripts inside the workflow. Their logic is fully
visible in `tools-builtin/ci-template/tool-ci.yml` — copy the
heredoc body to a local `.mjs` file and `node` it directly to
reproduce locally. Extracting the inlined linters to standalone
script files (consumable both inline and standalone) is tracked as
an M9.5 follow-up.

### Operator-side observability

- The PR's `Checks` tab shows one row per gate, with the gate name
  matching the M9.4.b reviewer vocabulary.
- The M9.4.b webhook receiver (`core/pkg/approval/webhook.go`)
  observes the merge event and emits `tool_approved` on the
  eventbus; M9.1.b's effective-toolset registry rebuilds on receipt.
- Build artifacts: the `sign` step's `tool_bundle_sha256` output is
  consumable by downstream workflows (release publishers, audit
  trail) via the GitHub Actions outputs surface.
