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

## M9.6 — Hosted ↔ git migration + tool sharing

M9.6 ships two operator-facing CLI surfaces plus one agent-facing
JSON-RPC method. Together they cover the lifecycle "hosted tool
graduates to git" (export → branch → commit → PR → notify lead).

### Credentials

Two credential shapes are supported. The `github.TokenSource`
interface is the boundary; the deployment supplies a concrete
implementation backed by its secrets store.

- **Personal Access Token (PAT)** — the Phase 1 quickstart default.
  Create a fine-grained PAT on the bot account with `Contents: read
& write` and `Pull requests: read & write` on the platform
  `watchkeeper-tools` repo (and the customer-private repo if the
  deployment shares to private). Bake the PAT into the deployment's
  secrets store; the CLI reads it from the env var named by
  `--token-env` (default `WATCHKEEPER_GITHUB_TOKEN`).
- **GitHub App** — production-recommended. Install the App on the
  target repo(s) with the minimal scope set (Contents R/W, Pull
  Requests R/W). A follow-up `github.TokenSource` implementation
  mints an installation token on demand from the App's private key;
  the toolshare orchestrator's per-call seam discipline lets the
  token rotate without a process restart. The M9.6 CLI ships PAT
  only; the orchestrator interface is App-ready.

The `tool:share` capability is **off by default**. The lead grants
it per-Watchkeeper by issuing a capability token via
`capability.Broker.Issue("tool:share", ttl)` and binding the token
to the agent's session. A revoked or expired token fails the
`watchmaster.promote_share_tool` RPC with code `-32005`
(ToolUnauthorized).

### `wk tool hosted export` — operator flow

Exports a tool tree from a `kind: hosted` source on the local
deployment's data directory into a self-contained bundle the
operator imports into a fresh git repo. Replaces the M9.7-era
`wk-tool hosted-export` invocation; M10.2 subsumed the standalone
`wk-tool` binary under the unified `wk` CLI (the flag shape is
preserved verbatim — only the invocation prefix changed).

```bash
WATCHKEEPER_DATA_DIR=/var/lib/watchkeepers \
  wk tool hosted export \
    --source hosted-private \
    --tool weekly_digest \
    --destination /tmp/weekly_digest-bundle \
    --reason "graduating weekly_digest for community use" \
    --operator alice@example.com
```

Requirements:

- The destination directory MUST be absent OR empty. The CLI
  refuses with `ErrDestinationNotEmpty` if the directory exists
  and contains entries — overwriting an arbitrary on-disk tree
  would be an irreversible operator surprise.
- The source MUST be a configured `kind: hosted` source. Local /
  git kinds are refused.
- A self-contained bundle imports cleanly into a fresh repo:
  `cd /tmp/weekly_digest-bundle && git init && git add . && git
commit -m "initial"`. The M9.4.d CI template runs unmodified.

Stdout shape: one JSONL event line on
`hostedexport.hosted_tool_exported` plus a one-line summary
ending with `correlation_id=<value>`.

### `wk tool share` — operator flow

Opens a PR on the target repo from the deployment's on-disk tool
tree. Replaces the M9.4.b-era `wk-tool share` invocation; M10.2
subsumed the standalone `wk-tool` binary under the unified `wk`
CLI.

```bash
WATCHKEEPER_DATA_DIR=/var/lib/watchkeepers \
  WATCHKEEPER_GITHUB_TOKEN=ghp_... \
  wk tool share \
    --source private \
    --tool weekly_digest \
    --target platform \
    --target-owner watchkeepers \
    --target-repo watchkeeper-tools \
    --target-base main \
    --reason "graduating weekly_digest" \
    --proposer alice@example.com
```

Stdout shape: TWO JSONL event lines on
`toolshare.tool_share_proposed` (BEFORE github calls) and
`toolshare.tool_share_pr_opened` (AFTER github calls succeed)
plus a one-line summary with `pr=<number>` and the PR HTML URL.

The CLI does NOT send a Slack DM; that path is exclusive to the
agent-facing `promote_share_tool` flow where the deployment's
`*slack.Client` is already wired.

### `promote_share_tool` — agent flow

The TS-side `promote_share_tool` builtin tool dispatches to the
Go-side `watchmaster.promote_share_tool` RPC handler. Wire shape:

