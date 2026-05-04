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

- [ ] Client `log_append` → server persists → client `log_tail` returns same event.
- [ ] `UPDATE`/`DELETE` on `keepers_log` rejected by trigger.
- [ ] RLS: client authenticated as agent A cannot `search` rows scoped to agent B.
- [ ] Contract tests pass against a fresh Keep instance.

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

- [ ] `Remember` + `Recall` cycle returns correct top-K.
- [ ] Recall latency stays sub-millisecond at 10k entries (benchmark gated).
- [ ] `Archive` to `LocalFS` and to an `S3Compatible` endpoint (docker test container); `Import` from each restores identical state.
- [ ] Cross-agent isolation: process A cannot open process B's file (filesystem perms).
- [ ] Every mutation produces a correlated Keeper's Log entry.

**Dependencies**: M2.
**Magnitude**: 3–5 days.

---

### M3 — Go core services [ ]

**Goal**: In-process services every Watchkeeper relies on.

**Scope**

- [x] **M3.1** In-process event bus (pub/sub) with handler registration, ordered per-topic delivery, and backpressure.
- [x] **M3.2** Lifecycle manager: `Spawn`, `Retire`, `Health`, `List` for Watchkeeper processes; state persisted in `watchkeeper` table **via `keepclient`, not direct DB access**.
  - [x] **M3.2.a** keepclient watchkeeper resource CRUD (Insert/UpdateStatus/Get/List) — adds a thin client surface for the watchkeeper table mirroring the manifest/knowledge_chunk pattern.
  - [x] **M3.2.b** lifecycle: Spawn/Retire/Health/List manager over keepclient — consumes the new keepclient methods via a LocalKeepClient interface; logical lifecycle only (process supervision deferred to M5.3).
- [x] **M3.3** Cron scheduler (robfig/cron) that emits events onto the bus.
- [ ] **M3.4** Config loader (env + `config.yaml`); secrets pluggable interface (env-first for Phase 1, Vault-ready).
  - [x] **M3.4.a** Secrets pluggable interface: `core/pkg/secrets/` defining `SecretSource` interface, `EnvSource` implementation (env-first for Phase 1, Vault-ready abstraction); functional-options constructor; cross-package compile-time interface check.
  - [ ] **M3.4.b** Config loader: `core/pkg/config/` loading env vars + `config.yaml` via `gopkg.in/yaml.v3` into a strongly-typed Config struct with per-service sub-structs; validates required fields at Load time; resolves `*_secret` fields through the M3.4.a SecretSource interface (depends on M3.4.a).
- [ ] **M3.5** Capability broker: issues scoped, short-lived tokens to tools and to `keepclient` / `notebook` / `archivestore` calls; validates on invocation; enforces TTL.
- [ ] **M3.6** Keeper's Log writer (thin wrapper on `keepclient.log_append`): structured event schema, correlation IDs, trace context propagation.
- [ ] **M3.7** Outbox consumer: reads `outbox` from Keep via `keepclient.subscribe`, publishes to event bus with at-least-once semantics and idempotency keys.

**Artifacts**: Go packages `eventbus`, `lifecycle`, `cron`, `capability`, `keeperslog`, `outbox`.

**Verification**

- [ ] Integration test: spawn a mock Watchkeeper, fire a cron event, Watchkeeper receives it, Keeper's Log contains both the cron-fired and the handler-ran events with matching correlation IDs.
- [ ] Capability token expires exactly at TTL; use after expiry rejected.
- [ ] Outbox consumer is at-least-once and idempotent under forced redeliveries.

**Dependencies**: M2.
**Magnitude**: 5–8 days.

---

### M4 — Messenger adapter + Slack integration [ ]

**Goal**: A Watchkeeper can appear as a Slack bot, receive and send messages.

**Scope**

- [ ] **M4.1** `MessengerAdapter` Go interface: `SendMessage`, `Subscribe`, `CreateApp`, `InstallApp`, `SetBotProfile`, `LookupUser`.
- [ ] **M4.2** Slack implementation:
  - Slack Manifest API client (parent app holds `app_configuration` scope).
  - OAuth install flow (admin-preapproval path for dev workspace).
  - Bot profile setup via `users.profile.set`, `bots.info`.
  - Event intake via **Socket Mode** (no public HTTPS required for Phase 1).
  - Rate limiter aware of Slack tier-2/tier-3 budgets.
