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

## M10.4 — Operator runbook + smoke

This section is the operator-facing reference for running, maintaining,
and recovering a Phase 1 Watchkeeper deployment. Sub-sections answer
one concrete operator question each and are deliberately
command-first; rationale appears inline only where the choice would
otherwise surprise a fresh reader.

### Workspace bootstrap (first deployment on a fresh host)

The Phase 1 stack runs out of `docker-compose.yml` at the repo root.
Bootstrap is six commands plus secret provisioning; the Phase 1
Definition of Done bullet 1 pins this contract.

1. **Clone + check prerequisites.** Required tools on `PATH`: `docker`
   (Compose v2 plugin), `make`, `openssl`. Verify with
   `docker compose version && make --version && openssl version`.
   `make tools-check` covers the dev-time toolchain as well.

2. **Seed the env file.** Copy the template:

   ```bash
   cp .env.example .env
   $EDITOR .env
   ```

   Mandatory non-secret values: `KEEP_TOKEN_ISSUER` (stable string;
   matches whatever the issuing service stamps),
   `GF_SECURITY_ADMIN_PASSWORD` (Grafana bootstrap password — iter-1
   #16 made this required; `admin/admin` was the foot-gun),
   `GF_SECURITY_ADMIN_USER` (defaults to `admin` per the Grafana
   image; only set explicitly if a different username is desired).
   Optional knobs are commented out by default; uncomment and edit
   only when a non-default is needed.

3. **Mint the load-bearing secrets.** Load-bearing secrets live in
   `./secrets/<name>` files mounted via docker `secrets:` (NEVER in
   `.env`, since `.env` reaches the build context and `docker inspect`):

   ```bash
   mkdir -p secrets && chmod 700 secrets
   openssl rand -hex 24    > secrets/postgres_password
   openssl rand -base64 48 > secrets/keep_token_signing_key
   chmod 600 secrets/*
   ```

   The Postgres password is hex-encoded so URI-reserved characters
   (`@`, `:`, `/`, `?`, `#`) never appear in the assembled DSN — the
   M10.3 iter-1 #1 fix routes the password through
   `deploy/migrate-entrypoint.sh`'s awk-based URL encoder for the
   migrate sidecar, but a hex-only password sidesteps the encoder
   altogether for the Keep service's own DSN.

4. **Bring up the data plane.** Default `docker compose up` boots
   `postgres → migrate (one-shot) → keep + prometheus + grafana`:

   ```bash
   make compose-up
   ```

   Health gates: `keep.depends_on.postgres = service_healthy` AND
   `keep.depends_on.migrate = service_completed_successfully` so the
   HTTP server never serves a request against a stale schema. First
   boot logs the migration application in `compose logs migrate`
   ending with `migrate: complete (up)` (iter-1 #14 grep marker).

5. **Verify the deployment is online.** Three quick checks:

   ```bash
   # Keep healthcheck.
   curl -fsS http://127.0.0.1:8080/metrics | head -5
   # Prometheus is ready.
   curl -fsS http://127.0.0.1:9090/-/ready
   # Prometheus is scraping the Keep job AND the keep target is up.
   # Iter-1: parse the result so a down target fails the check rather
   # than passing on a 200 status with `value=[ts, "0"]`.
   curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=up%7Bjob%3D%22keep%22%7D' \
     | jq -e '.data.result[0].value[1] == "1"'
   # Grafana login page (admin user from .env).
   curl -fsS -u "${GF_SECURITY_ADMIN_USER:-admin}:$GF_SECURITY_ADMIN_PASSWORD" \
     http://127.0.0.1:3000/api/health
   ```

   All four return HTTP 200 / `true`. The starter dashboard lives at
   <http://127.0.0.1:3000/d/watchkeeper-phase1/>; its `uid` is pinned
   by `TestDashboardUIDStable` so the permalink survives renames.

6. **Provision the dev Slack workspace** (only on hosts that need a
   live Slack integration). The `spawn-dev-bot` flow is the M4.3
   contract — see the "Provisioning the dev Slack workspace" section
   of `docs/DEVELOPING.md` and the
   `make spawn-dev-bot WATCHKEEPER_SLACK_CONFIG_TOKEN=xoxe-...`
   target. CI lanes and `make smoke` do NOT need a real workspace —
   Slack interactions land against fakes in unit tests.

Throughout, agent stubs (`core`, `watchmaster`, `coordinator`,
`slack-bridge`) stay quiet behind `profiles: [stubs]` (iter-1 #3).
Operators probing the future deployment shape run
`docker compose --profile stubs up <name>`; the stub stays in
`Running` state with the banner readable via `compose logs <name>`.

### Credential rotation

Phase 1 holds three credential surfaces. Rotation is always a
"replace the file/env, restart the service" sequence — no in-process
re-key. The Keep service does not cache the signing key across
restarts; the Postgres password is read by the postgres image's
`*_FILE` env-var support; PATs flow into the operator-supplied env.

#### Postgres password (`./secrets/postgres_password`)

The Postgres password reaches three surfaces: the `postgres` container
(via `POSTGRES_PASSWORD_FILE`, mounted secret), the `migrate` sidecar
(same mount, assembled into a DSN at runtime by
`deploy/migrate-entrypoint.sh`), AND the `keep` service's
`KEEP_DATABASE_URL` (a plain env var in `.env` today — iter-1 codex M2
flagged this asymmetry; a future M10.3.b will mount a pre-assembled
DSN file). All three must rotate together.

1. Mint replacement, world-unreadable from the start:

   ```bash
   ( umask 077 && openssl rand -hex 24 > secrets/postgres_password.new )
   ```

2. Update the Postgres role. Iter-1 codex+critic flagged the original
   `psql -c "ALTER ROLE ... PASSWORD '$(cat ...)'"` shape as a double
   leak (shell history captures the command-substitution form; `psql`
   receives the cleartext as argv, visible to `ps -ef` on the host
   until exec returns). Fix: pipe the SQL via stdin so the cleartext
   never reaches argv, and prefix the command with a leading space so
   `HISTCONTROL=ignorespace` drops it from shell history:

   ```bash
    printf "ALTER ROLE postgres PASSWORD '%s';\n" "$(cat secrets/postgres_password.new)" \
     | docker compose exec -T postgres psql -U postgres -d postgres
   ```

   (The leading space on the `printf` is intentional — keep it.)

3. Atomically swap the secret file:

   ```bash
   mv secrets/postgres_password.new secrets/postgres_password
   chmod 600 secrets/postgres_password
   ```

4. Update `KEEP_DATABASE_URL` in `.env` to embed the new password
   (the Keep service does NOT read `secrets/postgres_password`; it
   needs the password baked into its DSN). Edit `.env`, replace the
   password component, then restart keep so the new env value is
   picked up:

   ```bash
   $EDITOR .env                                # rotate KEEP_DATABASE_URL
   docker compose up -d --force-recreate keep  # picks up the new env
   docker compose restart migrate              # picks up secret-file mount
   ```

   `postgres` itself does not need a restart — the in-process
   `ALTER ROLE` in step 2 makes the new password effective for every
   subsequent connection. The secret-file swap in step 3 only matters
   on a future container re-init (e.g. `compose-down-volumes` followed
   by a fresh `compose-up`); the running cluster ignores
   `POSTGRES_PASSWORD_FILE` after initdb.

5. Sanity-check via the keep `/metrics` endpoint (proves the new DSN
   reaches Postgres):
   `curl -fsS http://127.0.0.1:8080/metrics | grep -c watchkeeper_`
   returns a non-zero count.

#### Keep token signing key (`./secrets/keep_token_signing_key`)

The signing key has no in-flight token concept — tokens are JWT-style
and validated independently per request. A rotation has zero downtime
for steady-state traffic; any in-flight token signed with the
previous key fails verification after the restart and the client
retries with a fresh token.

```bash
openssl rand -base64 48 > secrets/keep_token_signing_key.new
mv secrets/keep_token_signing_key.new secrets/keep_token_signing_key
chmod 600 secrets/keep_token_signing_key
docker compose restart keep
```

The `KEEP_TOKEN_SIGNING_KEY_FILE` env var (iter-1 #1/#2) is mounted
at `/run/secrets/keep_token_signing_key`; the M10.1 config code
trims the trailing newline. Decoded value MUST be ≥ 32 bytes
(`config.MinTokenSigningKeyBytes`); `openssl rand -base64 48`
produces 48 decoded bytes — well above the floor.

#### Operator GitHub credentials (`WATCHKEEPER_GITHUB_TOKEN`)

The unified `wk tool share` CLI accepts a PAT in `WATCHKEEPER_GITHUB_TOKEN`
(M9.6). Rotation steps:

1. Mint a new fine-grained PAT on the bot account: `Contents: R/W` +
   `Pull requests: R/W` on the target repo(s).
2. Update the deployment's secrets store with the new value.
3. Operators sourcing the env from a shell file: replace the
   `export WATCHKEEPER_GITHUB_TOKEN=...` line and `source` the file.

The PAT is **never** passed via argv (`ps -ef` would leak it). All
`wk` subcommands that need the token read it via `os.Getenv` on
process start; a fresh invocation after the swap picks up the new
value.

Production deployments SHOULD migrate to a GitHub App
(`github.TokenSource` interface; see "M9.6 — Credentials" above for
the scoped path). App installation tokens mint per-call and rotate
silently — no operator-side rotation drill at all.

#### Slack app configuration token (`WATCHKEEPER_SLACK_CONFIG_TOKEN`)

The `xoxe-*` token consumed by `make spawn-dev-bot` is a Slack
admin-issued configuration token. Slack rotates these via the
workspace admin's "Refresh tokens" UI; operator rotation is one-shot
("regenerate, paste into the secrets store, run `make spawn-dev-bot`
again"). The runbook for the broader dev-workspace provisioning
flow lives in `docs/DEVELOPING.md`.

### Keep backup / restore

Keep is the deployment's source-of-truth for business knowledge plus
manifests, watchkeepers, keepers-log, outbox, and pending approvals.
Backup is a `pg_dump` of the `keep` database; restore is
`drop database keep → CREATE DATABASE keep → goose up → pg_restore`.

#### Routine backup

The dump contains Watchmaster-issued tokens, manifests, and
`capability_records` — treat it as a load-bearing secret. The
snippet below sets a tight `umask` BEFORE the redirect so the dump
file lands at mode 0600, and pre-creates the `backups/` directory
at 0700:

```bash
mkdir -p backups && chmod 700 backups
# Inside the compose stack — the migrate sidecar's image carries
# psql; we lean on the running postgres container instead.
( umask 077 && docker compose exec -T postgres \
    pg_dump -U postgres -d keep --format=custom --no-owner --no-privileges \
    > "backups/keep-$(date -u +%Y-%m-%dT%H-%M-%SZ).dump" )
```

`--format=custom` produces a single-file compressed dump consumable
by `pg_restore --jobs=N` for parallel restore. `--no-owner` +
`--no-privileges` keep the dump portable across deployments (a
restore on a different Postgres user does not fail on missing roles).

Recommended cadence: nightly via cron on the operator host. A
follow-up M-id can wire this into the compose stack as a one-shot
sidecar with retention; today it is operator-driven so the credential
surface stays minimal.

#### Restore drill (the M10.4 DoD bullet 4)

```bash
# 1. Stop the keep service so nothing writes during the restore.
docker compose stop keep

# 2. Drop and recreate the target database.
docker compose exec -T postgres \
  psql -U postgres -d postgres -c "DROP DATABASE IF EXISTS keep;"
docker compose exec -T postgres \
  psql -U postgres -d postgres -c "CREATE DATABASE keep;"

# 3. Reapply migrations against the empty schema. The migrate
#    sidecar is idempotent; running it explicitly here surfaces any
#    schema drift between the dump and the current migration set.
docker compose run --rm migrate

# 4. Restore the dump (read from stdin).
docker compose exec -T postgres \
  pg_restore -U postgres -d keep --no-owner --no-privileges \
  < backups/keep-2026-05-14T03-00-00Z.dump

# 5. Bring keep back online.
docker compose start keep

# 6. Sanity-check the row counts against the most recent
#    keep-tail-keepers-log snapshot.
docker compose exec -T postgres \
  psql -U postgres -d keep -c "SELECT count(*) FROM keepers_log;"
```

The restore preserves all manifests / watchkeepers / pending
approvals / outbox state. After step 5 the outbox publisher resumes
from `published_at IS NULL` so any event that landed in the dump but
had not yet been published reaches subscribers on the next tick
(M10.1 outbox iter-1 C2 — counter committed atomically with the
transaction, so the restore cannot double-publish).

#### Hosted tool source inclusion

The hosted tool source (M9.6) is a Keep-managed surface; the
`pg_dump` above captures it transparently. The Phase 1 risk register
calls this out:

> `hosted`-mode Keep data loss wipes customer tools — Mitigation:
> Hosted tool source is included in the standard Keep backup path;
> `wk tool hosted export` works offline; restore procedure is part
> of the operator runbook DR drill.

The two complementary paths are the dump above (point-in-time DR)
and `wk tool hosted export --source hosted-private --tool <name>`
(deterministic per-tool bundle for tool-level rollback or
ex-deployment migration).

### Notebook archive backup / restore via ArchiveStore

The Notebook substrate is the agent's private memory (one SQLite
file per agent on the harness host's filesystem). Backups land in
`ArchiveStore`, which presents a backend-agnostic `Put / Get / List`
surface; M2b.3 ships `LocalFS` and `S3Compatible` implementations.

#### Storage layout

Both backends emit byte-identical wire format (per the M2b.3.b
contract test): single-entry gzipped tar with one
`<agent_id>.sqlite` member at mode `0o600`. The object key /
filesystem path is `notebook/<agent_id>/<timestamp>.tar.gz`. `List`
returns URIs newest-first via reverse-lex sort. Full schema in
`core/pkg/archivestore/README.md`.

#### Routine archive (operator-driven)

The retire saga (M7.2) archives every Notebook automatically before
tearing the agent down — never deletes. Operators rarely call
archive by hand; the two scenarios that do are (a) the periodic
backup cadence (M2b.5 cron-driven `notebook_backed_up`) and (b)
out-of-cycle DR drill.

Out-of-cycle archive of a live agent. **Today this is an M10.2.b
stub** — `wk notebook archive` returns exit 3 because the
`archivestore.Storer` config is wired in the M10.2.b follow-up. The
eventual shape (do NOT run verbatim yet — operators on Phase 1 today
should drive the daily periodic-backup cron M2b.5 OR run a small Go
program that calls `notebook.DB.Archive` directly):

```bash
# M10.2.b follow-up — exit 3 today.
WATCHKEEPER_DATA=/var/lib/watchkeepers \
WATCHKEEPER_KEEP_BASE_URL=http://127.0.0.1:8080 \
WATCHKEEPER_OPERATOR_TOKEN=$(cat secrets/operator_token) \
  wk notebook archive <watchkeeper-id>
```

For Phase 1 today, the supported paths are:

- The retire saga (M7.2) archives every Notebook automatically
  before tearing the agent down. No operator action required.
- The cron-driven periodic backup (M2b.5) emits
  `notebook_backed_up` against the configured `ArchiveStore`.
- Operators with shell access on the harness host can drive
  `notebook.DB.Archive(ctx, w)` directly into the configured
  store via a small Go program.

#### List archives

```bash
# M10.2.b follow-up — exit 3 today.
wk notebook list-archives <watchkeeper-id>
```

Same M10.2.b stub status. The underlying
`archivestore.ArchiveStore.List(ctx, agentID)` is shipped and
exercised by the contract suite; only the CLI wiring is pending.
Until M10.2.b lands, operators inspect the configured `LocalFS`
root directly (`ls -la $WK_ARCHIVE_ROOT/notebook/<agent_id>/`) or
the S3 bucket via `aws s3 ls s3://<bucket>/notebook/<agent_id>/`.

#### Restore drill (the M10.4 DoD bullet 4 — Notebook side)

```bash
# 1. Pick the snapshot to restore. The store sorts newest-first.
#    Today: list the LocalFS root or S3 prefix directly (see above);
#    M10.2.b will wire `wk notebook list-archives`.
ls -1t /var/lib/watchkeepers/archives/notebook/<watchkeeper-id>/

# 2. Stage the chosen archive at an operator-accessible path. For
#    LocalFS the archive is already a local file; for S3 fetch it
#    first (`aws s3 cp s3://<bucket>/.../<ts>.tar.gz /tmp/...`).

# 3. Import into a fresh agent file. `wk notebook import` IS shipped
#    today (M9.7 era). It takes an absolute filesystem PATH via the
#    `--archive` flag (NOT a `file://` URI; the CLI does `os.Open`
#    on the value). Flags MUST precede the <wk-id> positional —
#    Go's `flag` parser stops at the first non-flag arg.
WATCHKEEPER_DATA=/var/lib/watchkeepers \
  wk notebook import \
    --archive /var/lib/watchkeepers/archives/notebook/<wk>/2026-05-14T03-00-00Z.tar.gz \
    <watchkeeper-id>
```

The import opens the gzipped-tar wrapper transparently, validates
the single-entry contract (`<agent_id>.sqlite`, mode 0600), and
calls `notebook.DB.Import` on the destination file. The bytes that
end up on disk are byte-identical to the archive source — pinned by
the M2b.3 round-trip contract test
(`TestS3Compatible_NotebookRoundTrip`).

#### S3-compatible alternative

Operators with a remote object store swap `LocalFS` for
`S3Compatible` at process-wiring time. The Keep service's
configuration accepts an `archivestore.S3Config` struct
(Endpoint / AccessKey / SecretKey / Bucket / Region / Secure);
operator-side env: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` /
`WK_ARCHIVE_S3_*`. Production deployments use a customer-owned
bucket — no proprietary dependency in the Watchkeeper stack itself.

`Get` rejects cross-bucket URIs (`s3://other-bucket/...`) and
path-traversal attempts (`s3://bucket/notebook/../etc/passwd`)
pre-network with `ErrInvalidURI`. See
`core/pkg/archivestore/README.md` for the full URI shape and
supported S3-compatible providers (AWS S3 / Cloudflare R2 / Wasabi /
MinIO / SeaweedFS / Garage).

### Runaway-agent incident response

A "runaway" agent is one whose token spend / tool-invocation /
external-API rate has crossed a threshold the operator considers
unsafe. The Phase 1 Watchmaster auto-iteration features (Phase 3
M10) are not yet shipped; in Phase 1, the operator is the
escalation path. Two surfaces detect the runaway:

- **Prometheus alerting on the M10.1 metric set**. The starter
  dashboard panels for `watchkeeper_llm_tokens_total`,
  `watchkeeper_tool_invocations_total`,
  `watchkeeper_messenger_rate_limit_remaining` are the operator's
  human-readable canary. Production deployments add a Prometheus
  alert rule that pages when a sum-by-agent threshold trips.
- **Slack `/complaint` slash command** (Phase 3 M10.1 land — not
  Phase 1) and explicit lead-side complaints.

Once the agent is identified, the incident drill is three
commands:

1. **Pause the runtime via retire-with-archive.** Phase 1 has no
   "pause" verb — `retire` IS the immediate stop, because the M7.2
   retire saga always archives before tearing the harness down (no
   data loss). `--archive-uri` AND `--reason` are BOTH REQUIRED
   (M10.2 iter-1 C1 closed the no-archive escape hatch; iter-1 C2
   added `--reason`). The URI MUST be the already-archived snapshot
   for the target agent — operators typically pre-archive via the
   periodic backup OR via a direct `notebook.DB.Archive` call
   (`wk notebook archive` is the M10.2.b follow-up). Flags MUST
   precede the `<wk-id>` positional — Go's `flag` parser stops at
   the first non-flag arg, so `wk retire <wk-id> --archive-uri ...`
   surfaces a usage error.

   ```bash
   WATCHKEEPER_KEEP_BASE_URL=http://127.0.0.1:8080 \
   WATCHKEEPER_OPERATOR_TOKEN=$(cat secrets/operator_token) \
     wk retire \
       --archive-uri file:///var/lib/watchkeepers/archives/notebook/<wk>/<ts>.tar.gz \
       --reason "runaway: token-spend spike at 2026-05-14T03:42Z; see PR #..." \
       <watchkeeper-id>
   ```

   `--reason` lands verbatim in the keepers-log entry the retire
   saga emits, giving forensic review the operator's framing
   without a follow-up `keepers_log` UPDATE.

2. **Confirm the harness teardown.** The retire saga emits
   `manifest_retired` + `notebook_archived` events; tail them to
   confirm the runtime is gone:

   ```bash
   wk tail-keepers-log | grep -i "<watchkeeper-id>"
   ```

   The CLI uses SSE; the bufio.Writer wrap (M10.2 iter-1 M5)
   absorbs transient short writes so a terminal Ctrl-S does not
   silently truncate the stream.

3. **Forensic export of the agent's notebook** (so the post-mortem
   sees the agent's private memory at the moment of incident).
   Flags before the positional; the destination value is passed via
   `--destination` (absolute path; the file MUST NOT exist —
   `O_EXCL` refusal, M10.2 iter-1 M7). The resulting file is the
   archive tarball, not a raw SQLite file:

   ```bash
   wk notebook export \
     --destination /tmp/runaway-<wk>-postmortem.tar.gz \
     <watchkeeper-id>
   ```

   A Sync failure removes the partial file automatically before
   exit so a retry never trips on stale state.

If the runtime fails to tear down (the harness is unreachable, or
the per-call resolver for the WK's SpawnClaim cannot resolve), the
fallback is `docker compose restart core` to drop every agent the
harness supervisor is running. Phase 1 routinely runs one agent per
deployment so the blast radius is bounded.

### Upgrade procedure

A Watchkeeper deployment upgrade is two artefacts moving together:
container images (Postgres, Keep, Prometheus, Grafana, the agent
plane once M10.3.b lands) and the migration set (`deploy/migrations/`).
The compose stack's migrate sidecar runs goose `up` on every
`compose up` invocation; the rollback story is goose `down-to-<v>`
plus a previous-image pin.

#### Pre-upgrade checks

1. **Snapshot the running deployment** so a roll-back is possible.

   ```bash
   make compose-ps                            # what's running today
   docker compose exec -T postgres pg_dump -U postgres -d keep \
     --format=custom --no-owner --no-privileges \
     > backups/pre-upgrade-$(date -u +%Y-%m-%dT%H-%M-%SZ).dump
   ```

2. **Read the changelog** (`docs/lessons/M*.md` for the shipped
   M-ids in the upgrade range) and the migration diff
   (`git log --oneline -- deploy/migrations/`). Migrations are
   forward-only by convention; goose `down` exists for emergencies,
   not for routine downgrades.

3. **Run the smoke gate locally** against the new commit to catch
   compile breaks and missing fixtures BEFORE rolling out:

   ```bash
   git checkout <new-tag>
   make smoke
   ```

#### Apply the upgrade

```bash
git pull --ff-only origin main           # or checkout the release tag
make compose-build                       # rebuild local images
docker compose pull                      # refresh upstream pins
make compose-up-d                        # start detached
```

The migrate sidecar runs on every `up`; goose is idempotent so a
no-op upgrade prints `no migrations to run`. `keep` waits on
`service_completed_successfully` so the HTTP server never serves a
request against a stale schema (M10.3 pattern #4).

Verify post-upgrade:

```bash
docker compose logs migrate --tail 20 | grep "migrate: complete"
docker compose logs keep    --tail 20 | grep -E "ready|listening|started"
curl -fsS http://127.0.0.1:8080/metrics | head -3
```

#### Roll back

1. Stop the keep service: `docker compose stop keep`.
2. If the upgrade ran a migration that introduced a column / table
   that the previous binary cannot tolerate, roll the schema back
   to the target version. The compose `migrate` sidecar's image
   embeds `deploy/migrate-entrypoint.sh` which assembles the DSN
   from `POSTGRES_*` `_FILE` env vars at runtime (iter-1 M10.3
   #1 — the awk-based URL encoder); the entrypoint does NOT
   expose a `$DATABASE_URL` for the `sh -c` payload to consume.
   Two paths:

   Option A — exec a host-side goose against the same Postgres
   the compose stack runs (re-uses the goose version pin in the
   Makefile):

   ```bash
   WATCHKEEPER_DB_URL="postgres://postgres:$(cat secrets/postgres_password)@127.0.0.1:5432/keep?sslmode=disable" \
     make migrate-down                       # rolls back ONE step
   # Repeat make migrate-down per step down, or use the goose
   # `down-to <version>` invocation directly:
   WATCHKEEPER_DB_URL="postgres://postgres:$(cat secrets/postgres_password)@127.0.0.1:5432/keep?sslmode=disable" \
     go run github.com/pressly/goose/v3/cmd/goose@v3.27.0 \
       -dir deploy/migrations postgres "$WATCHKEEPER_DB_URL" down-to <target-version>
   ```

   Option B — exec inside the migrate container so the entrypoint
   assembles the DSN, then invoke goose against the resolved
   value. The entrypoint exits 0 after running goose `up` on
   default; override the command:

   ```bash
   docker compose run --rm --entrypoint sh migrate -c '
     export POSTGRES_PASSWORD=$(cat /run/secrets/postgres_password)
     DSN="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@${POSTGRES_HOST}:${POSTGRES_PORT}/${POSTGRES_DB}?sslmode=${POSTGRES_SSLMODE}"
     exec goose -dir /migrations postgres "$DSN" down-to <target-version>
   '
   ```

   Goose tracks applied versions in `goose_db_version`; `down-to`
   names the highest version to keep.

3. Pin the previous image tag in `.env` (or via per-service
   `image:` override in `docker-compose.override.yml`) and restart:

   ```bash
   docker compose pull keep                 # pulls the pinned tag
   docker compose up -d keep
   ```

4. If the migration is non-reversible (downward-incompatible
   schema change), restore from the pre-upgrade dump (see the
   Keep restore drill above).

### Disaster scenarios

The risk register at `docs/ROADMAP-phase1.md` §5 names ten Phase 1
risks; this section is the operational drill for the four with
"High" or "Critical" impact.

#### D1 — `hosted`-mode Keep data loss wipes customer tools

Symptom: agents that depended on a hosted tool now exit-3 on
dispatch with `tool not found`. `wk tool hosted list` and
`wk tool list` are both M10.2 follow-up stubs today (exit 3 with
the follow-up M-id in the diagnostic), so detection in Phase 1 is
agent-side or via direct Postgres probe.

Detection (Phase 1 today):

```bash
# Agent-side: a previously-loaded hosted tool exits 3 with
# `tool not found` on dispatch. Surfaces in keepers-log as
# `tool_invocation_failed` with the missing tool name.
wk tail-keepers-log | grep tool_invocation_failed

# Direct Postgres probe — count hosted tool source rows.
docker compose exec -T postgres \
  psql -U postgres -d keep -tAc \
    "SELECT count(*) FROM tools_hosted;"
```

Drill:

1. Restore Keep from the most recent nightly dump (see the Keep
   restore drill above). The hosted tool source is a Keep-managed
   surface so the dump captures it transparently.

2. For tools whose hosted version has drifted from the bundle on
   disk, re-import via the M9.6 export path. `wk tool hosted
export` IS shipped and requires `--source --tool --destination
--reason --operator` plus `--data-dir` (or `$WATCHKEEPER_DATA_DIR`).
   Flags MUST precede any positional (there are none in this
   subcommand):

   ```bash
   WATCHKEEPER_DATA_DIR=/var/lib/watchkeepers \
     wk tool hosted export \
       --source <hosted-source-name> \
       --tool <tool-name> \
       --destination /tmp/<tool>-bundle \
       --reason "D1 hosted-mode Keep data loss — restoring tool from bundle" \
       --operator alice@example.com
   # The bundle is a self-contained git repo; reinstall through
   # the normal toolregistry sync path (M9.1.a/b).
   ```

3. Verify via the Postgres probe above (count returns to the
   pre-incident value) AND by triggering the affected agent's
   tool invocation; the keepers-log shows `tool_invocation_ok`
   instead of `tool_invocation_failed`.

#### D2 — Notebook SQLite file corruption or disk loss

Symptom: `notebook.Stats` (run inside the harness) reports
`PRAGMA integrity_check` non-OK, OR the SQLite file is missing
from `$DATA_DIR/notebooks/<wk>.sqlite`.

Drill:

1. Retire the affected agent so the runtime stops trying to write
   to the corrupt file. `--archive-uri` AND `--reason` are both
   required; flags MUST precede the `<wk-id>` positional. The
   retire saga's archive step is best-effort against a corrupt
   source; a checksum mismatch surfaces as a `notebook_archived`
   event with the URI but a non-canonical tarball — note this in
   the post-mortem:

   ```bash
   wk retire \
     --archive-uri file:///var/lib/watchkeepers/archives/notebook/<wk>/<ts>.tar.gz \
     --reason "D2 notebook corruption — PRAGMA integrity_check non-OK at $(date -u +%FT%TZ)" \
     <watchkeeper-id>
   ```

2. Pick a known-good snapshot from `ArchiveStore`. `wk notebook
list-archives` is an M10.2.b stub today; inspect the LocalFS
   root or S3 prefix directly (see "List archives" section
   above).

3. Restore via `wk notebook import --archive <abs-path> <wk-id>`
   (see the Notebook restore drill above for the exact shape;
   `--archive` takes an absolute filesystem path, not a URI).

4. If no archive predates the corruption, the agent's private
   memory is lost; the agent itself is recoverable by spawning a
   replacement from the same manifest (the manifest lives in
   Keep, not Notebook). Post-mortem flags the gap so a future
   M2b.5 cadence change tightens the periodic-backup interval.

#### D3 — PAT / GitHub App credential compromise

Symptom: a forensic review of `keepers_log`'s
`tool_share_pr_opened` events shows a PR opened from the bot
account at a time / against a repo that does not match an
authorised operator session.

Drill:

1. Revoke the PAT from the bot account's GitHub settings page.
   For a GitHub App: rotate the app's private key in the GitHub
   Apps settings; existing installation tokens minted from the
   old key expire within one hour.
2. Mint a replacement (see "Credential rotation — Operator GitHub
   credentials" above).
3. Audit `keepers_log` for the compromise window. `wk logs` filters
   client-side by `actor_watchkeeper_id` and therefore only sees
   **agent-initiated** share events; operator-initiated `wk tool
share` invocations have the OPERATOR identity, not a wk-id, and
   are NOT captured by the wk-id filter. Use `wk tail-keepers-log`
   (no filter) or a direct Postgres query against `keepers_log` to
   see both:

   ```bash
   # Agent-initiated shares (per-wk-id filter):
   wk logs --limit 1000 <watchkeeper-id> | \
     grep -E "tool_share_pr_opened|tool_share_proposed"

   # Both agent- AND operator-initiated shares (direct probe):
   docker compose exec -T postgres \
     psql -U postgres -d keep -c \
       "SELECT happened_at, actor_human_id, actor_watchkeeper_id, event_kind, payload
        FROM keepers_log
        WHERE event_kind IN ('tool_share_pr_opened', 'tool_share_proposed')
          AND happened_at >= NOW() - INTERVAL '7 days'
        ORDER BY happened_at DESC;"
   ```

   Cross-reference each entry with the operator-side `--reason`
   text (M9.6 enforces a non-empty reason; a missing or generic
   reason on a share event is a flag).

4. Close every PR opened during the window via the github GUI or
   `gh pr close --delete-branch`; the upstream `watchkeeper-tools`
   repo's CI does not auto-merge so PRs sit in a reviewable state
   until the human lead approves.

#### D4 — Workstation loss with un-rotated long-lived secrets

Symptom: the operator workstation hosting the deployment's
`./secrets/` tree is lost or stolen.

Drill:

1. From a fresh host, rotate every credential surface in
   parallel (the "Credential rotation" sections above are
   independent — Postgres password rotation does not block Keep
   signing key rotation):
   - Postgres password (re-mint, ALTER ROLE, restart).
   - Keep token signing key (re-mint, restart keep).
   - Operator GitHub PAT (re-mint, update env).
2. Restore the Keep dump from off-site backup (assumes the
   nightly `pg_dump` ships its dumps off-host; the Keep restore
   drill above is the procedure).
3. Restore the agent notebooks from `ArchiveStore` (assumes the
   archive store is off-host — `S3Compatible` against a customer-
   owned bucket is the production-recommended path; `LocalFS`
   on the same workstation as Keep is NOT a recovery surface
   from this scenario).
4. Audit `keepers_log` from the new deployment for any
   unauthorised actions during the gap window.

### Smoke test (`make smoke`)

The smoke target reproduces the M7 + M8 + M9 success scenarios
against an isolated dev environment. It is **host-only** — no
docker compose, no Postgres, no real Slack required. Phase 1
Definition of Done bullet 7 pins this contract:
"`make smoke` passes in CI".

#### Running it

```bash
make smoke
```

The target invokes `scripts/smoke.sh`, which sequences:

1. **Build gate**: `go build ./...` — every binary compiles.
2. **M7 spawn + retire saga**: `go test -race -count=1` against
   `core/pkg/spawn/...`, `core/pkg/lifecycle/...`,
   `core/pkg/notebook/...`, `core/pkg/archivestore/...`.
3. **M8 coordinator + jira + cron daily briefing**: against
   `core/pkg/coordinator/...`, `core/pkg/jira/...`,
   `core/pkg/cron/...`.
4. **M9 tool authoring + approval + dry-run + share**: against
   `core/pkg/approval/...`, `core/pkg/toolregistry/...`,
   `core/pkg/toolshare/...`, `core/pkg/hostedexport/...`,
   `core/pkg/capdict/...`, `core/pkg/localpatch/...`.
5. **Operator CLI seam**: against `core/cmd/wk/...`.

Per-phase `go test -timeout=...` value defaults to 120s; override
via `WK_SMOKE_TIMEOUT=60s make smoke`. The value MUST be a Go
duration (e.g. `120s`, `2m`, `1h`); a bare integer such as
`WK_SMOKE_TIMEOUT=120` exits 2 with a script-level diagnostic
(iter-1). The exit code is 0 on full pass and non-zero on the
first failing build / test step.

#### Why not docker compose?

The M10.3 compose stack covers the **deployment** DoD (`docker
compose up brings Phase 1 online with no manual steps beyond
secret provisioning`). The smoke target covers the **regression**
DoD (every M7/M8/M9 success path stays green on a clean
checkout). Splitting the two surfaces keeps `make smoke` fast
enough to run on every PR (≈ 60 seconds) AND keeps the docker
dependency out of the lightweight Go CI lane.

#### Drift protection

The script's curated package set is duplicated in the Go contract
test at `core/internal/deploy/smoke_test.go`. A rename of any
M7/M8/M9 package without a matching edit to BOTH the script and
the contract test fails the contract test in the same PR
(`TestSmokeScriptIncludesM7M8M9Packages`). The two-place
duplication is intentional: the failure mode is self-describing
and the operator runbook surface (`scripts/smoke.sh`) is the
canonical source.

#### CI gate

The `go-ci` job in `.github/workflows/ci.yml` runs `make smoke`
as a final step after the full `go test -race ./...` and
coverage check. The cost is a few seconds of cached test
re-runs; the value is that the operator-facing entry point
stays green even when individual package tests change.