```json
{
  "proposer_id": "agent-coordinator-001",
  "source_name": "private",
  "tool_name": "weekly_digest",
  "target_hint": "platform",
  "reason": "graduating weekly_digest",
  "capability_token": "wkc_..."
}
```

The handler:

1. Validates `capability_token` against scope `tool:share` via
   `capability.Broker.Validate`. An invalid / expired / mismatched
   token fails with `-32005` (ToolUnauthorized).
2. Forwards to `toolshare.Sharer.Share`. The orchestrator emits
   `tool_share_proposed` BEFORE the github calls, then runs the
   github chain, then emits `tool_share_pr_opened` AFTER the PR
   is open.
3. The orchestrator sends a Slack DM to the lead (best-effort —
   a Slack outage logs but does not undo the PR open). The lead's
   user id is resolved via a per-call `LeadResolver` closure
   bound at process-wiring time.

Response shape:

```json
{
  "pr_number": 42,
  "pr_html_url": "https://github.com/watchkeepers/watchkeeper-tools/pull/42",
  "branch_name": "watchkeepers/share/weekly_digest/1.0.0/1",
  "tool_version": "1.0.0",
  "correlation_id": "1700000000000000000-1",
  "lead_notified": true
}
```

### Half-uploaded share branch — manual cleanup

The `toolshare.Sharer.Share` chain is `GetRef → CreateRef → N×CreateOrUpdateFile → CreatePullRequest`. The Contents API commits one
file per HTTP call, so a network outage or GitHub rate-limit
between file `k` and file `k+1` leaves the share branch on the
target repo with the first `k` files committed and no open PR.

Iter-1 fix (reviewer B M3): the orchestrator surfaces the branch
name + completed-count on the error wrap and also via the
optional `Logger`, e.g.:

```text
toolshare: github create file failed: branch="watchkeepers/share/weekly_digest/1.0.0/1700000000000000000-1"
file="src/index.ts" completed=3/12: github: api error: status=429 ...
```

Operator cleanup procedure when this fires:

```bash
gh pr close --delete-branch \
  --repo watchkeepers/watchkeeper-tools \
  watchkeepers/share/weekly_digest/1.0.0/1700000000000000000-1 \
  || gh api -X DELETE \
       /repos/watchkeepers/watchkeeper-tools/git/refs/heads/watchkeepers/share/weekly_digest/1.0.0/1700000000000000000-1
```

The first command runs when a PR was somehow opened against the
branch (rare for this failure mode); the fallback `gh api -X DELETE`
removes the branch directly when no PR exists. A subsequent
`wk tool share` invocation produces a NEW branch name (different
nanosecond timestamp + atomic nonce) so the cleanup does NOT race
the retry.

The `CreatePullRequest` failure path (last step) leaves the
fully-uploaded share branch without a PR. Same `gh api -X DELETE`
cleanup applies; the audit subscriber sees the
`tool_share_proposed` event with no matching `tool_share_pr_opened`
(see `core/pkg/toolshare/events.go` for the orphan-row tolerance
discipline).

### Deferred webhooks

The M9.7 audit-surface list names `tool_share_pr_merged` and
`tool_share_pr_rejected` as topics. M9.6 ships the PR-create side
only; the merge / rejected webhook handlers land in a follow-up.

In the meantime, two operator-friendly paths cover the gap:

- **Cron-scheduled re-sync** (M9.1.a/b default). The lead merges
  the share PR on the target repo. On the next scheduled pull
  cycle, `ToolSyncScheduler` re-clones the source and the M9.1.b
  effective-toolset recompute picks up the new tool version
  automatically.
- **Manual `wk tool sync` invocation** for an operator-triggered
  refresh on an `on-demand` source.

### Audit-event vocabulary

Per `docs/ROADMAP-phase1.md` M9.7's topic list, this milestone
emits three of the topics:

- `hostedexport.hosted_tool_exported` — emitted by
  `hostedexport.Exporter.Export` after the destination tree is
  written.
- `toolshare.tool_share_proposed` — emitted by
  `toolshare.Sharer.Share` BEFORE the github calls begin.
- `toolshare.tool_share_pr_opened` — emitted by
  `toolshare.Sharer.Share` AFTER the PR is open.

`tool_share_pr_merged` and `tool_share_pr_rejected` ship in the
M9.7 audit-surface follow-up.