- [ ] **M4.3** Dev workspace bootstrap script (creates parent app from manifest, grants scopes, stores credentials via secrets interface).
- [ ] **M4.4** Human identity mapping: `human` row keyed by Slack user ID; lead → Watchkeeper relation modeled.

**Artifacts**: `messenger/` package, `messenger/slack/` adapter, bootstrap script, operator doc section "Provisioning the dev Slack workspace".

**Verification**

- [ ] `make spawn-dev-bot` creates a new Slack child app in the dev workspace, installs it, the bot appears as a workspace member and echoes a DM sent to it.
- [ ] Rate limiter honors tier-2 burst + sustained limits under load test.
- [ ] Parent-app credentials never leave the secrets interface (grep the built binary for raw tokens — none).

**Dependencies**: M3.
**External prerequisite**: dev Slack workspace provisioned.
**Magnitude**: 5–7 days.

---

### M5 — Runtime adapter + Claude Code bridge [ ]

**Goal**: Go core can launch and drive a TS agent harness that uses Claude Code under the hood; tools execute in isolation; the LLM provider is swappable without touching core.

**Scope**

- [ ] **M5.1** `AgentRuntime` Go interface: `Start`, `SendMessage`, `InvokeTool`, `Terminate`, plus streaming event hook.
- [ ] **M5.2** `LLMProvider` interface (separate from `AgentRuntime`): `Complete`, `Stream`, `CountTokens`, `ReportCost`. Default implementation wraps Claude Code (via Claude Agent SDK if embedding, or as a subprocess if shelling out).
- [ ] **M5.3** TS harness process:
  - Claude Code integration via the `LLMProvider` wrapper — model, system prompt, and context parameterized from Manifest.
  - JSON-RPC over stdio with Go core (request/response + streaming notifications).
  - Tool invocation path: request → capability check → execute in `isolated-vm` (pure-JS tools) OR in a worker process (I/O-capable tools with declared capabilities) → return result.
  - Tool schemas defined with `zod`, auto-derived from Tool Manifest.
- [ ] **M5.4** Per-tool resource limits: wall-clock, CPU time, memory ceiling, output-byte cap; enforced by Go core via process controls and isolate options.
- [ ] **M5.5** Manifest loader: harness calls `keepclient.GetManifest(agent_id)` on boot; Manifest fields include `system_prompt`, `personality`, `language`, toolset ACLs, model, autonomy. `personality` and `language` are composed into the effective system prompt via a templater.
- [ ] **M5.6** **Notebook linked into harness**: harness opens its per-agent SQLite file on boot; auto-recall top-K relevant entries each turn and injects them into the prompt (configurable K, relevance threshold). `Remember` is available as a built-in tool the agent can call explicitly.
- [ ] **M5.7** **Auto-reflection on tool error**: harness triggers a reflection step on tool error and writes the result as a `lesson` entry with `evidence_log_ref` and `tool_version`. New lessons get `active_after = now() + 24h` — visible but not auto-injected until the cooling-off window passes.
- [ ] **M5.8** **Tool-version awareness**: on tool hot-load, lessons tied to the superseded version are marked `needs_review` and excluded from auto-injection until reviewed. Not deleted.
- [ ] **M5.9** Provider credentials: Claude Code credentials via the secrets interface; no `ANTHROPIC_API_KEY` references in core code.
- [ ] **M5.10** Provider-swap conformance test: a dummy `FakeProvider` passes the same harness tests as the Claude Code provider.

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

### M6 — Watchmaster (meta-agent) [ ]

**Goal**: First concrete Watchkeeper — the orchestrator humans talk to.

**Scope**

