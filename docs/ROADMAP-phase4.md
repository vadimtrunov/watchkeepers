# Watchkeeper Phase 4 — Enterprise Hardening Implementation Roadmap

**Status**: Planning
**Created**: 2026-04-22
**Scope reference**: [watchkeeper-business-concept.md](./watchkeeper-business-concept.md), [ROADMAP-phase3.md](./ROADMAP-phase3.md)
**Next phase**: [ROADMAP-phase5.md](./ROADMAP-phase5.md)
**Deliverable type**: implementation roadmap — not executable code.

**Progress symbols**: `⬜` not started · `🟨` in progress · `✅` done · `🚫` blocked.

---

## 1. Executive Summary

Phase 3 shipped the platform's depth — multi-model routing, Manifest editor UI, GitLab + Telegram adapters, full self-modification, compliance basics. Phase 4 is the **deployability phase**: make the platform safe and acceptable to run inside regulated, large-org, airgapped, security-conscious customer environments — without adding new product features.

**Framing**: Phase 4 adds zero agent capabilities. It adds **trust primitives** — SSO, HA, encryption everywhere, airgap, HIPAA-friendly posture — plus the remaining messenger/VCS adapters needed for "customer happens to use X" cases (MS Teams, Discord, Bitbucket, Mattermost for airgap). If a Phase 4 milestone makes the product more capable rather than safer-to-run, it's in the wrong phase.

**What stays explicitly out of Phase 4**:

- Multi-tenancy — one instance remains one org.
- SaaS deployment — self-hosted only.
- RBAC — operators of an instance are trusted; SSO restricts WHO operates, not WHAT they can do.
- Predictive / proactive intelligence, cross-org learning, SDK — Phase 5 (Grand Party).
- Public marketplace — Phase 6.

**Success metric**: a regulated-industry customer (fintech / healthcare / gov-adjacent) can deploy and operate the platform per their compliance team's checklist without engineering intervention. Audit acceptance is the acceptance signal.

---

## 1.1 Status Dashboard

| #   | Milestone                                              | Status | Magnitude | Notes                                        |
| --- | ------------------------------------------------------ | ------ | --------- | -------------------------------------------- |
| M1  | SSO (SAML + OIDC) for operators                        | ⬜     | 5–7d      |                                              |
| M2  | Keep HA (replication + failover)                       | ⬜     | 7–10d     |                                              |
| M3  | Multi-host harness deployment + zero-downtime upgrades | ⬜     | 6–8d      |                                              |
| M4  | Encryption at rest (Keep + Notebook + ArchiveStore)    | ⬜     | 4–6d      |                                              |
| M5  | Encryption in transit (mTLS) + secrets backend         | ⬜     | 5–7d      | Vault / KMS mandatory                        |
| M6  | Airgap mode + offline install                          | ⬜     | 6–8d      | flag-gated; Slack remains non-airgap default |
| M7  | Mattermost adapter                                     | ⬜     | 4–6d      | airgap-compatible messenger                  |
| M8  | MS Teams adapter                                       | ⬜     | 5–7d      |                                              |
| M9  | Discord adapter                                        | ⬜     | 3–5d      |                                              |
| M10 | Bitbucket adapter                                      | ⬜     | 4–6d      |                                              |
| M11 | HIPAA-friendly posture + data classification           | ⬜     | 6–8d      | controls + docs, no certification            |
| M12 | Phase 4 integration demo + compliance evidence         | ⬜     | 5–7d      | acceptance gate                              |

Total: ~60–85 days for one team. 12 milestones.

---

## 2. Key Architectural Decisions (additions to Phase 1 + 2 + 3)

