# Watchkeeper Phase 1 — First Party Implementation Roadmap

**Status**: Planning
**Created**: 2026-04-21
**Scope reference**: [watchkeeper-business-concept.md](./watchkeeper-business-concept.md) (Phase 1), [watchkeeper-spawn-flow.md](./watchkeeper-spawn-flow.md)
**Next phase**: [ROADMAP-phase2.md](./ROADMAP-phase2.md)
**Deliverable type**: implementation roadmap (milestones, interfaces, verification) — not executable code.

**Progress symbols**: `⬜` not started · `🟨` in progress · `✅` done · `🚫` blocked. Flip the symbol in-place on milestone headers and tick `[x]` on individual scope / verification items as they land.

---

## 1. Executive Summary

Build the minimal viable Party: a **Watchmaster** meta-agent that can spawn a **Coordinator** Watchkeeper into a Slack workspace on human command, backed by a shared knowledge store (**The Keep**) and an audit-grade event log (**Keeper's Log**). The Coordinator operates as a real Slack/Jira participant, executes Watch Orders from its human lead, and can extend its own toolset through a supervised self-modification loop.

**Success is binary.** A human writes the Watchmaster in Slack, approves a Manifest, and within minutes a new bot appears in the workspace as a team member, receives tasks, and acts on them. A Watchkeeper can also draft a new tool; on human approval, the tool hot-loads and is immediately usable.

---

## 1.1 Status Dashboard

| #   | Milestone                         | Status | Magnitude | Notes                        |
| --- | --------------------------------- | ------ | --------- | ---------------------------- |
| M1  | Foundation                        | ✅     | 3–5d      |                              |
| M2  | Keep service                      | ⬜     | 4–6d      |                              |
| M2b | Notebook library                  | ⬜     | 3–5d      |                              |
| M3  | Go core services                  | ⬜     | 5–8d      |                              |
| M4  | Messenger adapter + Slack         | ⬜     | 5–7d      | requires dev Slack workspace |
| M5  | Runtime adapter + Claude Code     | ⬜     | 7–10d     | requires Claude Code on host |
| M6  | Watchmaster                       | ⬜     | 4–6d      |                              |
| M7  | Spawn Flow end-to-end             | ⬜     | 4–6d      |                              |
| M8  | Coordinator + Jira adapter        | ⬜     | 5–7d      | requires Jira test project   |
| M9  | Tool Registry + self-modification | ⬜     | 14–20d    | requires platform tool repo  |
| M10 | Observability, CLI, runbook       | ⬜     | 4–6d      |                              |

---

## 2. Key Architectural Decisions

| Concern               | Decision                                                                                                                                                                  | Rationale                                                                                                                                                                                                                      |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Compiled core         | Go                                                                                                                                                                        | Mature Slack/Jira SDKs, fast iteration, simple ops.                                                                                                                                                                            |
| Interpreted layer     | TypeScript                                                                                                                                                                | Strict tool isolation (`isolated-vm`, workers, Deno-style perms); runtime validation via zod; Claude Code / Agent SDK first-class.                                                                                             |
| Agent harness         | Claude Code (first), behind `AgentRuntime` interface                                                                                                                      | Use the same harness humans use; multi-harness future (OpenAI Codex, custom) with no vendor lock-in.                                                                                                                           |
| LLM execution         | `LLMProvider` abstraction, Claude Code subprocess/SDK as default implementation                                                                                           | Operator runs the same Claude Code they already have; swap to another provider by replacing the wrapper, not touching core.                                                                                                    |
| Messenger             | Slack (first), behind `MessengerAdapter` interface                                                                                                                        | Multi-messenger future (Teams, Telegram, Discord).                                                                                                                                                                             |
| Keep storage          | Postgres + pgvector, served by a standalone `keep` service over HTTP/gRPC                                                                                                 | Keep is the **business-knowledge** service of the organization the bots serve — people, processes, decisions, documents, incidents. All bots connect to Keep over the network. Infrastructure metadata never lands in Keep.    |
| Keep scope discipline | Keep holds business knowledge only; multi-environment (prod/staging) isolation is handled by running **separate Keep instances**, not by schema columns                   | Protects the domain model from accumulating infra concerns; `deployment_id`-style fields explicitly rejected.                                                                                                                  |
| Notebook storage      | Per-agent embedded SQLite + `sqlite-vec`, co-located with the harness process                                                                                             | Watchkeepers may run on arbitrary hosts; personal memory must be local, latency-free, and available when Keep is not. No network hop for per-turn `recall`.                                                                    |
| Archive storage       | `ArchiveStore` interface with `LocalFS` default + `S3Compatible` alternative (any S3 API endpoint — AWS S3, Cloudflare R2, Wasabi, SeaweedFS, Garage)                     | Notebook tarballs on retire, periodic backups. No proprietary dependency; operator picks endpoint per deployment. No MinIO (license).                                                                                          |
| Event delivery        | Outbox pattern                                                                                                                                                            | ACID writes + at-least-once async publish; no dual-write problems.                                                                                                                                                             |
| Tool execution        | Capability-based sandbox (isolated-vm + worker processes + capability broker)                                                                                             | LLM-authored tools treated as untrusted by default; capabilities declared, issued, enforced.                                                                                                                                   |
| Tool Registry         | Multi-source overlay (built-in → platform → private → local), with two private-source modes: `git` or `hosted`                                                            | Customer private tools live in a layer platform updates cannot touch; non-engineer customers without git accounts served by `hosted` mode (code stored in the Keep); same-name conflicts resolved by priority with loud audit. |
| Self-modification     | `git` mode: PR-gated. `hosted` mode: Slack-native approval card driven by Watchmaster-as-AI-reviewer. Both run the same lint + typecheck + test + capability-drift gates. | Engineer-led customers get real code review; non-engineer leads approve in plain language via Slack, backed by AI reviewer output and mandatory dry-run where possible.                                                        |
| Approval UX           | Configured per deployment: `git-pr`, `slack-native`, or `both`                                                                                                            | Adapt to the customer's operator profile without forking the authoring flow.                                                                                                                                                   |
| Operator surface      | `make <target>` is the only supported entry point for operational and dev commands                                                                                        | Single, discoverable, CI-verifiable interface; no tribal-knowledge shell commands.                                                                                                                                             |
| Quality gates         | Aggressive linter + test + security matrix in CI and pre-commit                                                                                                           | LLM-authored code and human-authored code meet the same bar.                                                                                                                                                                   |
| Deployment            | docker-compose (Phase 1)                                                                                                                                                  | Local dev parity, simple operator model, clear migration path later.                                                                                                                                                           |
| Tenancy               | Single organization per instance                                                                                                                                          | Phase 1 constraint; multi-tenant deferred to Phase 3.                                                                                                                                                                          |

---

## 3. Scope

### In

- Go compiled core: agent runtime host, event bus, lifecycle manager, cron scheduler, capability broker, Tool Sync Scheduler. Owns Keep client and Notebook archive orchestration.
- **Keep service** — standalone Go binary serving the Keep API (Postgres + pgvector, RLS, outbox, Keeper's Log as the single audit surface). Business knowledge only.
- **Notebook library** — embedded SQLite + `sqlite-vec`, linked into each harness; per-agent file at `$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite`. `remember` / `recall` / `forget` / archive / import API as an in-process library.
- **ArchiveStore** — Go interface with `LocalFS` default and `S3Compatible` alternative; used for Notebook archives and backups.
- `MessengerAdapter` interface + Slack implementation (Slack Manifest API, OAuth, bot profile, Socket Mode events).
- `AgentRuntime` interface + Claude Code implementation (TS harness worker, JSON-RPC over stdio, capability-gated tool invocation).
- `LLMProvider` abstraction with Claude Code subprocess/SDK as the default implementation.
- Watchmaster meta-agent with spawn/retire/list/monitor tools, cost tracking, `adjust_personality` / `adjust_language` tools, `promote_to_keep` approval flow for Notebook entries.
- Coordinator Watchkeeper with Watch Orders, Jira adapter, daily briefing cron, pending-lesson digest.
- Spawn Flow end-to-end per `watchkeeper-spawn-flow.md`, provisioning a per-agent Notebook file on spawn and archiving it to `ArchiveStore` on retire.
- Manifest schema extensions: `personality` (free-text) and `language` (ISO code) fields.
- Auto-injection of top-K relevant Notebook entries into each prompt turn.
- Auto-reflection on tool error → lesson entry in Notebook with `evidence_log_ref` and `tool_version`; lessons have a 24h `active_after` cooling-off window before auto-injection.
- Multi-source Tool Registry (built-in, platform, private, local) with overlay resolution and hot-reload. Private source lives on local filesystem (`$DATA_DIR/tools/private/`), not in Keep.
- `promote_share_tool` flow: private tool graduates to platform or customer git repo via baked GitHub credentials (PAT default, GitHub App recommended for production).
- Self-modification loop with two approval paths: `git-pr` (engineer customer) or `slack-native` (non-engineer). Both gated by lint + typecheck + test + capability-drift. Slack-native path driven by Watchmaster-as-AI-reviewer.
- Dry-run infrastructure for tools (`ghost`, `scoped`, `none` modes) so non-engineer leads can see what a proposed tool will do before approving.
- Human-readable capability dictionary so non-engineers can understand capability declarations in plain language.
- Operator surface: Makefile-first entry point, `wk` CLI, Prometheus metrics, structured logs, docker-compose deployment, operator runbook.
- Quality toolchain: comprehensive linters, test stacks with coverage thresholds, secret scanning, dependency vulnerability scanning, pre-commit enforcement.

### Out (Phase 2+)

- Keeper-to-Keeper communication (only one concrete role in Phase 1 besides Watchmaster).
- Admin dashboard UI.
- Additional roles: Code Reviewer, Tech Writer, PM Assistant, Incident Responder, Onboarding Guide, Security Sentinel.
- Multi-model routing (architecture allows, Anthropic-only implementation).
- Prompt self-tuning via self-modification (Phase 1 allows **tool authoring only**).
- Behavioral regression tests for prompt drift (needed once a second role exists).
- Additional messengers: MS Teams, Telegram, Discord.
- SSO, RBAC, multi-tenancy, Enterprise Grid automation, on-prem hardening.

---

## 4. Milestones

### M1 — Foundation [x]

**Goal**: Repo skeleton and a quality toolchain that forces LLM-authored and human-authored code to the same bar.

**Scope**

- [x] **M1.1** **Layout**: monorepo — `/core` (Go), `/harness` (TS), `/tools-builtin` (TS, vendored tools), `/cli` (Go), `/deploy` (docker-compose), `/docs`, `/scripts`.
- [x] **M1.2** **Build systems**: Go module, pnpm workspace for TS, Dockerfiles per service, docker-compose skeleton with Postgres + Grafana stubs.
- [x] **M1.3** **Makefile as the only supported entry point** — every operational and dev action exposed as a `make <target>`; `make help` lists them all. Targets include at minimum: `up`, `down`, `test`, `lint`, `fmt`, `build`, `ci`, `secrets-scan`, `deps-scan`, `smoke`, `tools-sync`, `spawn-dev-bot`, `wk`.
- [x] **M1.4** **Go quality stack**:
  - `golangci-lint` with aggressive preset: `staticcheck`, `revive`, `gosec`, `errcheck`, `gofumpt`, `gocyclo`, `ineffassign`, `unused`, `depguard`, `bodyclose`, `noctx`, `contextcheck`.
  - `go test -race -cover` with coverage threshold (start at 60%, ratchet up).
  - `govulncheck` on every CI run.
- [x] **M1.5** **TypeScript quality stack**:
  - `tsc` with `strict`, `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes`.
  - `eslint` with `typescript-eslint` strict preset + `no-floating-promises`, import sorting, `no-restricted-imports` gating undeclared network/fs.
  - `prettier` for formatting.
  - `vitest` with coverage threshold.
  - `osv-scanner` or `npm audit` on every CI run.
- [x] **M1.6** **Cross-cutting linters**: `sqlfluff` (migrations), `shellcheck` (scripts), `hadolint` (Dockerfiles), `markdownlint` (docs), `yamllint` (configs).
- [x] **M1.7** **Secret scanning**: `gitleaks` in pre-commit and CI.
- [x] **M1.8** **Pre-commit framework**: `lefthook` running format + lint + secret scan on staged files.
- [x] **M1.9** **Commit quality**: `commitlint` with conventional-commits config; CI blocks non-conforming titles on PR.
- [x] **M1.10** **Dependency hygiene**: Renovate (preferred) or Dependabot config; license checker (allowlist policy).
- [x] **M1.11** **CI pipeline**: GitHub Actions (or equivalent) with parallel jobs — `go-ci`, `ts-ci`, `sql-ci`, `security-ci`, `docker-ci`; cached deps; required status checks on main.
- [x] **M1.12** **Developer bootstrap doc**: `docs/DEVELOPING.md` — prerequisites, `make bootstrap`, run the smoke path.

**Artifacts**: repo skeleton, Makefile, all lint/test configs, CI workflows, pre-commit config, developer bootstrap doc.

**Verification**

- [x] `make ci` locally reproduces the full CI matrix and passes.
- [x] `make help` lists every supported action.
- [x] `gitleaks` in pre-commit blocks a test-planted secret.
- [x] A PR with a convention-breaking commit title is blocked by CI.

**Dependencies**: none.
**Magnitude**: 3–5 days.

---

### M2 — Keep service (business knowledge + audit) [x]

**Goal**: Standalone Keep service that holds the organization's business knowledge and the platform's audit log. Every agent and core subsystem accesses it through the Keep API — no direct DB access from callers.

**Scope**

- [x] **M2.1** Keep schema foundation (business-domain only, protocol-neutral DDL). Subsumes former M2.1–M2.5. **No infrastructure-metadata columns** (explicitly no `deployment_id` — multi-environment is multi-instance). Publisher **worker** scaffold moves to M2.7 to preserve the standalone-Keep boundary; only the table lands here.
  - [x] **M2.1.a** Core tables DDL: `organization`, `human`, `watchkeeper`, `manifest`, `manifest_version` (with `personality` + `language`), `watch_order`. FKs + primary indices.
  - [x] **M2.1.b** `keepers_log` table DDL + append-only trigger rejecting `UPDATE`/`DELETE` (+ trigger tests).
  - [x] **M2.1.c** `knowledge_chunk` table DDL + pgvector extension + HNSW index on `embedding` (+ `EXPLAIN` assertion that KNN uses HNSW).
  - [x] **M2.1.d** Row-Level Security: `scope ∈ {org, user:<id>, agent:<id>}` column on scoped tables, session-role setup, per-table policies, cross-scope rejection tests.
  - [x] **M2.1.e** `outbox` table DDL only (publisher worker → M2.7).
- [x] **M2.6** Migration tool chosen and wired (goose / atlas / sqlc).
- [x] **M2.7** **Keep service binary** (Go) exposing HTTP/gRPC API: `search`, `store`, `subscribe`, `log_append`, `log_tail`, `get_manifest`, `put_manifest_version`. Auth via short-lived capability tokens issued by core.
  - [x] **M2.7.a** Keep service skeleton, HTTP-vs-gRPC decision, health endpoint, config, Dockerfile.
  - [x] **M2.7.b** Capability-token auth middleware and token-issuance contract.
  - [x] **M2.7.c** Read endpoints: `search`, `get_manifest`, `log_tail` with contract tests.
  - [x] **M2.7.d** Write endpoints: `store`, `log_append`, `put_manifest_version` with contract tests.
  - [x] **M2.7.e** `subscribe` endpoint plus outbox publisher worker (from M2.1.e).
    - [x] **M2.7.e.a** Add subscribe streaming endpoint with in-process publish API and fan-out registry
    - [x] **M2.7.e.b** Add outbox publisher worker consuming outbox table into subscribe publish API
- [x] **M2.8** Go client package `keepclient` used by core and by harness (no direct DB access from either).
  - [x] **M2.8.a** keepclient package skeleton: HTTP transport, options, typed errors, capability-token injection, and health check.
  - [x] **M2.8.b** keepclient read endpoints: Search, GetManifest, LogTail with typed models and contract tests.
  - [x] **M2.8.c** keepclient write endpoints: Store, LogAppend, PutManifestVersion with typed models and contract tests.
  - [x] **M2.8.d** keepclient Subscribe SSE streaming method with reconnect/dedup hooks and contract tests.
    - [x] **M2.8.d.a** Add keepclient Subscribe SSE consumption with typed Event model and httptest contract tests.
    - [x] **M2.8.d.b** Add Subscribe reconnect policy, Last-Event-ID resume, and dedup hooks with integration smoke.
- [x] **M2.9** Manifest schema fields added: `personality` (free-text) and `language` (ISO code).
  - [x] **M2.9.a** Manifest `personality`/`language` constraints, validation, and docs (columns already landed with M2.1.a).

**Artifacts**: schema SQL, migrations, Keep service binary, Go `keepclient` package, RLS and append-only tests, contract tests against the Keep API.

**Verification**

- [x] Client `log_append` → server persists → client `log_tail` returns same event.
- [x] `UPDATE`/`DELETE` on `keepers_log` rejected by trigger.
- [x] RLS: client authenticated as agent A cannot `search` rows scoped to agent B.
- [x] Contract tests pass against a fresh Keep instance.

**Dependencies**: M1.
**Magnitude**: 4–6 days.

---

### M2b — Notebook library (per-agent embedded memory) [x]

**Goal**: Embedded library that gives every Watchkeeper a local, latency-free personal memory with archive/restore support, with no dependency on Keep availability for per-turn operation.

**Scope**

- [x] **M2b.1** SQLite + `sqlite-vec` embedded storage. Per-agent file at `$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite`. Schema: `entry(id, category ∈ {lesson, preference, observation, pending_task, relationship_note}, subject, content, embedding, created_at, last_used_at, relevance_score, superseded_by, evidence_log_ref, tool_version, active_after)`.
- [x] **M2b.2** Go library `notebook/` linked into the harness process (not a service). API: `Remember`, `Recall`, `Forget`, `Archive`, `Import`, `Stats`.
  - [x] **M2b.2.a** Notebook in-process CRUD: `Remember`, `Recall`, `Forget`, `Stats` — `Entry`/`RecallQuery`/`RecallResult` structs, sentinel errors, deferred `entry(category)` / `entry(active_after)` indexes.
  - [x] **M2b.2.b** Notebook snapshot lifecycle: `Archive` (VACUUM INTO + stream) and `Import` (spool, schema-validate, atomic rename, reopen); `ErrCorruptArchive` sentinel.
- [x] **M2b.3** `ArchiveStore` Go interface with two implementations:
  - `LocalFS` — default; tarballs to `$DATA_DIR/archives/notebook/<agent_id>/<timestamp>.tar.gz`.
  - `S3Compatible` — any S3-API endpoint (AWS S3, Cloudflare R2, Wasabi, SeaweedFS, Garage, customer-owned); config via endpoint URL + credentials through secrets interface.
  - [x] **M2b.3.a** ArchiveStore Go interface + LocalFS implementation: define ArchiveStore (Put/Get/List) with sentinel errors in `core/pkg/archivestore/`; LocalFS writes tarballs to `<root>/notebook/<agent_id>/<RFC3339>.tar.gz` with 0o700 dir mode and UUID validation, plus contract+round-trip tests against `notebook.Archive`/`Import`.
  - [x] **M2b.3.b** S3Compatible ArchiveStore implementation: minio-go-based backend with endpoint/accessKey/secretKey/bucket/secure config (plain struct; secrets interface deferred to M9); object keys `notebook/<agent_id>/<RFC3339>.tar.gz`; testcontainers-go minio integration smoke wired into parameterised contract suite.
- [x] **M2b.4** Archive on retire: harness invokes `Archive` during graceful shutdown; resulting URI emitted to Keeper's Log via `keepclient.log_append` (`notebook_archived` event).
- [x] **M2b.5** Periodic backup: cron-scheduled snapshot of live Notebook files to `ArchiveStore` (`notebook_backed_up` event). Cadence configurable.
- [x] **M2b.6** Import: `notebook.Import(agent_id, archive_uri)` restores a predecessor's archive into a fresh agent file. Operator-driven in Phase 1 via CLI (see M10). Auto-inheritance policy → ⏸ Phase 2.
- [x] **M2b.7** Every mutating operation writes a correlated event to Keeper's Log via the Keep client — Notebook has no audit surface of its own; Keep is the single audit authority.
- [x] **M2b.8** `promote_to_keep(entry_id)` helper that packages a Notebook entry for Watchmaster approval → Keep write (Watchmaster-side implementation in M6).

**Artifacts**: Go `notebook/` library, `archivestore/` package with two implementations, contract tests, benchmark suite.

**Verification**

- [x] `Remember` + `Recall` cycle returns correct top-K.
- [ ] Recall latency stays sub-millisecond at 10k entries (benchmark gated).
- [x] `Archive` to `LocalFS` and to an `S3Compatible` endpoint (docker test container); `Import` from each restores identical state.
- [x] Cross-agent isolation: process A cannot open process B's file (filesystem perms).
- [x] Every mutation produces a correlated Keeper's Log entry.

**Dependencies**: M2.
**Magnitude**: 3–5 days.

---

### M3 — Go core services [x]

**Goal**: In-process services every Watchkeeper relies on.

**Scope**

- [x] **M3.1** In-process event bus (pub/sub) with handler registration, ordered per-topic delivery, and backpressure.
- [x] **M3.2** Lifecycle manager: `Spawn`, `Retire`, `Health`, `List` for Watchkeeper processes; state persisted in `watchkeeper` table **via `keepclient`, not direct DB access**.
  - [x] **M3.2.a** keepclient watchkeeper resource CRUD (Insert/UpdateStatus/Get/List) — adds a thin client surface for the watchkeeper table mirroring the manifest/knowledge_chunk pattern.
  - [x] **M3.2.b** lifecycle: Spawn/Retire/Health/List manager over keepclient — consumes the new keepclient methods via a LocalKeepClient interface; logical lifecycle only (process supervision deferred to M5.3).
- [x] **M3.3** Cron scheduler (robfig/cron) that emits events onto the bus.
- [x] **M3.4** Config loader (env + `config.yaml`); secrets pluggable interface (env-first for Phase 1, Vault-ready).
  - [x] **M3.4.a** Secrets pluggable interface: `core/pkg/secrets/` defining `SecretSource` interface, `EnvSource` implementation (env-first for Phase 1, Vault-ready abstraction); functional-options constructor; cross-package compile-time interface check.
  - [x] **M3.4.b** Config loader: `core/pkg/config/` loading env vars + `config.yaml` via `gopkg.in/yaml.v3` into a strongly-typed Config struct with per-service sub-structs; validates required fields at Load time; resolves `*_secret` fields through the M3.4.a SecretSource interface (depends on M3.4.a).
- [x] **M3.5** Capability broker: issues scoped, short-lived tokens to tools and to `keepclient` / `notebook` / `archivestore` calls; validates on invocation; enforces TTL.
- [x] **M3.5.a** (security): plumb `organization_id` through `auth.Claim` so per-tenant filtering is enforceable in keep handlers — cross-tenant gap blocking PR #49 production-ready posture. Required (1) adding `OrganizationID` field to `auth.Claim` and the capability-broker mint path, and (2) landing per-org RLS policies on `human` and `watchkeeper` tables (keyed off the same scope/org GUC that `knowledge_chunk` uses — migration 005). Split into **a.1** (foundation: `auth.Claim` field + JWT round-trip + `capability.Broker.IssueForOrg`/`ValidateForOrg` sibling methods, no handler changes) and **a.2** (handler wire-up, closes the gap at the handler layer). Per-org RLS migrations on `human` / `watchkeeper` remain a defense-in-depth follow-up; the M3.5.a.2 handler enforcement closes the M4.4 cross-tenant gap on its own.
  - [x] **M3.5.a.1** Foundation: `auth.Claim.OrganizationID` field + JWT `org_id` round-trip (legacy tokens still parse); `capability.Broker.IssueForOrg` / `ValidateForOrg` sibling methods that bind tokens to a tenant in addition to a scope; `auth.TestIssuer.Issue` carries the field through verbatim. NO handler changes — keep handlers still trust request-body `organization_id` (DEVELOPING.md "Security posture" note unchanged).
  - [x] **M3.5.a.2** Handler wire-up: `handleInsertHuman`, `handleSetWatchkeeperLead`, `handleUpdateWatchkeeperStatus`, and `handleInsertWatchkeeper` consume `claim.OrganizationID`. `handleInsertHuman` cross-checks `body.organization_id == claim.OrganizationID` and rejects mismatches with `403 organization_mismatch`. The three `watchkeeper`-table mutators filter the target / anchored row through a `human.organization_id` filter keyed by the claim's tenant (`watchkeeper.watchkeeper` carries no `organization_id` column of its own — see migration 002): the two existing mutators use a JOIN/subquery on `watchkeeper.human`; `handleInsertWatchkeeper` uses an `INSERT … SELECT … WHERE EXISTS` shape so a cross-tenant `lead_human_id` produces no row through `RETURNING` and surfaces as `404 not_found`. Legacy claims (empty `OrganizationID`) are rejected with `403 organization_required`. DEVELOPING.md "Security posture" updated to past-tense. Per-org RLS migration on `human` / `watchkeeper` is deferred as a defense-in-depth follow-up. The `handlePutManifestVersion` cross-tenant gap remains open and is tracked at M3.5.a.3 (requires a schema migration before handler wire-up).
  - [x] **M3.5.a.3** (security): close cross-tenant write gap on `handlePutManifestVersion`. Requires schema migration (add `manifest.organization_id` column + FK to `watchkeeper.organization` + RLS policy on `manifest` / `manifest_version`) before handler wire-up — the `manifest` table currently has no `organization_id` column (see migration 002:30-35), so the handler cannot filter on tenant without a schema change comparable in size to migration 005. Gate on a separate migration PR; once landed, mirror the M3.5.a.2 wire-up shape on the PUT path so cross-tenant callers surface as `404 not_found` and legacy claims as `403 organization_required`.
    - [x] **M3.5.a.3.1** Schema migration: `013_manifest_org_id_rls.sql` adds `manifest.organization_id` column + FK + per-role RLS (ENABLE + FORCE) on both `manifest` and `manifest_version`. `WithScope` plumbs `watchkeeper.org` GUC; legacy claims fail closed via `nullif(..., '')::uuid`. NO handler changes — `handlePutManifestVersion` still trusts request input.
    - [x] **M3.5.a.3.2** Handler wire-up: `handlePutManifestVersion` consumes `claim.OrganizationID` via an `INSERT … SELECT … WHERE EXISTS (SELECT 1 FROM watchkeeper.manifest WHERE id = $manifest_id AND organization_id = $claim_org)` shape; cross-tenant `manifest_id` surfaces as `404 not_found` (no row-existence oracle) and legacy claims as `403 organization_required` before `WithScope` opens any tx. RLS on `manifest` / `manifest_version` from M3.5.a.3.1 is the defense-in-depth backstop. DEVELOPING.md "Security posture" updated to past-tense final state covering all five wired handlers.
- [x] **M3.6** Keeper's Log writer (thin wrapper on `keepclient.log_append`): structured event schema, correlation IDs, trace context propagation.
- [x] **M3.7** Outbox consumer: reads `outbox` from Keep via `keepclient.subscribe`, publishes to event bus with at-least-once semantics and idempotency keys.

**Artifacts**: Go packages `eventbus`, `lifecycle`, `cron`, `capability`, `keeperslog`, `outbox`.

**Verification**

- [ ] Integration test: spawn a mock Watchkeeper, fire a cron event, Watchkeeper receives it, Keeper's Log contains both the cron-fired and the handler-ran events with matching correlation IDs.
- [x] Capability token expires exactly at TTL; use after expiry rejected.
- [x] Outbox consumer is at-least-once and idempotent under forced redeliveries.

**Dependencies**: M2.
**Magnitude**: 5–8 days.

---

### M4 — Messenger adapter + Slack integration [x]

**Goal**: A Watchkeeper can appear as a Slack bot, receive and send messages.

**Scope**

- [x] **M4.1** `MessengerAdapter` Go interface: `SendMessage`, `Subscribe`, `CreateApp`, `InstallApp`, `SetBotProfile`, `LookupUser`.
- [x] **M4.2** Slack implementation:
  - Slack Manifest API client (parent app holds `app_configuration` scope).
  - OAuth install flow (admin-preapproval path for dev workspace).
  - Bot profile setup via `users.profile.set`, `bots.info`.
  - Event intake via **Socket Mode** (no public HTTPS required for Phase 1).
  - Rate limiter aware of Slack tier-2/tier-3 budgets.
- [x] **M4.3** Dev workspace bootstrap script (creates parent app from manifest, grants scopes, stores credentials via secrets interface).
- [x] **M4.4** Human identity mapping: `human` row keyed by Slack user ID; lead → Watchkeeper relation modeled.

**Artifacts**: `messenger/` package, `messenger/slack/` adapter, bootstrap script, operator doc section "Provisioning the dev Slack workspace".

**Verification**

- [ ] `make spawn-dev-bot` creates a new Slack child app in the dev workspace, installs it, the bot appears as a workspace member and echoes a DM sent to it.
- [ ] Rate limiter honors tier-2 burst + sustained limits under load test.
- [x] Parent-app credentials never leave the secrets interface (grep the built binary for raw tokens — none).

**Dependencies**: M3.
**External prerequisite**: dev Slack workspace provisioned.
**Magnitude**: 5–7 days.

---

### M5 — Runtime adapter + Claude Code bridge [x]

**Goal**: Go core can launch and drive a TS agent harness that uses Claude Code under the hood; tools execute in isolation; the LLM provider is swappable without touching core.

**Scope**

- [x] **M5.1** `AgentRuntime` Go interface: `Start`, `SendMessage`, `InvokeTool`, `Terminate`, plus streaming event hook.
- [x] **M5.2** `LLMProvider` interface (separate from `AgentRuntime`): `Complete`, `Stream`, `CountTokens`, `ReportCost`. Default implementation wraps Claude Code (via Claude Agent SDK if embedding, or as a subprocess if shelling out). _Interface ships in `core/pkg/llm/` (M5.2.a); the Claude Code default impl is deferred to a follow-up M5.2.b._
- [x] **M5.3** **TS harness process** — JSON-RPC stdio scaffold, isolated-vm + worker-process tool paths, zod-derived schemas, Claude Code wired via the `LLMProvider` wrapper.
  - [x] **M5.3.a** Scaffold + JSON-RPC framing + hello/shutdown.
  - [x] **M5.3.b.a** isolated-vm pure-JS invocation path.
  - [x] **M5.3.b.b** Worker-process tool path with capability gating (substrate ADR + zod policy + transport + dispatcher landed; runtime test suite still pending under M5.3.c).
  - [x] **M5.3.c** **Finish harness**: vitest suite covering worker-path execution and capability-gating denials; tool schemas auto-derived from Tool Manifest via `zod`; Claude Code integration via the `LLMProvider` wrapper (model, system prompt, context parameterized from Manifest). _(vitest suite portion already satisfied by tests landed in PRs #57–#61: `worker-spawn.test.ts`, `invokeTool-worker.test.ts`, `worker-broker.test.ts` — 172 tests / 95% harness coverage; outstanding work decomposed below.)_
    - [x] **M5.3.c.a** Auto-derive zod tool schemas from Tool Manifest at harness boot
    - [x] **M5.3.c.b** LLMProvider wrapper: parameterize model/system-prompt/context from Manifest
    - [x] **M5.3.c.c** Wire Claude Code as default LLMProvider impl into harness loop
      - [x] **M5.3.c.c.a** Add TS LLMProvider interface + FakeProvider mirroring Go contract
      - [x] **M5.3.c.c.b** Implement ClaudeCodeProvider adapter (default impl) with unit tests
      - [x] **M5.3.c.c.c** Wire LLMProvider into harness loop via complete/stream JSON-RPC methods
        - [x] **M5.3.c.c.c.a** Wire complete + countTokens + reportCost JSON-RPC methods with provider injection
        - [x] **M5.3.c.c.c.b** Wire stream JSON-RPC method with multi-event notification protocol
          - [x] **M5.3.c.c.c.b.a** Add JSON-RPC notification builder + inject shared stdout writer into LLM method wiring
          - [x] **M5.3.c.c.c.b.b** Implement stream + stream/cancel JSON-RPC methods with stream registry and multi-event notification protocol
- [x] **M5.4** **Sandbox guardrails** — per-tool resource limits (wall-clock, CPU time, memory ceiling, output-byte cap) enforced by Go core via process controls and isolate options.
  - [x] **M5.4.a** Sandbox guardrails — wall-clock timeout + output-byte cap (Go-side timer + wrapped stdout/stderr readers, no syscalls)
  - [x] **M5.4.b** Sandbox guardrails — CPU-time + memory-ceiling rlimits (platform-specific setrlimit via SysProcAttr, build-tagged sandbox_linux.go / sandbox_darwin.go)
- [x] **M5.5** **Manifest-driven boot + Notebook integration** — harness calls `keepclient.GetManifest(agent_id)` on boot, composes `personality`/`language` into the effective system prompt via a templater, applies toolset ACLs / model / autonomy, opens its per-agent SQLite Notebook, auto-recalls top-K relevant entries each turn (configurable K + relevance threshold), and exposes `Remember` as a built-in tool.
  - [x] **M5.5.a** Harness boot fetches Manifest via keepclient and templates personality/language into system prompt
  - [x] **M5.5.b** Apply Manifest toolset ACLs, model selection, and autonomy bounds in harness loop
    - [x] **M5.5.b.a** Decode Manifest toolset jsonb and enforce ACLs at harness InvokeTool gate
    - [x] **M5.5.b.b** Project Manifest model field into LLMProvider config at runtime boot
      - [x] **M5.5.b.b.a** Add manifest_version.model column + server response projection
      - [x] **M5.5.b.b.b** Extend keepclient.ManifestVersion with Model field + decoder tests
      - [x] **M5.5.b.b.c** Project Model via manifest loader into LLMProvider boot config
    - [x] **M5.5.b.c** Decode authority_matrix and apply autonomy bounds at approval gates
      - [x] **M5.5.b.c.a** Add manifest_version.autonomy column + server PUT/GET projection
      - [x] **M5.5.b.c.b** Extend keepclient.ManifestVersion with Autonomy field + tests
      - [x] **M5.5.b.c.c** Project AuthorityMatrix + Autonomy in loader; enforce at approval gate
        - [x] **M5.5.b.c.c.a** Project AuthorityMatrix + Autonomy in manifest loader
        - [x] **M5.5.b.c.c.b** Add runtime authority/autonomy enforcement at approval gate
  - [x] **M5.5.c** Open per-agent SQLite Notebook on boot and auto-recall top-K entries with relevance threshold
    - [x] **M5.5.c.a** Add manifest_version columns notebook_top_k + notebook_relevance_threshold + server projection
    - [x] **M5.5.c.b** Extend keepclient.ManifestVersion with NotebookTopK / NotebookRelevanceThreshold + loader projection
    - [x] **M5.5.c.c** Open per-agent Notebook on harness boot; close on terminate
    - [x] **M5.5.c.d** Auto-recall top-K with relevance threshold per turn; inject into LLM request
      - [x] **M5.5.c.d.a** Introduce llm.EmbeddingProvider seam (interface + in-process fake) for per-turn query embedding
      - [x] **M5.5.c.d.b** Add llm.WithRecalledMemory option + manifest-aware turn helper that calls notebook.DB.Recall(topK, threshold) via NotebookSupervisor and injects results into the LLM request
        - [x] **M5.5.c.d.b.a** Add llm.WithRecalledMemory option + RecalledMemory shape + injection into BuildCompleteRequest/BuildStreamRequest/BuildCountTokensRequest System slot
        - [x] **M5.5.c.d.b.b** Add manifest-aware turn helper (BuildTurnRequest) that calls NotebookSupervisor.Lookup + EmbeddingProvider.Embed + notebook.DB.Recall(TopK, Threshold) and threads results via WithRecalledMemory, with fail-soft matrix
  - [x] **M5.5.d** Expose Remember as a built-in harness tool writing to per-agent Notebook
    - [x] **M5.5.d.a** Add notebook.remember JSON-RPC method to TS harness wired to NotebookSupervisor.Lookup via in-process Go bridge, with embedding computed Go-side via EmbeddingProvider
      - [x] **M5.5.d.a.a** Bidirectional NDJSON JSON-RPC framing: extend harness/src/jsonrpc.ts with parseResponse + request-emitting client (id correlation, pending-request map), and add core/pkg/harnessrpc Go-side stdio host that reads inbound harness requests and writes responses, validated end-to-end with a no-op echo method
      - [x] **M5.5.d.a.b** Wire notebook.remember over the M5.5.d.a.a bridge: register notebook.remember method on the Go host that calls llm.EmbeddingProvider.Embed then NotebookSupervisor.Lookup + notebook.DB.Remember, and add the TS-side client emitter
    - [x] **M5.5.d.b** Register Remember as a built-in harness tool: extend invokeTool dispatch with a 'builtin' kind that routes to the notebook.remember method, gated by the M5.5.b.a manifest ACL
    - [x] **M5.5.d.c** Manifest projection + end-to-end test: surface 'remember' in manifest_version.tools projection, assert ACL allow/deny, and add an integration test that drives invokeTool('remember', {category, content}) through to a per-agent SQLite notebook.DB row
- [x] **M5.6** **Reflection lifecycle** — auto-reflection on tool error writes a `lesson` entry with `evidence_log_ref`, `tool_version`, and `active_after = now() + 24h` (visible but not auto-injected during the cooling-off window); on tool hot-load, lessons tied to a superseded version are flagged `needs_review` and excluded from auto-injection until reviewed (never deleted).
  - [x] **M5.6.a** Add needs_review column + cooling-off/exclusion predicates in notebook.DB
  - [x] **M5.6.b** Auto-reflect on tool error: compose lesson Entry and write via Remember
  - [x] **M5.6.c** Emit lesson_learned to Keeper's Log with evidence_log_ref linkage
  - [x] **M5.6.d** Gate auto-injection in BuildTurnRequest by active_after and needs_review
  - [x] **M5.6.e** Boot-time hot-load check: flag lessons on superseded tool_version as needs_review
    - [x] **M5.6.e.a** Project per-tool Version through manifest loader into runtime.Manifest (typed ToolEntry); migrate ACL/Toolset consumers via Names() helper
    - [x] **M5.6.e.b** Boot-time superseded-lesson scan in notebook supervisor: flag lessons whose tool_version != current manifest version via MarkNeedsReview
  - [x] **M5.6.f** E2E verification: forced tool error produces lesson + cooling-off injection behavior
- [x] **M5.7** **Provider plumbing** — Claude Code credentials flow through the secrets interface (no `ANTHROPIC_API_KEY` references in core); a dummy `FakeProvider` passes the same harness tests as the real provider, proving swap-without-touching-core.
  - [x] **M5.7.a** Route Claude Code credentials through secrets.SecretSource and enforce no ANTHROPIC_API_KEY literal in core/ (grep-invariant CI check)
  - [x] **M5.7.b** FakeProvider conformance suite — parameterised TS tests run against both FakeProvider and ClaudeCodeProvider proving swap-without-touching-core

**Artifacts**: Go `runtime/` and `llm/` packages, TS `harness/` package, JSON-RPC contract doc, provider conformance test harness.

**Verification**

- [ ] Go core spawns harness from a fake Manifest; harness calls an "echo" tool in `isolated-vm`; result returns.
- [ ] A runaway test tool is killed by the wall-clock limit.
- [ ] A tool that tries undeclared network access is rejected by the capability broker.
- [ ] Replacing `LLMProvider` with `FakeProvider` runs the full harness suite without code changes outside the provider package.
- [ ] Auto-injection: seed Notebook with a lesson, start a new turn, observe lesson content in the prompt window.
- [ ] Auto-reflection: force a tool error, check that a `lesson` entry appears with `active_after` 24h in the future and `lesson_learned` is in Keeper's Log.

**Dependencies**: M3, M2b.
**External prerequisite**: Claude Code installed on the host with valid credentials.
**Magnitude**: 7–10 days.

---

### M6 — Watchmaster (meta-agent) [x]

**Goal**: First concrete Watchkeeper — the orchestrator humans talk to.

**Scope**

- [x] **M6.1** **Watchmaster manifest + privilege boundary** — system prompt, authority matrix, and the rule that the Watchmaster never executes Slack App creation directly (calls a core-owned privileged RPC instead).
  - [x] **M6.1.a** Watchmaster manifest seed + authority matrix entries — canonical Watchmaster manifest (personality, language, orchestrator system prompt) plus authority_matrix entries declaring which privileged actions Watchmaster may request, request-with-approval, or is forbidden from invoking.
  - [x] **M6.1.b** Core-owned privileged RPC for Slack App creation — new core RPC package that owns Slack Manifest API credentials and enforces approvals, exposed to Watchmaster as a tool so the orchestrator never executes Slack App creation directly.
- [x] **M6.2** **Toolset** — `list_watchkeepers`, `propose_spawn` (drafts Manifest with `personality`/`language`), `retire_watchkeeper`, `report_cost`, `report_health`, `adjust_personality` / `adjust_language` (both draft a new Manifest version through lead approval), `promote_to_keep(agent_id, notebook_entry_id)` (lead-approved write into Keep as org-scoped knowledge, emits `notebook_promoted_to_keep`).
  - [x] **M6.2.a** Read-only tools: list_watchkeepers, report_cost, report_health (no approval gate; reads watchkeeper table, keepers_log cost rollups, runtime health)
  - [x] **M6.2.b** Manifest-bump tools (lead-approval gated): propose_spawn, adjust_personality, adjust_language — all draft a new manifest_version via PutManifestVersion
  - [x] **M6.2.c** retire_watchkeeper (lead-approval gated) — lifecycle flag flip, separated from manifest bumps to keep PR scope tight
  - [x] **M6.2.d** promote_to_keep(agent_id, notebook_entry_id) (lead-approval gated) — read-then-write across notebook→keep boundary, emits notebook_promoted_to_keep event
- [x] **M6.3** **Operator surface** — Slack DM conversation with designated admins (Manifest drafts rendered as Slack blocks with Approve/Reject actions) and per-Watchkeeper cost tracker (prompt + completion tokens, daily/weekly rollups persisted in Keep).
  - [x] **M6.3.a** Slack inbound webhook scaffolding (Events API + Interactivity) — request signature verification, dispatch skeleton, no business logic
  - [x] **M6.3.b** Manifest-draft approval card renderer + pending-approval DAO — Slack blocks for `propose_spawn` / `adjust_personality` / `adjust_language` / `retire_watchkeeper`, Approve/Reject button handler that resolves approval tokens
  - [x] **M6.3.c** DM intent router wiring read-only + manifest-bump tools — parses "what's running?", "propose Coordinator for backend team", etc., dispatches to existing M6.2 toolset
  - [x] **M6.3.d** promote_to_keep diff-preview renderer for approval cards — extends M6.3.b's renderer with notebook-entry diff
  - [x] **M6.3.e** Per-Watchkeeper token spend recording on LLM calls — `keepers_log` event emitted per call with prompt + completion tokens, agent_id, model
  - [x] **M6.3.f** Daily/weekly cost rollups persisted in Keep — aggregation tables / queries, optional CLI/Slack surface

**Artifacts**: Watchmaster manifest file, toolset TS implementations, Slack interaction flow.

**Verification**

- [ ] Admin DMs "what's running?" → Watchmaster replies with a live list.
- [ ] Admin DMs "propose a Coordinator for the backend team" → Watchmaster posts a Manifest draft with Approve / Reject buttons.
- [ ] `adjust_personality` drafts a Manifest version bump that goes through lead approval.
- [ ] `promote_to_keep` surfaces a diff preview and requires explicit lead approval before the Keep write.

**Dependencies**: M4, M5.
**Magnitude**: 4–6 days.

---

### M7 — Spawn Flow end-to-end [x]

**Goal**: The flow from `watchkeeper-spawn-flow.md` works front-to-back.

**Scope**

- [x] **M7.1** **Spawn saga (forward path)** — Watchmaster posts the draft Manifest in Slack (Approve/Reject blocks); approval writes an event to Keeper's Log and triggers a core RPC saga that chains: Manifest approval → Slack App create → OAuth install → bot profile set → provision per-agent Notebook file → runtime launch (personality/language applied) → intro message.
  - [x] **M7.1.a** Saga skeleton: spawn/ package, state machine, saga-state DAO + migration
  - [x] **M7.1.b** Slack interaction handler: approval action → Keeper's Log event → saga kickoff
  - [x] **M7.1.c** Slack App provisioning step: create app + OAuth install + bot profile set
    - [x] **M7.1.c.a** CreateApp saga step + watchkeeper secrets column + migration
    - [x] **M7.1.c.b** OAuthInstall saga step with encrypted bot-token storage
      - [x] **M7.1.c.b.a** Extend core/pkg/secrets with AES-GCM Encrypt/Decrypt primitive + KEK resolution
      - [x] **M7.1.c.b.b** OAuthInstall saga step + encrypted bot-token storage using secrets crypto primitive
    - [x] **M7.1.c.c** BotProfile saga step + register step list in spawn kickoff wiring
  - [x] **M7.1.d** Notebook provision step: per-agent Notebook file with personality/language
  - [x] **M7.1.e** Runtime launch + intro message step, wiring saga to completion
- [x] **M7.2** **Retire saga** — harness `Archive` runs; tarball lands in `ArchiveStore` (LocalFS or S3-compatible); `notebook_archived` event with archive URI logged; Watchkeeper row in Keep marked retired with archive reference.
  - [x] **M7.2.a** Retire saga kickoff seam: RetireSagaContext + RetireKickoff seam + audit chain + production-wiring helper (zero-step Run; mirrors M7.1.b)
  - [x] **M7.2.b** NotebookArchive saga step: thin seam over `notebook.ArchiveOnRetire`; archive_uri returned via SpawnContext-equivalent
  - [x] **M7.2.c** MarkRetired saga step + keepclient `archive_uri` extension + watchkeepers.archive_uri column + migration; wires M6.2.c retire tool through the saga
- [x] **M7.3** **Robustness** — saga compensations (install failure rolls back Slack App creation, removes the freshly-provisioned Notebook file, marks Manifest rejected; runtime boot failure tears down the app and **archives** — never deletes — any Notebook data written, flagged for review) plus idempotency keys so retried approvals never double-create apps.
  - [x] **M7.3.a** Idempotency keys on spawn + retire saga kickoffers — `spawn_sagas.idempotency_key text NULL UNIQUE` column + migration; `SpawnSagaDAO.InsertIfAbsent(ctx, id, manifestVersionID, idempotencyKey)`; both kickoffers derive idempotency_key from approval_token, on duplicate emit `*_replayed_*` audit event and return nil (no second saga, no second Slack App, no second runtime). Defense-in-depth: empty key rejected at DAO layer; UNIQUE index allows multiple legacy NULL rows.
  - [x] **M7.3.b** Compensation infrastructure in `saga` package + manifest-rejected emit on rollback — extend `saga.Step` with optional `Compensator` interface; `saga.Runner` calls `Compensate` in reverse order for all successfully-executed steps when a later step fails; new audit events `saga_step_compensated` / `saga_compensation_failed`; on full rollback the kickoffer emits `manifest_rejected_after_spawn_failure` so the Manifest is marked rejected and the operator is surfaced. No concrete Compensate impls — foundation for M7.3.c.
  - [x] **M7.3.c** Per-step Compensate implementations + fault-injection harness — `CreateApp.Compensate` (Slack App teardown via SlackAppRPC), `OAuthInstall.Compensate` (revoke + wipe encrypted bot-token), `BotProfile.Compensate` (no-op explicit), `NotebookProvision.Compensate` (archive-not-delete + flag-for-review), `RuntimeLaunch.Compensate` (runtime teardown, cost-record finalisation). Fault-injection harness test: kill saga during step N → reverse compensations execute, Slack App deleted, Notebook archived with review flag, Manifest marked rejected.

**Artifacts**: `spawn/` saga package, Slack interaction handler for approval actions, saga-state DAO.

**Verification**

- [x] Scripted end-to-end: admin DMs Watchmaster "spawn coordinator for backend team", approves the draft, new bot appears in the workspace within 90 seconds posting its intro message.
- [x] Fault-injection: kill the runtime during step 5 → saga rolls back the Slack App and surfaces failure to the admin.
- [x] Idempotency: same approval re-submitted does not create a second Slack app.
- [x] Retire flow archives Notebook before tearing down runtime.

**Dependencies**: M6.
**Magnitude**: 4–6 days.

---

### M8 — Coordinator Watchkeeper + Jira adapter [x]

**Goal**: A second role exists and performs real work.

**Scope**

- [x] **M8.1** **Jira adapter** — REST via `go-jira` or direct HTTP: JQL search, read, comment, and update of a whitelisted set of fields.
- [x] **M8.2** **Coordinator manifest + toolset** — system prompt and authority matrix (comment + field-update allowed; no reassignment without lead approval); tools `fetch_watch_orders` (reads Slack DMs from lead), `find_stale_prs`, `find_overdue_tickets`, `nudge_reviewer`, `post_daily_briefing`, `update_ticket_field`. Decomposed below by adapter scope so each sub-item is shippable end-to-end.
  - [x] **M8.2.a** **Coordinator manifest seed + tool dispatch primitive + first Jira-write tool (`update_ticket_field`)** — migration seeding ONE `organization` (reuse system tenant `00000000-…`) + ONE `manifest` + ONE `manifest_version` row under stable `CoordinatorManifestID`, mirroring migration 017 (Watchmaster); system prompt encoding the Coordinator role, lead-deferral discipline, audit boundaries, and PII restrictions; authority matrix using the runtime authority vocabulary (`"self"`/`"lead"`/`"operator"`/`"watchmaster"` per `core/pkg/runtime/authority.go`) with `tools.update_ticket_field.assignee → lead` (no reassignment without lead approval) and other whitelisted writes → `self`; `core/pkg/manifest/coordinator.go` exporting `CoordinatorManifestID`; runtime tool-dispatch primitive — a `ToolHandler` registry on the wired runtime that maps `Toolset` names to concrete handler funcs and gates each `InvokeTool` call through both `Manifest.Toolset` membership AND `RequiresApproval`; first concrete handler `update_ticket_field` consuming the M8.1 `jira.Client.UpdateFields` whitelist write path; per-call resolver shape for the Jira `BasicAuthSource` so the Coordinator's tenant-scoped credentials never become a process-global static.
  - [x] **M8.2.b** **Jira read tool — `find_overdue_tickets`** — JQL composition with assignee + status + age threshold + project scope; cursor pagination via the M8.1 `jira.Client.Search` adapter; result projection through the shared M8.1 `issueWire.toIssue()` decoder; Coordinator manifest seed migration extends the `Toolset` array to include the new tool and the authority matrix grants `self` (read-only).
  - [x] **M8.2.c** **Slack tools bundle — `fetch_watch_orders` + `nudge_reviewer` + `post_daily_briefing`** — three tools sharing the M4.2 slack adapter: `fetch_watch_orders` reads Coordinator-DM history filtered to the lead's Slack user id (read-only, `self`); `nudge_reviewer` posts a templated nudge DM to a Slack user id (write, `self`); `post_daily_briefing` posts a structured briefing payload to a configured channel id (write, `self`); Coordinator manifest seed migration extends the `Toolset` and authority matrix; one shared briefing-formatter helper avoids per-tool duplication.
  - [x] **M8.2.d** **GitHub PR tool — `find_stale_prs` (REQUIRES new `core/pkg/github` adapter)** — sibling adapter to `core/pkg/jira` (REST v3 stdlib HTTP client, `BasicAuthSource`/`TokenSource` per-call resolver, repo + PR list + age filter, fail-closed defaults); `find_stale_prs` consumes the adapter to surface PRs older than a configurable threshold filtered by reviewer; manifest seed update extends `Toolset`. Magnitude is comparable to M8.1 because of the new adapter — split is intentional: the prior six tools all reuse existing M8.1/M4.2 adapters and ship cleanly without GitHub work.
- [x] **M8.3** **Watch Orders + scheduled work** — natural-language Watch Order parser distills lead priorities into a persistent task list in Keep with round-trip confirmation; cron-driven daily briefing at configurable time and morning overdue sweep; daily briefing includes a pending-lesson digest section listing lessons in the 24h cooling-off window (lead replies `forget <id>` to kill before activation; auto-activate otherwise).

**Artifacts**: `jira/` adapter package, Coordinator manifest, tool implementations, parser tests.

**Verification**

- [x] Coordinator is spawned via the M7 flow.
- [x] Reads a Watch Order from a Slack DM; parser round-trips confirmation with the lead.
- [x] Finds an overdue ticket in the test Jira project; posts a nudge comment.
- [x] Posts a daily briefing to the configured Slack channel; briefing includes pending-lesson digest when applicable.

**Dependencies**: M7.
**External prerequisite**: Jira test project provisioned with API credentials.
**Magnitude**: 5–7 days.

---

### M9 — Multi-source Tool Registry + self-modification loop [ ]

**Goal**: Watchkeepers can draft a new tool in the appropriate source; platform updates never destroy customer-private tools; all sources converge into a single effective toolset per Watchkeeper.

**Tool source model (overlay; higher priority shadows lower)**

1. **Built-in** — vendored inside the core release (folder `/tools-builtin/`). Immutable at runtime. Signed by the platform release process. Baseline tools every deployment has.
2. **Platform** — public `watchkeeper-tools` git repo, pulled by the core over the network. Signed by platform release tags. Delivers platform-blessed tools without requiring a core rebuild.
3. **Private** — customer-scoped tools. **Two interchangeable modes** configured per deployment:
   - **`git` mode** — customer-owned git repo(s); URL + auth + branch in config; can live behind VPN. Home of customer-specific features for engineering-led customers. **Never touched by platform updates.**
   - **`hosted` mode** — code stored on local filesystem at `$DATA_DIR/tools/private/` on the platform host, mounted as a persistent docker-compose volume. Survives platform upgrades (the volume is not touched on container replace). Each tool is a folder with `manifest.json` + `tool.ts` + tests. **Not stored in Keep** — Keep is business knowledge only; tool source is infrastructure.
4. **Local** — on-host filesystem directory (`$DATA_DIR/tools-local/`). Operator quick-patches for emergencies or debugging. Loudest audit trail.

Both private modes are read by the same overlay resolver — the runtime sees no difference between `git`-sourced and `hosted`-sourced tools at invocation time.

**Scope**

- [ ] **M9.1** **Source config + sync + manifest + hot-reload** — `config.yaml` lists sources in priority order with per-source auth / branch / pull policy (`on-boot` | `cron <schedule>` | `on-demand`); ToolSyncScheduler clones/pulls each into `$DATA_DIR/tools/<source-name>/`, verifies signatures when enabled, recomputes the effective toolset on change, and emits `source_synced` / `source_failed`; per-tool `manifest.json` carries `name`, `version`, `capabilities`, zod-compatible `schema`, auto-filled `source`, and optional `signature`; on toolset change running runtimes receive an update signal — in-flight calls complete on the old version, new invocations use the new, grace period configurable. Decomposed below into a data+sync layer and a runtime layer so each sub-item is shippable end-to-end.
  - [x] **M9.1.a** **`tool_sources` config block + per-tool `manifest.json` schema + ToolSyncScheduler** — operator config gains a `tool_sources` section (ordered list of `{name, kind ∈ {git, local, hosted}, url, branch, pull_policy ∈ {on-boot, cron <schedule>, on-demand}, auth_secret}`); per-tool `manifest.json` schema (`name`, `version`, `capabilities`, zod-compatible `schema`, auto-filled `source`, optional `signature`) with a strict loader that rejects undeclared fields and missing required values; new `core/pkg/toolregistry/` package houses (1) the manifest decoder + validator, (2) a `ToolSyncScheduler` with file-system, git-clone, clock, and signature-verifier seams (default signature-verifier is a no-op — real cosign/minisign lands in M9.3) that clones/pulls each configured source into `$DATA_DIR/tools/<source-name>/` per its `pull_policy`, (3) emission of `source_synced` / `source_failed` events onto a dedicated eventbus topic for downstream subscribers. Effective-toolset recompute + runtime hot-reload signal are explicitly deferred to M9.1.b. Per-call resolver shape for source-auth secrets so per-tenant tokens never become a process-global static.
  - [ ] **M9.1.b** **Effective-toolset recompute + runtime hot-reload signal** — on each `source_synced` event the registry scans the synced directory, decodes every `manifest.json` via the M9.1.a loader, and builds the effective per-Watchkeeper toolset (precedence flattening only — full priority+shadow conflict resolution lives in M9.2); the new toolset is published to running runtimes via an update signal — in-flight `InvokeTool` calls complete on the old version while new invocations resolve against the new toolset, with a configurable grace period; runtime tests assert the in-flight-vs-new boundary holds under contention.
- [ ] **M9.2** **Resolver + shadow safety** — overlay merge by priority into the effective per-Watchkeeper toolset; same-name conflict resolved by priority with the lower-priority tool marked `tool_shadowed` and a Slack DM to the lead ("Platform now ships `count_open_prs` v1.2.0; your private repo's `count_open_prs` v0.4.1 takes precedence. Review?"); platform-release integration test simulates a platform `main` carrying a same-named tool to assert the private one still wins, the platform one is shadowed, and the warning event fires.
- [ ] **M9.3** **Signing + capability dictionary** — `cosign` or `minisign` over tool tarball + manifest with per-source public keys pinned in `config.yaml` (optional in Phase 1, mandatory in Phase 2 for non-local sources; unsigned load refused when enabled); `dict/capabilities.yaml` maps every capability id to a plain-language description (versioned, translation-ready, missing entry is a CI failure).
- [ ] **M9.4** **Authoring + approval + dry-run + repo CI** — `propose_tool(name, purpose, plain_language_description, code_draft, capabilities, target_source ∈ {platform, private})` with mandatory `plain_language_description` (local source never offered to the agent; lead can override `target_source` at approval time); per-deployment `approval_mode ∈ {git-pr, slack-native, both}`; `git-pr` runs the shared CI workflow on the tool repo (typecheck, undeclared-fs/net lint, vitest with coverage, capability-declaration linter, optional signing step on merge), human lead reviews the PR, merge fires a webhook → core re-syncs → hot-loads; `slack-native` (mandatory for `hosted` source) is Watchmaster-as-AI-reviewer running the same gate set in-process and posting an approval card with plain-language description, human-readable capability translations, per-gate pass/fail, heuristic risk level, and `[Approve] [Reject] [Test in my DM] [Ask questions]` buttons; tool manifest declares `dry_run_mode ∈ {ghost, scoped, none}` (ghost stubs writes and records "would have done: X, Y, Z"; scoped runs with broker-injected filters — Slack sends forced to lead's DM, Jira writes to sandbox project; none surfaces an explicit pre-approval warning); shared CI workflow template is published by the platform and consumed by `watchkeeper-tools` and customer-private repos.
- [ ] **M9.5** **Local patches + rollback** — `make tools-local-install <folder>` requires a `--reason` field and emits `local_patch_applied` with operator identity, diff hash, and reason; `wk tool rollback <name> --to <version> [--source <source>]` is operator-driven and logged.
- [ ] **M9.6** **Hosted ↔ git migration + tool sharing** — `wk tool hosted export <name>` produces a self-contained bundle (manifest + source + tests) that imports cleanly into a fresh git repo; `promote_share_tool` reads `$DATA_DIR/tools/private/<tool>/`, branches and commits in the target (platform `watchkeeper-tools` for broadly-useful tools, customer-owned git repo for customer-IP), opens a PR, and notifies the lead in Slack; auth via baked credentials — PAT (simpler, Phase 1 quickstart default) or GitHub App (scoped, revocable, production-recommended); capability `tool:share` off by default and granted per-Watchkeeper by the lead.
- [ ] **M9.7** **Keeper's Log audit surface** — emits `source_synced`, `source_failed`, `tool_proposed`, `tool_ai_review_passed`, `tool_ai_review_failed`, `tool_dry_run_executed`, `tool_approved`, `tool_rejected`, `tool_loaded`, `tool_shadowed`, `tool_retired`, `local_patch_applied`, `signature_verification_failed`, `hosted_tool_stored`, `hosted_tool_exported`, `tool_share_proposed`, `tool_share_pr_opened`, `tool_share_pr_merged`, `tool_share_pr_rejected`.
- [ ] **M9.8** **Out of scope for Phase 1** — prompt self-tuning (tool authoring only); multi-Watchkeeper tool sharing via Keeper-to-Keeper (Phase 2).

**Artifacts**: `toolregistry/` Go package (config, sync scheduler, resolver, signer-verifier, hosted-mode storage), `approval/` package (Slack-native flow + AI reviewer runner + dry-run executor), `dict/capabilities.yaml`, shared repo CI workflow template, local-patch Make target, authoring tool in TS, scaffold `watchkeeper-tools` platform repo, scaffold `watchkeeper-tools-private-example` customer-repo template.

**Verification**

- [ ] **Engineer-customer demo** (`git` mode + `git-pr` approval) — ask Coordinator to create `count_open_prs`; drafts → opens PR in private git repo → CI passes → human merges → hot-loads → Coordinator uses it in-session.
- [ ] **Non-engineer-customer demo** (`hosted` mode + `slack-native` approval) — Coordinator drafts `weekly_overdue_digest`; Watchmaster posts approval card; lead clicks `[Test in my DM]`, dry-run output appears; `[Approve]`; tool hot-loads; Coordinator uses it. Lead never sees raw code.
- [ ] **Dry-run modes** — ghost: "would post" payload without posting. Scoped: Slack sends redirect to lead's DM. None: explicit "no dry-run available" warning on card.
- [ ] **Overlay test** — seed a private tool (either mode) with the same name as a platform tool; private wins, `tool_shadowed` logged, lead receives Slack warning.
- [ ] **Update-safety test** — bump platform repo to a new `main` with a same-named tool; re-sync; private tool (both modes tested) still wins and runs unchanged.
- [ ] **Signature test** — with signing enabled, an unsigned tool dropped into the platform source is refused with `signature_verification_failed`.
- [ ] **Local-patch audit** — `make tools-local-install` without `--reason` fails; with reason, reason + operator identity lands in Keeper's Log.
- [ ] **Hosted → git migration** — `wk tool hosted export count_open_prs` produces a bundle; the bundle imports cleanly into a fresh git repo, CI passes, tool functionally identical.
- [ ] **Capability-dictionary completeness** — CI fails when a declared capability has no entry in `dict/capabilities.yaml`.
- [ ] **`promote_share_tool` demo** — agent with `tool:share` proposes share of a private tool; PR opens in target repo; lead reviews diff in PR + receives Slack notification; approval merges PR; tool appears in the appropriate upstream source on next sync.

**Dependencies**: M8.
**External prerequisite**: `watchkeeper-tools` platform git repo created and reachable. Customer-private git repo is **optional** (only needed for `git` mode demo).
**Magnitude**: 14–20 days.

---

### M10 — Observability, CLI, operator runbook [ ]

**Goal**: Phase 1 is operable by someone who did not build it.

**Scope**

- [ ] **M10.1** **Observability** — Prometheus metrics (per-Watchkeeper token spend, latency histograms, tool-invocation counts, event-bus queue depth, Slack/Jira rate-limit headroom) plus structured JSON logs with correlation IDs and configurable per-subsystem log levels.
- [ ] **M10.2** **`wk` CLI + Make targets** — `spawn`, `retire`, `list`, `logs <wk>`, `inspect <wk>`, `tail-keepers-log`, `tool list | rollback`, `tool hosted list | show | export`, `tool share <name> --target <repo>`, `tools sources list | status | sync`, `notebook show <wk> | forget <wk> <id> | export <wk> | import <wk> <archive> | archive <wk> | list-archives <wk>`, `personality show <wk> | set <wk>`, `language show <wk> | set <wk>`, `budget show | set`, `approvals pending | inspect <id>` — every command mirrored behind a `make wk CMD="..."` shortcut.
- [ ] **M10.3** **docker-compose + Grafana** — finalize compose stack (core, keep, postgres, watchmaster, sample coordinator, dev Slack socket bridge) and ship a Grafana starter dashboard.
- [ ] **M10.4** **Operator runbook + smoke** — runbook covers workspace bootstrap, credential rotation, Keep backup/restore, Notebook archive backup/restore via `ArchiveStore`, runaway-agent incident response, upgrade procedure, and disaster scenarios; `make smoke` reproduces the M7 + M8 + M9 success scenarios against an isolated dev environment.

**Artifacts**: metrics package, CLI binary, runbook markdown, smoke script, Grafana dashboard JSON.

**Verification**

- [ ] Smoke test green in CI against stubbed Slack + real Postgres.
- [ ] Runbook dry-run performed by a teammate who was not involved in building Phase 1.
- [ ] Every `wk` CLI command has a matching `make wk CMD="..."` shortcut that works.
- [ ] Backup/restore drill: wipe `$DATA_DIR`, restore from archives, Notebook state identical.

**Dependencies**: M9.
**Magnitude**: 4–6 days.

---

## 5. Risks (Phase 1-specific)

| Risk                                                                                       | Likelihood | Impact   | Mitigation                                                                                                                                                                                                                                                                                                                                            |
| ------------------------------------------------------------------------------------------ | ---------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Slack Manifest API rate limits or policy changes                                           | Med        | High     | Isolate to Go core; retry with backoff; fall back to manual-create flow if quotas hit.                                                                                                                                                                                                                                                                |
| Capability-broker bypass via harness bug                                                   | Low        | Critical | Defense in depth: OS-level seccomp on worker processes, resource limits enforced by core regardless of tool trust level.                                                                                                                                                                                                                              |
| LLM-authored tools pass lint and tests but misbehave semantically                          | Med        | Med      | Human code review remains mandatory gate; default capabilities set scoped minimally; rollback CLI one command away.                                                                                                                                                                                                                                   |
| Coordinator becomes a nuisance to real humans                                              | Med        | Med      | Dev workspace first; opt-in channel participation only; per-user per-day message rate limit.                                                                                                                                                                                                                                                          |
| The Keep grows large from chatty logging                                                   | Med        | Low      | Retention policy from day one; compact `keepers_log` to cold storage after N days; metric-based alert on table growth.                                                                                                                                                                                                                                |
| Self-modification loop becomes the main failure surface                                    | High       | Med      | Scope Phase 1 to tool authoring only; prompt self-tuning deferred; per-Watchkeeper tool-proposal quotas.                                                                                                                                                                                                                                              |
| Claude Code SDK / CLI changes mid-build                                                    | Med        | Med      | `LLMProvider` abstraction already isolates it; pin version; conformance test via `FakeProvider`; smoke test on upgrade.                                                                                                                                                                                                                               |
| Cost overrun during development on the model side                                          | Med        | Low      | Per-Watchkeeper daily budget enforced from M6 onward; alerting from M10.                                                                                                                                                                                                                                                                              |
| Platform update shadows/breaks customer-private tools                                      | Med        | High     | Overlay priority model: platform can never overwrite private; same-name shadow emits loud Keeper's Log event + Slack DM to lead; update-safety integration test gates every release.                                                                                                                                                                  |
| Customer-private tool repo becomes stale / unreachable at runtime                          | Med        | Med      | Sync failures emit `source_failed` events and alerts; resolver falls back to last-good cached tarball; CLI `wk tools sources status` surfaces state.                                                                                                                                                                                                  |
| Unsigned / tampered tool loaded into runtime                                               | Low        | Critical | Optional signing in Phase 1, mandatory in Phase 2 for non-local sources; capability declarations linted; local-patch source requires operator identity + reason field.                                                                                                                                                                                |
| Operator sidesteps audit via local patches                                                 | Med        | Med      | `local` source always emits `local_patch_applied` with mandatory reason; metric `local_patches_active` drives a dashboard alert when it crosses a threshold.                                                                                                                                                                                          |
| Non-engineer lead rubber-stamps proposed tools without understanding                       | High       | Med      | Watchmaster-as-AI-reviewer gates block before the Slack card is posted; plain-language capability translations mandatory; dry-run required by default (`dry_run_mode: none` shows an explicit warning); risk level surfaced prominently; post-approval digest to the lead summarizes what their Watchkeepers did with each new tool in the first 24h. |
| `hosted`-mode Keep data loss wipes customer tools                                          | Low        | Critical | Hosted tool source is included in the standard Keep backup path; `wk tool hosted export` works offline; restore procedure is part of the operator runbook DR drill.                                                                                                                                                                                   |
| Dry-run "ghost" mode produces misleading approvals (tool works in ghost but fails in prod) | Med        | Med      | Ghost-mode output explicitly marked "simulated"; `scoped` mode preferred where feasible; first real invocations after approval monitored at elevated verbosity for a configurable window.                                                                                                                                                             |
| Notebook SQLite file corruption or disk loss                                               | Low        | High     | Periodic backups to `ArchiveStore`; `notebook.Stats` exposes integrity check (SQLite `PRAGMA integrity_check`); retire saga always archives before tearing down; operator runbook documents restore from any archive URI.                                                                                                                             |
| Notebook promotion to Keep leaks private customer info into org-wide knowledge             | Med        | High     | `promote_to_keep` is Watchmaster-mediated and always goes through lead approval with full diff preview; approval card explicitly flags "this will become visible to all Watchkeepers in the org"; lead can reject or amend before promotion.                                                                                                          |
| `tool:share` capability misuse leaks customer IP via upstream PR                           | Med        | High     | Capability off by default; per-PR diff surfaced to lead for approval in all cases; PR-template banner "Review for customer IP before merge"; additional CI check on `watchkeeper-tools` repo scans for common secret patterns.                                                                                                                        |
| PAT / GitHub App credential compromise                                                     | Med        | High     | Short-lived PATs preferred; App installation scope minimal (one repo); rotation documented in runbook; Keeper's Log records every `tool_share_pr_opened` with actor for forensics.                                                                                                                                                                    |
| False self-learned lesson degrades agent behavior before lead notices                      | Med        | Med      | 24h cooling-off window on new lessons before auto-injection; daily Coordinator briefing surfaces pending lessons; CLI `wk notebook forget` is one command away; integration test: plant a malicious lesson, verify it does not influence prompts within the cooling-off window.                                                                       |

---

## 6. Cross-cutting Constraints

- Every Watchkeeper action recorded in Keeper's Log with a correlation ID tracing back to the originating human action or event.
- Per-Watchkeeper daily token budget enforced by Watchmaster; overage triggers pause and a Slack alert to the admin.
- Capability tokens TTL ≤ 5 minutes; no long-lived grants.
- Only the Watchmaster may invoke privileged core RPCs (Slack App creation, Manifest mutation), and only after an explicit human approval event is present in Keeper's Log.
- All tools are code; all code reaches the runtime via a configured source (git or local patch folder); there is no "paste-to-run" path.
- **Makefile is the only supported operator and dev entry point.** Any workflow that isn't a `make <target>` is considered not-yet-done. `make help` is the discovery surface.
- The platform never writes into customer-private or local tool sources. Updates only mutate built-in (on core upgrade) and platform (on its own release). Private layer is sovereign to the customer.
- LLM access goes through `LLMProvider` — core code contains no direct references to vendor-specific env vars or client libraries.
- Pre-commit lints and secret scans run locally; the same checks are blocking status checks in CI — no local-only or CI-only discrepancies.
- **Keep contains business knowledge of the organization the bots serve — nothing else.** No infrastructure metadata, no runtime state, no deployment identifiers, no per-environment flags. Multi-environment isolation (prod/staging/dev) is achieved by running separate Keep instances; the schema stays pure-domain. Agents' personal memory is in Notebook; tool source code is on filesystem; operational state is in core's own tables — never in Keep.
- Notebook is physically isolated per agent: one SQLite file, filesystem perms, co-located with the harness process. Cross-agent reads are blocked by the OS, not by policy.
- Every mutating Notebook operation emits an audit event to Keeper's Log via the Keep client; Notebook has no audit surface of its own.

---

## 7. Definition of Done (Phase 1)

- [ ] `docker-compose up` brings Phase 1 online with no manual steps beyond secret provisioning.
- [ ] Admin DMs the Watchmaster in the dev Slack workspace and receives a live list of Watchkeepers.
- [ ] Admin requests spawn of a Coordinator; Manifest draft appears for approval; on approval, Coordinator is live in Slack within 90 seconds and posts its intro.
- [ ] Coordinator processes a Watch Order, nudges a reviewer in Jira, and posts a daily briefing on cron.
- [ ] A Watchkeeper drafts a new tool; PR is opened; CI passes; human merges; tool hot-loads; Watchkeeper uses it in-session.
- [ ] All of the above are visible as correlated events in the Keeper's Log.
- [ ] `make smoke` passes in CI.
- [ ] Operator runbook walked through by a second engineer without assistance.

---

## 8. External Prerequisites (collect before or during M4 / M5 / M8 / M9)

- [ ] **Dev Slack workspace** provisioned, admin access, parent app with `app_configuration` scope. _(required before M4)_
- [ ] **Claude Code** installed on every host that runs the harness, with valid credentials (API key or subscription) made available through the core's secrets interface — no direct vendor env vars. _(required before M5)_
- [ ] **Jira test project** with API credentials and a couple of seeded tickets. _(required before M8)_
- [ ] **Platform Tool Registry repo** (`watchkeeper-tools`) created and reachable by the core; shared CI workflow template published. _(required before M9)_
- [ ] **Customer-private tool repo** (`git` mode only) configured — local bare git repo for dev, or a real private git host for production-like testing. _(optional: required only if the deployment runs in `git` private-mode)_
- [ ] **Signing keys** (if signing is enabled in Phase 1): platform public key pinned in release assets; customer-private key generated and its public counterpart pinned in operator config. _(optional in Phase 1; required in Phase 2)_
- [ ] **Per-deployment decision captured**: private mode (`git`, `hosted`, or both) and approval mode (`git-pr`, `slack-native`, or both). _(decided per onboarding)_
- [ ] **Git credentials for `promote_share_tool`** — PAT (bot account) OR GitHub App installation on target repo. PAT is the Phase 1 quickstart default; GitHub App is production-recommended. _(required before M9 if `tool:share` capability will be enabled)_
- [ ] **Archive store endpoint** — `LocalFS` works out of the box; production deployment should point `ArchiveStore` at a durable S3-compatible endpoint (AWS S3, Cloudflare R2, customer-owned SeaweedFS/Garage). _(required before production use; Phase 1 dev is fine on LocalFS)_

---

## 9. Out of Scope — deferred to Phase 2 and beyond

| Item                                                                                                                 | Target phase | Reason deferred                                            |
| -------------------------------------------------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------- |
| Keeper-to-Keeper communication                                                                                       | Phase 2      | Only one concrete role in Phase 1.                         |
| Admin dashboard UI                                                                                                   | Phase 2      | CLI + Grafana cover Phase 1 operator needs.                |
| Additional roles (Code Reviewer, Tech Writer, PM Assistant, Incident Responder, Onboarding Guide, Security Sentinel) | Phase 2      | Focus Phase 1 on the core loop.                            |
| Prompt self-tuning (self-modification beyond tool authoring)                                                         | Phase 2      | Blast radius control.                                      |
| Multi-model routing                                                                                                  | Phase 3      | Anthropic-only in Phase 1; architecture accepts providers. |
| Additional messengers (Teams, Telegram, Discord)                                                                     | Phase 2/3    | Adapter interface ready; only Slack implemented.           |
| SSO, RBAC, multi-tenancy                                                                                             | Phase 3      | Single org per instance in Phase 1.                        |
| Enterprise Grid automation, on-prem polish                                                                           | Phase 3      | Operator runbook sufficient for self-host.                 |
| Behavioral regression tests for prompt drift                                                                         | Phase 2      | Meaningful only with multiple roles.                       |
| Notebook auto-inheritance policy (same-role successor auto-seed)                                                     | Phase 2      | Phase 1 supports manual `wk notebook import` only.         |
| Reflection sampling on tool success; lesson consolidation                                                            | Phase 2      | Phase 1: reflect on errors only, no compaction.            |
| Personality preset catalog, emoji/humor axes, full tone DSL                                                          | Phase 2      | Phase 1: free-text `personality` + ISO `language` fields.  |