- [ ] **M6.1** Watchmaster Manifest (system prompt, authority matrix).
- [ ] **M6.2** Toolset: `list_watchkeepers`, `propose_spawn` (drafts Manifest, including `personality` and `language` fields), `retire_watchkeeper`, `report_cost`, `report_health`, `adjust_personality(watchkeeper, new_personality)`, `adjust_language(watchkeeper, new_language)` (both draft a new Manifest version going through lead approval), `promote_to_keep(agent_id, notebook_entry_id)` (routes a Notebook entry for approval by lead; on approval writes it into Keep as org-scoped knowledge and emits `notebook_promoted_to_keep`).
- [ ] **M6.3** Cost tracker: prompt + completion tokens per Watchkeeper, rolled up to day / week, persisted in Keep.
- [ ] **M6.4** Slack conversational surface: Watchmaster responds to DMs from designated admins, renders Manifest drafts as Slack blocks with Approve / Reject actions.
- [ ] **M6.5** Privilege boundary: Watchmaster does **not** execute Slack App creation itself — it calls into a core-owned privileged RPC for that.

**Artifacts**: Watchmaster manifest file, toolset TS implementations, Slack interaction flow.

**Verification**

- [ ] Admin DMs "what's running?" → Watchmaster replies with a live list.
- [ ] Admin DMs "propose a Coordinator for the backend team" → Watchmaster posts a Manifest draft with Approve / Reject buttons.
- [ ] `adjust_personality` drafts a Manifest version bump that goes through lead approval.
- [ ] `promote_to_keep` surfaces a diff preview and requires explicit lead approval before the Keep write.

**Dependencies**: M4, M5.
**Magnitude**: 4–6 days.

---

### M7 — Spawn Flow end-to-end [ ]

**Goal**: The flow from `watchkeeper-spawn-flow.md` works front-to-back.

**Scope**

- [ ] **M7.1** Orchestration saga in Go core chaining: Manifest approval → Slack App create → OAuth install → bot profile set → **provision per-agent Notebook file** → runtime launch (with personality/language applied) → intro message.
- [ ] **M7.2** Retire flow: harness `Archive` is called; tarball goes to `ArchiveStore` (LocalFS or S3-compatible); `notebook_archived` event with archive URI in Keeper's Log; Watchkeeper row in Keep marked retired with archive reference.
- [ ] **M7.3** Saga compensations: on install failure, rollback Slack App creation, remove the freshly-provisioned Notebook file, mark Manifest as rejected; on runtime boot failure, tear down the app, archive (not delete) any Notebook data that was written, flag for review.
- [ ] **M7.4** Idempotency keys so retried approvals never double-create apps.
- [ ] **M7.5** Watchmaster invokes the saga via core RPC when a human approves a draft.
- [ ] **M7.6** Approval UX: draft Manifest posted in Slack with Approve / Reject blocks; button callback writes an approval event to Keeper's Log and triggers the saga.

**Artifacts**: `spawn/` saga package, Slack interaction handler for approval actions, saga-state DAO.

**Verification**

- [ ] Scripted end-to-end: admin DMs Watchmaster "spawn coordinator for backend team", approves the draft, new bot appears in the workspace within 90 seconds posting its intro message.
- [ ] Fault-injection: kill the runtime during step 5 → saga rolls back the Slack App and surfaces failure to the admin.
- [ ] Idempotency: same approval re-submitted does not create a second Slack app.
- [ ] Retire flow archives Notebook before tearing down runtime.

**Dependencies**: M6.
**Magnitude**: 4–6 days.

---

### M8 — Coordinator Watchkeeper + Jira adapter [ ]

**Goal**: A second role exists and performs real work.

**Scope**

- [ ] **M8.1** Jira adapter (REST via `go-jira` or direct HTTP): JQL search, read, comment, update a whitelisted set of fields.
- [ ] **M8.2** Coordinator Manifest: system prompt, authority matrix (comment + field-update allowed; no reassignment without lead approval).
- [ ] **M8.3** Toolset:
  - `fetch_watch_orders` — reads Slack DMs from lead.
  - `find_stale_prs`, `find_overdue_tickets`, `nudge_reviewer`, `post_daily_briefing`, `update_ticket_field`.