| Concern                | Decision                                                                                                                                                                                                                                                                                            | Rationale                                                                                                                                     |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| Operator identity      | **SSO via SAML or OIDC** — pluggable per deployment; connects to customer's corporate IdP (Okta, Azure AD, Google Workspace, ADFS). Local accounts fallback for bootstrap only.                                                                                                                     | Central source of truth for human identity; joiner/leaver automation via the customer's existing IAM; compliance requirement.                 |
| Access model           | **Still no RBAC.** SSO restricts WHO can log in; everyone who passes SSO has full operator access inside their instance. One instance = one org; operators inside that org are trusted.                                                                                                             | Aligns with Phase 1/2/3 decisions; adding RBAC without multi-tenancy is YAGNI.                                                                |
| Keep durability        | **Postgres streaming replication** with Patroni for failover. Read-replica-aware `keepclient` with failover-on-error semantics. `keepers_log` append-only invariant fits replication naturally.                                                                                                     | Primary safety concern in Phase 4: data loss / downtime during outage.                                                                        |
| Harness distribution   | Harness processes can run on **any host in a configured pool**; lifecycle manager coordinates placement; zero-downtime rolling upgrades.                                                                                                                                                            | Removes the "Phase 1 single host" implicit assumption; enables real production ops.                                                           |
| Encryption at rest     | Postgres via filesystem LUKS (or native TDE where available); Notebook SQLite via **SQLCipher**; ArchiveStore via the endpoint's server-side encryption.                                                                                                                                            | Coverage of every persistent data store; customer-controlled keys where possible.                                                             |
| Encryption in transit  | **mTLS between every internal component** (core ↔ Keep, harness ↔ Keep, harness ↔ core, editor UI ↔ core). Public-facing surfaces use TLS 1.3. Cert management via `cert-manager` or equivalent.                                                                                                    | Defense-in-depth; compliance baseline for enterprise.                                                                                         |
| Secrets backend        | **Vault or cloud-KMS mandatory** for production deployments. Env-first path deprecated (still supported for dev).                                                                                                                                                                                   | Eliminates secret sprawl; satisfies most compliance frameworks out of the box.                                                                |
| Airgap                 | **Opt-in flag** in core config. When enabled, core refuses any HTTP call to non-whitelist destinations at runtime (verified on boot + every request); self-hosted LLM (Gemma via vLLM/Ollama from Phase 3 M3) becomes the default `LLMProvider`; Mattermost becomes the default `MessengerAdapter`. | Covers the "can't reach the internet" regulated/gov-adjacent customer; explicit flag avoids accidental offline mode for normal deployments.   |
| Airgap install         | Offline package: docker image tarballs + offline Go/npm/pip mirrors + documented install procedure.                                                                                                                                                                                                 | Airgapped hosts can't `apt install` or pull from Docker Hub; we ship a package they can transfer on media.                                    |
| Compliance target      | **HIPAA-friendly posture, not HIPAA certification.** Controls implemented (access logs, data classification, break-glass, retention, BAA-ready audit surfaces); customer handles certification + BAA paperwork.                                                                                     | Pragmatic — we give customers evidence; they close the compliance loop. SOC2 Type II certification would be our own corp paperwork, not code. |
| Deployment tenancy     | **Still one instance = one org.** Multi-tenancy → Phase 6 with SaaS.                                                                                                                                                                                                                                | Phase 4 scope is deployability at single-org scale, not scale-out.                                                                            |
| Phase scope discipline | **Phase 4 adds no agent capabilities.** Every milestone is about trust / safety / deployability. Capability additions (richer self-mod, SDK, etc.) belong to Phase 5 / 6.                                                                                                                           | Keeps the phase focused and acceptance-testable.                                                                                              |

---

## 3. Scope

### In

- SSO for operator access (SAML + OIDC), pluggable IdP; SSO identity threaded through Keeper's Log actor field for audit trails.
- Keep HA: Postgres streaming replication, Patroni-style failover, read-replica support in `keepclient`, verified RPO ≤ 5s / RTO ≤ 60s in drill.
- Multi-host harness deployment: harness pool discovery, placement policy, health-based migration, zero-downtime rolling upgrade of both core and harness.
- Encryption at rest for Keep Postgres (filesystem LUKS or TDE), Notebook SQLite (SQLCipher), ArchiveStore endpoint SSE. Customer-managed keys where the endpoint supports it.
- Encryption in transit: mTLS between all internal services; TLS 1.3 for external surfaces; cert automation via `cert-manager` or operator-managed cert rotation.
- Secrets backend integration: Vault, AWS KMS, GCP KMS, Azure Key Vault — pluggable; `secrets/` interface from Phase 1 gets real implementations.
- Airgap mode (opt-in flag): runtime enforcement of zero external HTTP; self-hosted LLM default; Mattermost default messenger; offline install package with offline dep mirrors.
- Mattermost adapter — airgap-compatible messenger with same shape as Slack adapter.
- MS Teams adapter — enterprise messenger.
- Discord adapter — additional messenger (smaller enterprise demand but Phase 4 catches up on the deferral).
- Bitbucket adapter — additional VCS + code-review tool suite.
- HIPAA-friendly posture: data-classification tags on Keep rows + Notebook entries; PHI-aware access logs with justification fields; break-glass procedure; retention enforcement; evidence-collection runbook.
- Phase 4 integration demo in a simulated regulated-customer environment + compliance evidence bundle.

### Out (Phase 5+ or rejected)

- Predictive / proactive intelligence (Phase 5).
- Cross-org learning / anonymized pattern sharing (Phase 5).
- Watchkeeper specialization via extended self-modification (Phase 5).
- API + SDK for third-party integrations + custom Watchkeepers (Phase 5).
- Public cross-org role marketplace (Phase 6).
- SaaS multi-tenant deployment, billing, per-tenant isolation (Phase 6).
- Multi-tenancy in a single instance (Phase 6).
- HIPAA certification (customer responsibility; we ship controls, not certification).
- RBAC (rejected — Phase 4 does not motivate it; would reconsider in Phase 6 under multi-tenancy).
- Admin dashboard UI (rejected — consistent with Phase 2/3).

---

## 4. Milestones

### M1 — SSO (SAML + OIDC) for operators [ ]

**Goal**: Operator identity sourced from the customer's corporate IdP. Joiner/leaver lifecycle handled automatically.

**Scope**