- [ ] **M8.4** Cron subscriptions: daily briefing at configurable time; morning overdue sweep.
- [ ] **M8.5** **Pending-lesson digest**: Coordinator's daily briefing includes a section listing lessons learned in the past 24h still in the cooling-off window. Lead can reply `forget <id>` to kill a lesson before it activates; otherwise they auto-activate on schedule.
- [ ] **M8.6** Watch Order parser: lead's natural-language priorities distilled into a persistent task list in the Keep; round-trip with the lead to confirm parsing.

**Artifacts**: `jira/` adapter package, Coordinator manifest, tool implementations, parser tests.

**Verification**

- [ ] Coordinator is spawned via the M7 flow.
- [ ] Reads a Watch Order from a Slack DM; parser round-trips confirmation with the lead.
- [ ] Finds an overdue ticket in the test Jira project; posts a nudge comment.
- [ ] Posts a daily briefing to the configured Slack channel; briefing includes pending-lesson digest when applicable.

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

- [ ] **M9.1** **Tool Source config** (`config.yaml`): list of sources in priority order, per-source auth, branch/tag, pull policy (`on-boot` | `cron <schedule>` | `on-demand`).
- [ ] **M9.2** **ToolSyncScheduler** in Go core: clones/pulls each source into `$DATA_DIR/tools/<source-name>/`; verifies signatures (if enabled); re-computes effective toolset on change. Sync results as Keeper's Log events (`source_synced`, `source_failed`).
- [ ] **M9.3** **Resolver**: merges sources by priority into an effective toolset per Watchkeeper Manifest. Same-name conflicts resolved by priority; lower-priority tool marked `tool_shadowed` in Keeper's Log with a Slack warning to the lead.
- [ ] **M9.4** **Hot-reload**: when the effective toolset changes, running runtimes receive an update signal; in-flight tool calls complete on the old version; new invocations use the new version. Grace period configurable.
- [ ] **M9.5** **Tool manifest** (per tool): `manifest.json` with `name`, `version`, `capabilities`, `schema` (zod-compatible), `source` (auto-filled on load), `signature` (optional in Phase 1, mandatory in Phase 2 for non-local sources).
- [ ] **M9.6** **Signing (optional, gated by flag)**: `cosign` or `minisign` over tool tarball + manifest; public keys per source pinned in `config.yaml`. Unsigned load refused when signing is enabled.
- [ ] **M9.7** **Authoring tool granted to Watchkeepers**:
  - `propose_tool(name, purpose, plain_language_description, code_draft, capabilities, target_source)` — `target_source ∈ {platform, private}` (local never offered to the agent). `plain_language_description` is mandatory — it is what non-engineer leads read.
  - In `git` mode: opens a PR on the Watchkeeper's behalf in the chosen repo.
  - In `hosted` mode: creates a draft record (status `proposed`) and triggers the Slack-native approval flow.
  - Lead can override `target_source` at approval time.
- [ ] **M9.8** **Approval paths** (selected per deployment via `approval_mode ∈ {git-pr, slack-native, both}`):
  - **`git-pr` path**: shared CI workflow runs on the tool repo — typecheck, lint with undeclared-fs/net rule, vitest with coverage, capability-declaration linter, optional signing step on merge. Human lead reviews PR. Merge fires a webhook → core re-syncs source → hot-loads.
  - **`slack-native` path** (mandatory for `hosted` source): Watchmaster-as-AI-reviewer runs the same gate set in-process; posts approval card to the lead in Slack with plain-language description, human-readable capability translations, AI reviewer pass/fail per gate, heuristic risk level, buttons `[Approve] [Reject] [Test in my DM] [Ask questions]`.