- [ ] **M1.1** SAML 2.0 SP implementation with standard metadata exchange; tested against Okta, Azure AD, Google Workspace, ADFS.
- [ ] **M1.2** OIDC client implementation for IdPs exposing OIDC (Auth0, Keycloak, Okta OIDC, Azure AD v2).
- [ ] **M1.3** Operator session: SSO-issued identity minted into a short-lived capability token consumed by all Phase 1+ interfaces (CLI, Manifest editor UI, HTTP APIs).
- [ ] **M1.4** Keeper's Log `actor` field now records SSO identity (email + IdP group claims) rather than local operator name where SSO enabled.
- [ ] **M1.5** Local account fallback: kept for bootstrap (first operator before SSO is configured) and for break-glass; disabled automatically once an SSO session has been established. Break-glass local login is a loud audit event.
- [ ] **M1.6** SSO attribute mapping: which IdP group maps to "operator"; no finer-grained RBAC (per Phase 4 decision).
- [ ] **M1.7** Runbook: SSO bootstrap procedure, metadata refresh, IdP outage response.

**Artifacts**: `auth/saml/` + `auth/oidc/` Go packages, CLI + editor UI integration, runbook section.

**Verification**

- [ ] SSO login flow against Okta / Azure AD / Keycloak (at least two) completes; capability token issued; CLI call succeeds under that session.
- [ ] Joiner test: add a user to the operator IdP group; user can log in. Leaver test: remove the user; existing sessions expire within TTL; new logins refused.
- [ ] Break-glass local login generates a Keeper's Log entry with `severity=high`.
- [ ] Group-attribute mismatch (user not in the operator group): login refused with clear error.

**Dependencies**: Phase 1 M10 (CLI + auth plumbing), Phase 3 M4 (editor UI auth).
**External prerequisite**: customer SAML or OIDC IdP with metadata URL + operator group configured.
**Magnitude**: 5–7 days.

---

### M2 — Keep HA (replication + failover) [ ]

**Goal**: Keep survives primary DB failure with RPO ≤ 5 seconds and RTO ≤ 60 seconds.

**Scope**

- [ ] **M2.1** Postgres streaming replication: primary + at least one synchronous replica + one asynchronous replica; config templates for both streaming and logical modes.
- [ ] **M2.2** Patroni (or equivalent) for automated failover; etcd/consul cluster for coordination.
- [ ] **M2.3** `keepclient` changes: connection string accepts a service name that resolves to current primary; on write-failure, retry against newly-elected primary with idempotency-safe semantics.
- [ ] **M2.4** Read-replica routing: read-only operations (e.g., `keep.search`, `keep.log_tail`) can be directed to replicas via a client-side preference flag. Writes always go to primary.
- [ ] **M2.5** Append-only invariant on `keepers_log` verified to survive replication + failover (trigger replicates; constraint holds on replicas).
- [ ] **M2.6** Backup chain updated to be replication-aware (base backup + WAL archive), compatible with Phase 2 M9 cron scheduler.
- [ ] **M2.7** HA drill: `make keep-failover-drill` kills the primary, observes election, confirms traffic continues.

**Artifacts**: Patroni config, docker-compose / k8s templates for HA topology, `keepclient` failover extensions, drill script, runbook.

**Verification**

- [ ] Failover drill: kill primary while a Watchkeeper is active; harness retries, no lost Keeper's Log entries, RPO ≤ 5s, RTO ≤ 60s measured.
- [ ] Read-replica routing: `keep.search` measurably off-loads from primary; can be disabled per-query when strong read-after-write consistency is needed.
- [ ] Append-only trigger holds on replicas during and after failover.
- [ ] Base + WAL backup restorable to a fresh cluster; integrity matches primary.

**Dependencies**: Phase 1 M2 (Keep service), Phase 2 M9 (backup automation).
**Magnitude**: 7–10 days.

---

### M3 — Multi-host harness deployment + zero-downtime upgrades [ ]

**Goal**: Harnesses run across a pool of hosts; core-binary and harness-binary upgrades complete with zero user-visible downtime.

**Scope**

- [ ] **M3.1** Harness host pool: config declares a set of hosts; lifecycle manager picks placement based on simple policy (load-aware by default; pinnable per Watchkeeper via Manifest `host_affinity` hint).
- [ ] **M3.2** Health-based migration: harness crashloop on host X triggers re-spawn on a healthy host; Notebook file either travels (via ArchiveStore round-trip) or the new instance is seeded with a fresh Notebook + import of latest archive.
- [ ] **M3.3** Zero-downtime upgrade for **core**: blue/green deployment — new core instance spun up, passes health checks, traffic swung over via service discovery, old instance drained.
- [ ] **M3.4** Zero-downtime upgrade for **harnesses**: rolling replacement — one Watchkeeper at a time, `SIGTERM` + grace period allows in-flight turn to finish, new harness binary picks up identical Manifest, confirmed healthy, next one.
- [ ] **M3.5** Upgrade verification: post-upgrade smoke must pass before the next node is touched; rollback on smoke failure.
- [ ] **M3.6** Graceful shutdown signal handling everywhere; in-flight saga state persisted so an interrupted spawn flow can resume post-restart.

**Artifacts**: lifecycle manager host-pool extensions, upgrade script, blue/green config, graceful-shutdown hardening across core + harness.

**Verification**