- [ ] **M9.9** **Dry-run infrastructure**:
  - Tool manifest declares `dry_run_mode ∈ {ghost, scoped, none}`.
  - `ghost` — capability broker stubs writes and records intents; approval card shows "would have done: X, Y, Z".
  - `scoped` — real execution with filters injected by the broker (Slack sends forced to lead's DM; Jira writes to sandbox project).
  - `none` — tool cannot dry-run; agent explains why in manifest; lead sees an explicit warning before approving.
- [ ] **M9.10** **Capability dictionary**: `dict/capabilities.yaml` mapping every capability id to a plain-language description; versioned, translation-ready. Missing entry is a CI failure.
- [ ] **M9.11** **Repo CI (`git` mode)**: shared workflow template published by the platform; consumed by `watchkeeper-tools` and private repos. Same gate set as the Slack-native AI reviewer.
- [ ] **M9.12** **Local patches**: `make tools-local-install <folder>` with mandatory `--reason` field; event `local_patch_applied` with operator identity, diff hash, reason.
- [ ] **M9.13** **Keeper's Log entries**: `source_synced`, `source_failed`, `tool_proposed`, `tool_ai_review_passed`, `tool_ai_review_failed`, `tool_dry_run_executed`, `tool_approved`, `tool_rejected`, `tool_loaded`, `tool_shadowed`, `tool_retired`, `local_patch_applied`, `signature_verification_failed`, `hosted_tool_stored`, `hosted_tool_exported`, `tool_share_proposed`, `tool_share_pr_opened`, `tool_share_pr_merged`, `tool_share_pr_rejected`.
- [ ] **M9.14** **Rollback**: `wk tool rollback <name> --to <version> [--source <source>]`; operation logged.
- [ ] **M9.15** **Migration path (`hosted` → `git`)**: `wk tool hosted export <name>` produces a self-contained bundle (manifest + source + tests) that can be committed to a git repo.
- [ ] **M9.16** **`promote_share_tool` flow (private → shared git source)**:
  - Operation: read folder from `$DATA_DIR/tools/private/<tool>/`, create branch in the target repo, commit, open PR, notify lead in Slack.
  - Target repo: platform `watchkeeper-tools` (broadly-useful tools) or customer-owned git repo (customer-IP tools).
  - Auth via baked credentials: **PAT** (simpler, default for Phase 1 quickstart) or **GitHub App** (scoped, revocable, production-recommended).
  - Capability `tool:share` **off by default**; lead explicitly grants it per Watchkeeper.
- [ ] **M9.17** **Shadow warning**: when a platform tool is shadowed by a private tool on sync, Slack DM to lead: "Platform now ships `count_open_prs` v1.2.0; your private repo's `count_open_prs` v0.4.1 takes precedence. Review?".
- [ ] **M9.18** **Update safety test**: platform release integration test simulates a platform `main` containing a tool with the same name as a private one; asserts the private one still wins, the platform one is shadowed, and the warning event fires.
- [ ] **M9.19** **Out of scope for Phase 1**: prompt self-tuning (tool authoring only); multi-Watchkeeper tool sharing via Keeper-to-Keeper (Phase 2).

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

- [ ] **M10.1** Prometheus metrics: per-Watchkeeper token spend, latency histograms, tool-invocation counts, event-bus queue depth, Slack / Jira rate-limit headroom.
- [ ] **M10.2** Structured JSON logs with correlation IDs; configurable log levels per subsystem.
- [ ] **M10.3** `wk` CLI: `spawn`, `retire`, `list`, `logs <wk>`, `inspect <wk>`, `tail-keepers-log`, `tool list | rollback`, `tool hosted list | show | export`, `tool share <name> --target <repo>`, `tools sources list | status | sync`, `notebook show <wk> | forget <wk> <id> | export <wk> | import <wk> <archive> | archive <wk> | list-archives <wk>`, `personality show <wk> | set <wk>`, `language show <wk> | set <wk>`, `budget show | set`, `approvals pending | inspect <id>`. All CLI commands mirrored behind `make wk CMD="..."` targets.
- [ ] **M10.4** docker-compose finalization: core, keep, postgres, watchmaster, sample coordinator, dev Slack socket bridge, Grafana with starter dashboard.
- [ ] **M10.5** Operator runbook: workspace bootstrap, credential rotation, backup / restore of Keep, backup / restore of Notebook archives via `ArchiveStore`, incident response (runaway agent), upgrade procedure, disaster scenarios.
- [ ] **M10.6** Smoke test script: `make smoke` reproduces the success scenarios from M7, M8, and M9 against an isolated dev environment.

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