- [ ] Host-pool test: harness on host A crashes; lifecycle manager respawns on host B within SLA; user DM traffic to the bot continues with brief queue delay.
- [ ] Core zero-downtime upgrade: `make upgrade-core` swaps versions while a Watchkeeper is in an active turn; turn completes successfully; no lost messages.
- [ ] Harness zero-downtime upgrade: same, but for harness binary.
- [ ] Upgrade rollback on smoke failure: force smoke to fail after first node upgrade; remaining nodes roll back; overall state returns to previous version.

**Dependencies**: M2 (Keep HA — core can't upgrade if DB is single-point-of-failure).
**Magnitude**: 6–8 days.

---

### M4 — Encryption at rest (Keep + Notebook + ArchiveStore) [ ]

**Goal**: Every persistent data store encrypted at rest with customer-controllable keys where possible.

**Scope**

- [ ] **M4.1** Keep Postgres: filesystem-layer encryption via LUKS for self-hosted Postgres; or Postgres TDE extension (`pgcrypto` for column-level PHI) on top.
- [ ] **M4.2** Notebook SQLite: migrate from plain SQLite to **SQLCipher** (compatible API, encrypted file); key derived from secrets backend per-agent.
- [ ] **M4.3** ArchiveStore encryption: for `LocalFS`, filesystem LUKS; for `S3Compatible`, SSE-S3 / SSE-KMS configured per endpoint.
- [ ] **M4.4** Key management integration: keys live in the secrets backend from M5 — not on disk.
- [ ] **M4.5** Rotation procedure: key rotation documented and drill-tested; in-flight Watchkeepers smoothly transition.
- [ ] **M4.6** Column-level encryption for explicit PHI/PII fields in Keep (optional per deployment; activated by data-classification tags from M11).

**Artifacts**: LUKS provisioning scripts (or TDE setup), SQLCipher integration in Notebook library, ArchiveStore encryption config, rotation runbook.

**Verification**

- [ ] Stolen-disk test: pull a raw disk image from a running deployment; none of Keep DB, Notebook SQLite, or ArchiveStore tarballs readable without keys.
- [ ] Key rotation drill: rotate per-agent Notebook keys; agents continue operating through the rotation; old-key encrypted files re-encrypted with new key.
- [ ] Column-level encryption: tag a row with `classification=phi`; raw SELECT from Postgres returns encrypted bytes; Keep API surfaces decrypted content to authorized callers.

**Dependencies**: M5 (secrets backend).
**Magnitude**: 4–6 days.

---

### M5 — Encryption in transit (mTLS) + secrets backend [ ]

**Goal**: All internal communication mTLS-protected; secrets live in a proper backend, not env vars.

**Scope**

- [ ] **M5.1** Cert issuance: `cert-manager` for k8s deployments; operator-managed certs for docker-compose (bootstrap script generates per-component certs via a local CA).
- [ ] **M5.2** mTLS between: core ↔ Keep, harness ↔ Keep, harness ↔ core (JSON-RPC over stdio within the same host stays unencrypted since it's same-process; cross-host harness gets mTLS).
- [ ] **M5.3** Editor UI ↔ core: TLS 1.3 with optional client cert for operator authentication when SSO is unavailable (break-glass).
- [ ] **M5.4** Secrets backend pluggable interface gets real implementations: **Vault** (recommended), **AWS KMS**, **GCP KMS**, **Azure Key Vault**, **HashiCorp Boundary** (for short-lived creds).
- [ ] **M5.5** All Phase 1+ secret consumers migrated from env-first path to backend-first: Claude Code credentials, Slack tokens, GitHub/GitLab/Jira/Confluence tokens, database passwords, signing keys, encryption keys.
- [ ] **M5.6** Env-first path still works for dev (documented); disabled automatically in production when `environment=production` config is set.
- [ ] **M5.7** Rotation automation: secrets backend rotates where supported; core refreshes via short-lived lease pattern.

**Artifacts**: `secrets/` Go packages for each backend, cert-generation script, mTLS config templates, migration docs.

**Verification**

- [ ] Man-in-the-middle attempt between harness and Keep on another host: connection fails with TLS verification error.
- [ ] Secrets backend swap: start deployment with Vault, rotate to AWS KMS via config change + migration script, no downtime.
- [ ] Env-vars-in-prod check: deployment with `environment=production` and ANY `WK_SECRET_*` env var refuses to boot.
- [ ] Short-lived lease: Claude Code token lease expires mid-session, refreshed transparently.

**Dependencies**: Phase 1 M3 (secrets interface).
**Magnitude**: 5–7 days.

---

### M6 — Airgap mode + offline install [ ]

**Goal**: Platform can be deployed on a host with zero internet connectivity; all required capabilities work in-cluster.

**Scope**

- [ ] **M6.1** `airgap: true` flag in core config. When set:
  - Core refuses any outbound HTTP to non-whitelist destinations; whitelist explicitly empty by default (customer adds on-prem services: their GitLab, their Jira, their Mattermost).
  - Runtime verification at boot: resolves every configured integration URL; refuses to start if any resolves to a public IP outside whitelist.
  - Self-hosted LLM (Gemma via vLLM/Ollama from Phase 3 M3) becomes the default `LLMProvider`.
  - Mattermost (from M7) becomes the default `MessengerAdapter`.
  - Claude Code / Codex providers disabled automatically.
- [ ] **M6.2** Offline install package: docker image tarballs (`wk-core`, `wk-keep`, `wk-editor`, `wk-gemma-backend`, `wk-mattermost`, `wk-postgres`) + Go/npm/pip offline mirrors + install script that works from a USB drive on an isolated host.
- [ ] **M6.3** Dependency update procedure: documented process for importing a new release into an airgapped environment (platform ships a signed tarball; customer verifies signature + runs an import script).
- [ ] **M6.4** Airgap smoke test: `make airgap-smoke` launches a network-namespaced test environment with no internet; full Phase 1 success scenario passes.

**Artifacts**: airgap-mode config gating, boot-time verification, offline install package build script, airgap smoke test, runbook chapter on airgap deployment.

**Verification**

- [ ] Airgap boot: set `airgap=true`, configure only internal hostnames; platform starts healthy.
- [ ] External HTTP attempt: core sends a request to `example.com`; refused at runtime with `airgap_violation` Keeper's Log event.
- [ ] Offline install drill: on a host with firewall blocking all external traffic, install from the offline package and reach Phase 1 success scenario.
- [ ] Update drill: import a newer release tarball; smoke test passes on the upgraded airgapped environment.

**Dependencies**: M7 (Mattermost), Phase 3 M3 (Gemma), Phase 4 M5 (secrets — airgap likely uses Vault in-cluster).
**Magnitude**: 6–8 days.

---

### M7 — Mattermost adapter [ ]

**Goal**: Mattermost as a `MessengerAdapter` implementation — the self-hosted, airgap-compatible default messenger alternative to Slack.

**Scope**

- [ ] **M7.1** `messenger/mattermost/` Go adapter using Mattermost REST API + WebSocket for events.
- [ ] **M7.2** `MessengerAdapter` methods: `SendMessage`, `Subscribe`, `CreateApp` (Mattermost bots are created via admin API — simpler than Slack Manifest API), `InstallApp`, `SetBotProfile`, `LookupUser`.
- [ ] **M7.3** Channel model parity: public channels, private channels (equivalent to Slack private channels), DMs, threading.
- [ ] **M7.4** K2K over Mattermost: private channel per conversation, same pattern as Slack.
- [ ] **M7.5** Bot token management via secrets backend.
- [ ] **M7.6** Runbook: Mattermost workspace bootstrap, admin token provisioning, rate-limit guidance.

**Artifacts**: `messenger/mattermost/` adapter, runbook section, docker-compose service for self-hosted Mattermost (used in airgap deployments).

**Verification**

- [ ] Watchkeeper spawned in Mattermost: operator issues `/spawn coordinator` to Watchmaster bot; new Watchkeeper bot appears, responds to DMs.
- [ ] K2K test: two bots in a private channel exchange messages; escalation to lead's DM triggers on budget breach.
- [ ] Feature parity vs Slack: every Phase 1 M4 verification criterion passes under Mattermost with equivalent outcome.

**Dependencies**: Phase 1 M4 (messenger interface).
**External prerequisite**: Mattermost instance (self-hosted or Mattermost Cloud) with admin API access.
**Magnitude**: 4–6 days.

---

### M8 — MS Teams adapter [ ]

**Goal**: MS Teams as a `MessengerAdapter` for enterprise customers standardized on Microsoft.

**Scope**

- [ ] **M8.1** `messenger/teams/` Go adapter using Microsoft Bot Framework + Graph API.
- [ ] **M8.2** `MessengerAdapter` methods: standard set; Teams has more bureaucratic bot-creation flow (Azure AD app registration + Bot Channel Registration + app manifest upload). Runbook walks through the one-time setup; adapter picks up from a registered app onwards.
- [ ] **M8.3** Teams conversation model: 1:1 chats, group chats, channels in Teams (different from Slack channels), threaded messages.
- [ ] **M8.4** K2K over Teams: private Teams group chat per conversation; escalation via lead's 1:1 DM.
- [ ] **M8.5** Bot credentials via secrets backend; token lifecycle handled.
- [ ] **M8.6** Runbook: Azure AD app registration walkthrough, permission consent, rate-limit awareness.

**Artifacts**: `messenger/teams/` adapter, runbook chapter, credential-setup script.

**Verification**

- [ ] Watchkeeper spawned in Teams: bot registered, approved, user can DM it; replies flow correctly.
- [ ] K2K test: private group chat between two bots; escalation reaches the lead's 1:1.
- [ ] Channel interaction: bot can be @-mentioned in a Teams channel and respond.

**Dependencies**: Phase 1 M4.
**External prerequisite**: Azure AD tenant with bot creation privileges; registered Teams app; at least one Teams workspace.
**Magnitude**: 5–7 days.

---

### M9 — Discord adapter [ ]

**Goal**: Discord as a `MessengerAdapter`, covering gaming / community / dev-heavy customers who run on Discord.

**Scope**

- [ ] **M9.1** `messenger/discord/` Go adapter via `discordgo` or similar; uses the Gateway + REST APIs.
- [ ] **M9.2** Standard `MessengerAdapter` methods; Discord's model: servers ("guilds"), channels (text/voice/thread), DMs. Voice is out of scope.
- [ ] **M9.3** Slash command registration per bot (Discord's primary interaction pattern).
- [ ] **M9.4** Bot token management via secrets backend.
- [ ] **M9.5** Runbook: Discord developer portal walkthrough, bot creation, server invite link generation.

**Artifacts**: `messenger/discord/` adapter, runbook chapter.

**Verification**

- [ ] Watchkeeper in Discord: bot invited to server, responds to DMs and slash commands.
- [ ] K2K test: bot-to-bot private thread; escalation to lead's DM.
- [ ] Rate-limit handling: Discord's per-route limits honored without user-visible failure.

**Dependencies**: Phase 1 M4.
**External prerequisite**: Discord developer portal access; at least one test Discord server.
**Magnitude**: 3–5 days.

---

### M10 — Bitbucket adapter + code-review tool suite [ ]

**Goal**: Bitbucket as a VCS integration, symmetric with Phase 2 M4 (GitHub) and Phase 3 M6 (GitLab).

**Scope**

- [ ] **M10.1** `bitbucket/` Go adapter supporting Bitbucket Cloud and Bitbucket Data Center (self-hosted). Base URL configurable.
- [ ] **M10.2** Tool suite:
  - `bitbucket.list_open_prs(repo, filters)`
  - `bitbucket.fetch_pr(repo, pr_id)`
  - `bitbucket.post_pr_comment`, `bitbucket.post_diff_comment`
  - `bitbucket.request_changes`, `bitbucket.approve_pr`
  - `bitbucket.fetch_pipeline_status`
- [ ] **M10.3** Capabilities: `bitbucket:read`, `bitbucket:write`, `bitbucket:review`.
- [ ] **M10.4** Webhook intake for Bitbucket events (`bitbucket.pr_opened`, etc.).
- [ ] **M10.5** Manifest template `code-reviewer-bitbucket.yaml`.

**Artifacts**: `bitbucket/` adapter, TS tools in platform registry, capability dictionary entries, template, webhook receiver.

**Verification**

- [ ] Manifest using Bitbucket tools spawned; agent reviews a real PR in the test Bitbucket repo.
- [ ] Capability enforcement and self-hosted `base_url` work the same as GitLab M6.
- [ ] Webhook delivery confirmed.

**Dependencies**: Phase 1 M9 (Tool Registry).
**External prerequisite**: Bitbucket workspace (Cloud or Data Center) + access token; sample PR seeded.
**Magnitude**: 4–6 days.

---

### M11 — HIPAA-friendly posture + data classification [ ]

**Goal**: Controls and documentation sufficient for a customer to claim HIPAA readiness without us claiming certification. SOC2 audit-forwarding from Phase 3 M11 is already in place; M11 adds HIPAA-specific controls.

**Scope**

- [ ] **M11.1** **Data classification tags**: `keep_row.classification ∈ {public, internal, confidential, phi, pii}`; `notebook_entry.classification` same. Policy: `phi`/`pii` rows subject to column-level encryption (M4) + stricter audit.
- [ ] **M11.2** **PHI-aware access logging**: every access to a `phi`/`pii` row/entry requires a `justification` field (free-text supplied by the operator CLI or embedded by the agent's tool invocation). Access logs retained per HIPAA minimum retention (6 years).
- [ ] **M11.3** **Break-glass procedure**: emergency access that bypasses normal approval flows (e.g., lead unreachable during incident). Requires explicit `--break-glass <reason>` flag; emits high-severity Keeper's Log event + immediate notification to all configured contacts + automatic post-incident review ticket.
- [ ] **M11.4** **Retention enforcement for HIPAA**: log retention configurable per event type; default minimums aligned with HIPAA; automated purge of expired low-classification data.
- [ ] **M11.5** **Data subject controls**: GDPR APIs from Phase 3 M11 extended with HIPAA-specific data-type filters.
- [ ] **M11.6** **Evidence bundle**: `wk compliance evidence-bundle --framework hipaa` produces a structured bundle for an auditor (policies applied, sample audit logs, data-classification inventory, access-log samples, retention proofs).
- [ ] **M11.7** **Customer documentation**: HIPAA policies-and-procedures template (customer fills in specifics), shared responsibility matrix (what the platform does vs what the customer does for certification), sample BAA language.

**Artifacts**: classification schema migration, access-log justification field, break-glass CLI, evidence-bundle generator, HIPAA documentation package.

**Verification**

- [ ] Classification tag on a Keep row triggers column-level encryption on next write.
- [ ] Access to a PHI-classified row without justification is refused; with justification, access is logged with the justification attached.
- [ ] Break-glass session: `wk --break-glass "incident-001" ...` works, emits high-severity event, notifies admin list, creates post-incident review ticket in Jira (or equivalent).
- [ ] Evidence bundle generated and sampled by a mock auditor (teammate following a HIPAA audit checklist); bundle has all requested items.
- [ ] Retention: seed records older than policy; automated purge removes them; audit trail of the purge retained.

**Dependencies**: M4, M5, Phase 3 M11.
**External prerequisite**: a customer with a HIPAA audit checklist (or a rehearsal based on the HIPAA Security Rule).
**Magnitude**: 6–8 days.

---

### M12 — Phase 4 integration demo + compliance evidence [ ]

**Goal**: Acceptance gate. Demonstrate Phase 4 end-to-end in a simulated regulated-customer environment, with compliance evidence collected.

**Scope**

- [ ] **M12.1** Demo deployment in a regulated-scenario stand-in:
  - SSO-integrated operator login (via a test IdP).
  - Keep running in HA with replicas + failover drill executed in the demo.
  - Harnesses spread across 2+ hosts with a zero-downtime upgrade executed in the demo.
  - Encryption at rest + in transit verified.
  - Secrets in Vault (not env vars).
  - One Watchkeeper each on Slack, Mattermost, MS Teams, Discord.
  - GitHub + GitLab + Bitbucket adapters all spawned with Watchkeepers.
  - Airgap mode exercised in a separate stand (sub-demo): isolated host, offline install, full Phase 1 success scenario reproduced without internet.
  - Data classification + PHI access + break-glass exercised.
- [ ] **M12.2** Compliance evidence bundle generated and reviewed by a mock auditor (teammate).
- [ ] **M12.3** `make phase4-smoke` and `make phase4-airgap-smoke` both pass in CI.
- [ ] **M12.4** Phase 4 runbook appendix: SSO bootstrap, HA operations, encryption key rotation, airgap install, secrets rotation, HIPAA evidence collection, each adapter's workspace setup.

**Artifacts**: demo scripts, mock-audit scenario, smoke tests, runbook additions.

**Verification**

- [ ] Full demo runs without manual intervention.
- [ ] Airgap sub-demo runs on an isolated host.
- [ ] Mock auditor pulls evidence bundle; all requested artifacts present.
- [ ] Second engineer reproduces the demo from the runbook without assistance.

**Dependencies**: M1–M11.
**Magnitude**: 5–7 days.

---

## 5. Risks (Phase 4-specific)

| Risk                                                                                       | Likelihood | Impact   | Mitigation                                                                                                                                                                                 |
| ------------------------------------------------------------------------------------------ | ---------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| SSO IdP misconfiguration locks out operators                                               | Med        | High     | Local-account break-glass path always available; documented in the runbook front-matter; alerting on SSO-rejected-login spikes.                                                            |
| HA failover causes in-flight saga loss                                                     | Med        | High     | Saga state persisted in Keep (which is replicated); sagas resume on failover; integration test includes mid-saga primary kill.                                                             |
| Zero-downtime upgrade hides a regression that only manifests post-upgrade                  | Med        | Med      | Post-upgrade smoke must pass before next node; automatic rollback on smoke failure; canary host first in the pool.                                                                         |
| Encryption key loss = total data loss                                                      | Low        | Critical | Key management via secrets backend with its own backup policy; documented key backup + recovery drill; customer retains final key custody.                                                 |
| mTLS cert expiry causes silent outage                                                      | Med        | High     | Cert expiry metric with alert at 30 days + 7 days + 1 day; `cert-manager` auto-renewal where available; runbook cert-rotation drill quarterly.                                             |
| Airgap mode surprises operators (some feature fails at runtime because it wanted external) | Med        | Med      | Boot-time verification pre-flights every configured integration; list of disabled providers surfaced in `wk airgap status`; runbook enumerates every external dependency the platform has. |
| Mattermost / Teams / Discord / Bitbucket API changes break adapter                         | Med        | Med      | Each adapter has contract tests + smoke tests; version pinning; runbook includes adapter-version compatibility matrix.                                                                     |
| HIPAA controls implemented but customer audit fails on a control we didn't anticipate      | Med        | High     | Evidence bundle includes shared-responsibility matrix; customer's compliance team reviews the matrix pre-audit; post-audit fixes fed back as control additions.                            |
| Secrets backend swap mid-prod fails and leaves deployment without credentials              | Low        | Critical | Swap procedure is dual-write (write to both backends until cutover verified); tested in staging; runbook enforces the procedure.                                                           |
| Break-glass used routinely instead of in emergencies                                       | Med        | Med      | Break-glass events high-visibility alerted to admin group; quarterly review of break-glass usage; soft-limit that requires explanation to Watchmaster after N uses.                        |
| Airgap install package signature verification broken                                       | Low        | High     | Signature verified on two independent layers (outer tarball + per-image); release CI produces reproducible builds; signature verification tested in every airgap smoke.                    |
| Multi-host harness placement converges all load on one host                                | Med        | Med      | Load-aware placement policy; placement decision logged; admin can override via Manifest `host_affinity`; metric on per-host harness count alerts on imbalance.                             |

---

## 6. Cross-cutting Constraints (additions to Phase 1–3)

- **No RBAC in Phase 4 either.** SSO restricts the WHO; everyone inside WHO has full operator access. Proposals to add fine-grained RBAC are deferred to Phase 6 (multi-tenancy) or rejected.
- **No new agent capabilities in Phase 4.** Every milestone is a trust primitive or a deployability primitive. Capability additions (richer self-mod, SDK, predictive) belong to Phase 5+.
- **Airgap mode is an opt-in flag, not a separate product.** Same binaries, same architecture; runtime behavior changes based on the flag.
- **Secrets never live in env vars in production.** `environment=production` config disables env-first secret paths.
- **Every external endpoint the platform may call is listed.** Airgap verification enumerates them; runbook documents them; they're either whitelistable or disableable.
- **Every persistent data store is encrypted at rest.** No exceptions in Phase 4 deployments — Keep Postgres, Notebook SQLite, ArchiveStore, and any M11 data classification enforcement.
- **Every internal call is mTLS.** Except intra-process (stdio between core and its own harness child on the same host).

---

## 7. Definition of Done (Phase 4)

- [ ] Operator logs into CLI and Manifest editor UI via SSO (SAML or OIDC) against a real IdP; joiner/leaver tested.
- [ ] Keep HA drill passes with RPO ≤ 5s and RTO ≤ 60s measured.
- [ ] Zero-downtime upgrade drill passes for both core and harness binaries while Watchkeepers are active.
- [ ] Stolen-disk test confirms every persistent store unreadable without keys.
- [ ] mTLS enforced between every internal service; certificate rotation drill passes.
- [ ] Secrets backend (Vault or equivalent) holds every credential in a production-configured deployment; `environment=production` refuses to boot with env-var secrets.
- [ ] Airgap deployment on an isolated host reaches Phase 1 success scenario using only internal services (Gemma + Mattermost + on-prem GitLab).
- [ ] Mattermost, MS Teams, Discord, and Bitbucket adapters each exercise their full Phase 1 M4 / M9 / M10 equivalent tests.
- [ ] HIPAA evidence bundle reviewed by a mock auditor against a published HIPAA checklist — all requested artifacts present.
- [ ] Phase 4 runbook walked through by a second engineer without assistance.
- [ ] `make phase4-smoke` and `make phase4-airgap-smoke` both pass in CI.

---

## 8. External Prerequisites

- [ ] **Customer SAML or OIDC IdP** configured with an operator group and metadata URL / OIDC discovery endpoint. _(before M1)_
- [ ] **Secrets backend endpoint** provisioned (Vault, AWS KMS, GCP KMS, Azure Key Vault, or equivalent). _(before M5)_
- [ ] **Certificate authority** (cert-manager in k8s; operator-managed local CA for docker-compose). _(before M5)_
- [ ] **HA hosts / k8s cluster** capable of running Postgres primary + replicas + Patroni coordinator. _(before M2)_
- [ ] **Harness host pool** of at least 2 hosts for M3 verification. _(before M3)_
- [ ] **Mattermost instance** (self-hosted or Cloud). _(before M7)_
- [ ] **Azure AD tenant** with bot creation privileges + Teams workspace. _(before M8)_
- [ ] **Discord test server** + developer portal access. _(before M9)_
- [ ] **Bitbucket workspace** (Cloud or Data Center) + access token. _(before M10)_
- [ ] **Airgap-capable test host** — isolated environment for M6 verification. _(before M6)_
- [ ] **Mock HIPAA auditor** — teammate with a HIPAA Security Rule checklist or similar. _(before M11 + M12 final)_

---

## 9. Out of Scope — deferred to Phase 5 / 6 (or rejected)

| Item                                                         | Target                  | Reason                                                                                                                 |
| ------------------------------------------------------------ | ----------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| Predictive analytics / proactive intelligence                | Phase 5                 | Business-concept Grand Party; adds agent capability, not trust primitive.                                              |
| Cross-org learning (anonymized pattern sharing)              | Phase 5                 | Needs multi-tenant trust + anonymization story + privacy review — Phase 5 alongside Grand Party.                       |
| Watchkeeper specialization via extended self-modification    | Phase 5                 | Advanced self-mod; Phase 4 stays focused on deployability.                                                             |
| API + SDK for third-party integrations + custom Watchkeepers | Phase 5                 | Needs stable contract; Phase 5 delivers it alongside SDK work.                                                         |
| Public cross-org role marketplace                            | Phase 6                 | Needs multi-tenant identity + trust + content-safety mechanisms.                                                       |
| SaaS multi-tenant deployment                                 | Phase 6                 | Phase 4 stays self-hosted; SaaS = separate operational product.                                                        |
| Multi-tenancy in a single instance                           | Phase 6                 | Defers with SaaS.                                                                                                      |
| RBAC                                                         | Rejected for Phase 4    | One-instance-one-org; revisit in Phase 6 under multi-tenancy.                                                          |
| Admin dashboard UI                                           | Rejected                | CLI + Slack/Teams/Mattermost + Grafana + Keeper's Log remain the operator surfaces; editor UI is the only web surface. |
| HIPAA certification                                          | Customer responsibility | We ship controls + evidence; certification paperwork is the customer's compliance team.                                |
| SOC2 Type II certification of the vendor corp                | Out of roadmap scope    | This is corporate compliance, not a product feature.                                                                   |
