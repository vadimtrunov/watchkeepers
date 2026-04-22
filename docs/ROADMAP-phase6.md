# Watchkeeper Phase 6 — Platform Ecosystem Implementation Roadmap

**Status**: Planning
**Created**: 2026-04-22
**Scope reference**: [watchkeeper-business-concept.md](./watchkeeper-business-concept.md), [ROADMAP-phase5.md](./ROADMAP-phase5.md)
**Next phase**: none — Phase 6 completes the business-concept vision. Phase 7+ (vertical specializations, edge deployment, fine-tuning per customer, etc.) is product-direction territory, not planned in this document.
**Deliverable type**: implementation roadmap — not executable code.

**Progress symbols**: `⬜` not started · `🟨` in progress · `✅` done · `🚫` blocked.

---

## 1. Executive Summary

Phases 1–5 built the platform: core loop (P1), multi-agent Party (P2), compositional depth with UI (P3), enterprise deployability (P4), proactive intelligence with federation (P5). Phase 6 is the **ecosystem phase** — opening the platform up to many organizations at once, either via multi-tenancy in a single instance or via SaaS, and growing a public marketplace where templates, tools, and runtime extensions flow between deployments.

This is the most ambitious phase. It adds three foundational capabilities the previous five avoided:

- **Multi-tenancy** — one instance serves many organizations; tenant isolation is load-bearing.
- **RBAC** — finally motivated, because multi-tenancy means operators of different tenants must not see each other.
- **SaaS operability** — billing, customer onboarding automation, tenant provisioning, and vendor-side compliance (the vendor corp becomes the data processor for many customers).

Plus two ecosystem fabric pieces:

- **Public cross-org marketplace** — tenants can opt-in to publish templates and tools to a shared catalog; community discovery, reputation, moderation.
- **Runtime-extension SDK** — third parties write harness plugins (WASM-sandboxed), not just TS tools.

Plus federation maturity:

- **Dynamic peer discovery** via a registry, replacing Phase 5's static config.
- **Differential-privacy-strength anonymization** option on top of k-anonymity.

### What completes after Phase 6

Business-concept goals satisfied end-to-end:
- Phase 1 "First Party" ✓ (P1)
- Phase 2 "Party Grows" ✓ (P2)
- Phase 3 "Platform" ✓ (P3 UI + P4 enterprise + P6 multi-tenancy/SaaS/ecosystem)
- Phase 4 "Grand Party" ✓ (P5 proactive + P6 public marketplace + runtime SDK)

After Phase 6, remaining work is product direction — vertical specializations, on-device harnesses, per-customer model fine-tuning, voice/phone adapters — treated as post-platform topics, not this roadmap's responsibility.

### Framing — the most deliberate phase

Every earlier phase said "no" to at least some of what lands here. The reasons were about sequencing, not rejection: multi-tenancy needs a stable single-tenant product first; public marketplace needs content-safety mechanisms that only make sense with multi-tenancy; SaaS needs compliance posture that only makes sense after the product is proven. Phase 6 picks up the accumulated Yes List.

Because of this, Phase 6 is larger than prior phases. Realistic expectation: ~4–6 months of one team's work, possibly split across two sub-phases (6a = multi-tenancy + RBAC + SaaS; 6b = public marketplace + runtime SDK + federation maturity).

Success metric: **at least 3 tenants operating independently on one instance with full isolation verified, OR at least 3 customers onboarded via SaaS self-service flow without engineering touch, AND a public marketplace with at least 10 published artifacts consumed across tenants**.

---

## 1.1 Status Dashboard

| # | Milestone | Status | Magnitude | Notes |
|---|---|---|---|---|
| M1 | Multi-tenancy foundation (tenant schema + Keep + Notebook scoping) | ⬜ | 10–14d | foundation for everything Phase 6 |
| M2 | RBAC (capability-based for operators, with 3 default presets) | ⬜ | 5–7d | motivated by M1 |
| M3 | Per-tenant resource quotas + noisy-neighbor isolation | ⬜ | 6–8d | |
| M4 | SaaS deployment architecture (provisioning, tenant lifecycle) | ⬜ | 10–14d | vendor operates a central cluster |
| M5 | Billing integration + usage metering | ⬜ | 7–10d | Stripe / equivalent |
| M6 | Customer self-service portal (sign-up, tenant admin, user invites) | ⬜ | 8–12d | web surface for tenants |
| M7 | Vendor-side compliance posture (SOC2 Type II readiness as a vendor) | ⬜ | 7–10d | different from Phase 3 M11 customer-side |
| M8 | Public cross-org marketplace | ⬜ | 8–12d | opt-in publishing + moderation + trust tiers |
| M9 | Runtime-extension SDK (WASM-sandboxed harness plugins) | ⬜ | 12–16d | biggest capability addition |
| M10 | Dynamic peer discovery (federation registry service) | ⬜ | 5–7d | replaces Phase 5 static config |
| M11 | Differential-privacy option for federation anonymization | ⬜ | 6–9d | strictest tier, opt-in |
| M12 | Phase 6 integration demo + ecosystem acceptance | ⬜ | 7–10d | acceptance gate |

Total: ~91–129 days for one team. **Consider splitting into Phase 6a (M1–M7, ~53–75 days) and Phase 6b (M8–M12, ~38–54 days)** — operator may want to sequence rather than parallelize.

---

## 2. Key Architectural Decisions (additions to Phase 1–5)

| Concern | Decision | Rationale |
|---|---|---|
| Tenant model | `tenant` is a new top-level entity: every Keep row, Notebook file, Manifest, capability token, budget is scoped to a `tenant_id`. One instance serves many tenants. | Multi-tenancy is load-bearing — either proper isolation or don't bother. |
| Tenant isolation | Enforced at every boundary: Keep RLS adds tenant predicate; Notebook file paths include tenant prefix; messenger adapters per tenant; per-tenant capability broker namespaces. | Any gap in isolation = total-product-failure risk in multi-tenant context. |
| RBAC model | Capability-based, reusing Phase 1 broker primitives. Three default presets per tenant: `admin` (everything), `operator` (spawn/retire/approve), `viewer` (read-only including Keeper's Log viewing). Tenants can author custom presets via the Manifest editor UI. | Consistent with existing capability model; simple defaults + flexibility. |
| SaaS operational model | Vendor operates a central cluster that hosts all SaaS tenants. Self-hosted deployments remain a supported path. SaaS and self-hosted share the same codebase; SaaS adds operational surfaces (billing, provisioning, customer portal). | Keeps the codebase unified; enables customers to migrate between self-hosted and SaaS. |
| Customer onboarding | Self-service web flow: sign up → tenant provisioned in ≤5 min → guided first-Watchkeeper spawn via the Manifest editor UI → usable within 15 min. | "Customers creating custom Watchkeepers without engineering support" is the success metric; slow onboarding kills it. |
| Billing | Pluggable `BillingBackend` interface with Stripe as the first implementation. Usage meters: active Watchkeepers, token spend (across all `LLMProvider`s), Keep storage, Notebook storage, ArchiveStore storage, federation bandwidth. Invoice generation monthly. Customer-portal view of current-month usage. | Billing must be accurate enough to charge but simple enough to implement in a reasonable milestone. |
| Public marketplace | Opt-in publishing per tenant. Three trust tiers: **vendor-signed** (default visible), **community-signed** (opt-in per tenant), **unsigned** (disabled by default, operator explicit enable). Moderation queue for all vendor-signed submissions; community signing relies on community reputation. | Minimum viable safety without building a full community-moderation system; vendor acts as default gatekeeper. |
| Runtime SDK | **WASM-sandboxed plugins** executing in the harness host. Plugins written in any WASM-targetable language (Go, Rust, AssemblyScript). Plugin interface exposes capability-gated host calls only. | WASM gives language freedom + sandbox safety; Go plugins (`plugin` package) are native-only, not sandboxed, and unsuitable for untrusted third-party code. |
| Federation discovery | Vendor-operated **federation registry** (optional — operators can also run their own). Keep instances register their public endpoint + trust metadata; others look them up by trust-group tag. | Static config from Phase 5 M9 doesn't scale to ecosystems with many peers. |
| Anonymization tiers | k-anonymity from Phase 5 M10 remains default. Phase 6 adds an **optional differential-privacy tier** (ε-DP with configurable budget) for deployments in sensitive industries. | DP guarantees are stronger but degrade pattern utility; opt-in keeps both options available. |
| Vendor compliance | Vendor corp achieves SOC2 Type II for the SaaS offering. Phase 3 M11 covered controls for customer-side SOC2 audits; Phase 6 M7 covers vendor-side controls for vendor-audited compliance. | Enterprise customers ask "is your vendor SOC2?" before adopting SaaS; without it, sales stalls. |
| Multi-tenancy and airgap | Airgap mode implies single-tenant — multi-tenancy + airgap is a conflicting config; core refuses to boot. | Airgap is customer-own-infrastructure; multi-tenancy is vendor-hosted-or-shared-infrastructure. They don't overlap. |
| Admin dashboard UI | Still rejected. The Manifest editor UI is extended to host customer portal + marketplace browse + moderation dashboard + billing view. One web surface, no second. | Consistent with all prior phases. |

---

## 3. Scope

### In
- Tenant-scoped Keep + Notebook + Manifest + toolset + messenger bindings.
- Capability-based RBAC with 3 default presets; custom presets per tenant.
- Per-tenant resource quotas; noisy-neighbor protection; billing-tied limits.
- SaaS cluster operational architecture: tenant provisioning, lifecycle, vendor-operated infrastructure.
- Billing integration (Stripe first) with usage metering across all cost dimensions.
- Customer self-service portal: sign-up, user invites, tenant admin, billing view, marketplace browse, Manifest editor — all one web app (extending Phase 3 M4).
- Vendor-side SOC2 Type II compliance controls + evidence collection for the SaaS offering.
- Public cross-org marketplace with opt-in publishing, trust tiers, moderation, reputation signals.
- Runtime-extension SDK using WASM sandbox with capability-gated host calls.
- Federation registry service for dynamic peer discovery.
- Differential-privacy tier for anonymization pipeline.
- Phase 6 integration demo: 3 tenants, SaaS self-service flow, public marketplace with cross-tenant consumption, WASM plugin demo, federation via registry.

### Out (Phase 7+ or rejected)
- Vertical specializations (healthcare-specific, finance-specific Watchkeepers) — post-platform product direction.
- On-device / edge harnesses (mobile agents, IoT Watchkeepers) — post-platform product.
- Per-customer model fine-tuning — post-platform (LLM-infrastructure play).
- Voice / phone messenger adapters — post-platform or specialty project.
- Federated identity across tenants (tenant A user accesses tenant B resources) — sufficient complexity to defer as its own later phase.
- Cross-tenant Keeper-to-Keeper communication (agent in tenant A talks to agent in tenant B without federation) — rejected; federation is the cross-tenant path.
- Admin dashboard UI — rejected, consistent with all prior phases.

---

## 4. Milestones

### ⬜ M1 — Multi-tenancy foundation
**Goal**: `tenant` becomes a top-level entity; every resource in the platform is tenant-scoped; isolation verified at every boundary.

**Scope**
- [ ] Schema: `tenant` table; foreign-key `tenant_id` added to every Keep table (organization, human, watchkeeper, manifest, manifest_version, keepers_log, knowledge_chunk, outbox, watch_order, role_template, shared_lesson, cross_org rows).
- [ ] RLS policies updated: every query constrained to the tenant_id of the caller's session. Cross-tenant reads refused at RLS layer.
- [ ] `keepclient` sessions carry tenant_id; operator's SSO identity maps to one tenant (Phase 6 default — federated multi-tenant identity is post-Phase-6).
- [ ] Notebook files: path includes `<tenant_id>/<agent_id>.sqlite`; filesystem perms restrict per tenant.
- [ ] Tool sources per tenant: `$DATA_DIR/tools/<tenant_id>/private/`, `$DATA_DIR/tools-local/<tenant_id>/`. Built-in and platform sources shared across tenants (read-only, identical).
- [ ] Messenger bindings: each tenant has its own Slack/Teams/etc workspace linkage; core never mixes channels across tenants.
- [ ] Manifest `tenant_id` immutable after creation; Watchkeepers cannot migrate across tenants.
- [ ] Capability broker per-tenant namespace: capability tokens carry tenant_id; cross-tenant capability use refused at broker layer.
- [ ] Federation `cross_org` rows from Phase 5 get a tenant scope — each tenant federates independently.
- [ ] Migration tool for Phase 5 deployments: existing single-tenant data migrates to a single `tenant_id=primary` — zero functional change for self-hosted, lays the groundwork.

**Artifacts**: schema migrations, RLS policy updates, `keepclient` session extensions, Notebook file-path changes, capability broker namespace updates, single-tenant migration tool.

**Verification**
- [ ] Isolation test: tenant A creates a Watchkeeper; tenant B's operator cannot see it via any interface (CLI, editor UI, Keep query).
- [ ] Cross-tenant capability attempt: tenant A's token used against tenant B's resource refused.
- [ ] Federation scoping: tenant A's `cross_org` rows only federate with tenant A's configured peers, not tenant B's.
- [ ] Migration: Phase 5 single-tenant deployment migrates cleanly; all existing data accessible under `tenant_id=primary`.
- [ ] Penetration test: simulated malicious operator attempts cross-tenant access through known attack patterns (IDOR, session confusion, RLS bypass); all refused.

**Dependencies**: Phase 1 M2, Phase 4 M1 (SSO).
**Magnitude**: 10–14 days.

---

### ⬜ M2 — RBAC (capability-based for operators)
**Goal**: Tenants can restrict what operators within their tenant can do, via capability-based role presets.

**Scope**
- [ ] Operator capability model: reuse Phase 1 broker primitives, operators get capability tokens scoped to their tenant and their role.
- [ ] Three default role presets per tenant:
  - `admin` — every capability.
  - `operator` — spawn/retire/approve/read-write Keep; no billing, no role-preset edits, no tenant-admin actions.
  - `viewer` — read-only: Keeper's Log tail, Watchkeeper list, Manifest read. No writes anywhere.
- [ ] Custom role presets: tenant admin can compose custom role presets via the Manifest editor UI (Phase 3 M4 extended).
- [ ] SSO attribute mapping extended: IdP group claims map to tenant roles (not just binary operator-yes/no from Phase 4 M1).
- [ ] Capability enforcement in every operator-facing endpoint: CLI checks role, editor UI checks role, HTTP/RPC APIs check role.
- [ ] Operator action audit: every privileged action already in Keeper's Log; now carries both SSO identity and resolved role.
- [ ] Role changes: tenant admin demotes a user in real time — active sessions refresh capability tokens on next request, access tightens immediately.

**Artifacts**: role-preset schema, operator capability broker extension, editor UI role management, SSO attribute mapping update.

**Verification**
- [ ] `viewer` role attempts a `wk spawn` command — refused with role-based error.
- [ ] Custom role: tenant admin composes "PR-reviewer-operator" with only specific capabilities; user with that role can only perform those actions.
- [ ] Real-time demotion: user's role changed to `viewer`; next request fails write attempt; no restart needed.
- [ ] Audit: every refused action logged with SSO identity + role + requested capability.

**Dependencies**: M1.
**Magnitude**: 5–7 days.

---

### ⬜ M3 — Per-tenant resource quotas + noisy-neighbor isolation
**Goal**: One tenant's heavy usage cannot degrade another tenant's experience; quotas enforceable for billing.

**Scope**
- [ ] Quota dimensions per tenant (configurable, with billing-tier defaults):
  - Concurrent Watchkeepers (soft + hard limits).
  - Token spend per day across all `LLMProvider`s.
  - Keep storage (rows + bytes in `knowledge_chunk`).
  - Notebook storage (total across all agents).
  - ArchiveStore storage.
  - Federation bandwidth (in + out).
  - `propose_tool` / `self_tune` / `propose_intervention` per day per Watchkeeper and per tenant.
- [ ] Enforcement points: capability broker checks quota before token issuance; writes to Keep / Notebook / ArchiveStore check size; harness spawn checks concurrent count.
- [ ] Soft vs hard limits: soft limit triggers alert + cost-tier upgrade suggestion; hard limit refuses the action with a clear error + upgrade CTA.
- [ ] Per-tenant runtime isolation: harness processes can be assigned per-tenant host pools (from Phase 4 M3) to prevent cross-tenant CPU contention at the hardware level.
- [ ] Postgres row-level fairness: slow queries scoped to a single tenant don't block other tenants (statement timeouts, per-role connection limits, separate connection pools per tenant optionally).
- [ ] Usage metrics collection (feeds M5 billing): real-time usage counters, end-of-hour rollups, end-of-day snapshots.

**Artifacts**: quota schema + enforcement package, usage-metering service, per-tenant host-pool extension to lifecycle manager, Postgres fairness config.

**Verification**
- [ ] Quota breach: tenant hits token-spend hard limit; further LLM requests refused with explicit message; tenant admin sees actionable error.
- [ ] Noisy-neighbor test: tenant A runs a pathologically expensive Keep query; tenant B's latency unaffected (within SLO).
- [ ] Soft-limit alert: tenant at 85% of quota receives alert with upgrade CTA; usage continues normally.
- [ ] Real-time usage counters match end-of-day rollup within expected drift (for billing accuracy).

**Dependencies**: M1, M2, Phase 4 M3 (host pools).
**Magnitude**: 6–8 days.

---

### ⬜ M4 — SaaS deployment architecture
**Goal**: Vendor operates a central multi-tenant cluster; new tenants provisioned on demand.

**Scope**
- [ ] SaaS cluster topology: Kubernetes-based deployment (or operator equivalent) with core, Keep (HA from Phase 4), harness pool, editor UI, marketplace, federation registry (M10).
- [ ] Tenant provisioning flow: new tenant signup → tenant row created → tenant admin invited via email → initial default Manifest templates installed → ready in ≤5 min.
- [ ] Tenant lifecycle operations: pause (stop all Watchkeepers, retain data), resume, suspend (billing failure), delete (full tenant data purge with GDPR-equivalent retention window).
- [ ] Per-tenant secrets: each tenant's secrets (LLM keys, Slack tokens, etc.) live in a tenant-scoped path in the secrets backend; cross-tenant secret read refused at the secrets-backend layer.
- [ ] Tenant domain model: SaaS customer might have `acme.wk-saas.com` subdomain OR custom domain; both supported with TLS.
- [ ] Vendor operations runbook: tenant creation / suspension / deletion, incident response per tenant, emergency access procedures (break-glass from Phase 4 M11 extended with tenant scoping).
- [ ] Deployment smoke: `make saas-deploy-smoke` provisions a SaaS cluster from scratch, creates a tenant, verifies full flow.

**Artifacts**: k8s manifests / operator, tenant provisioning API, tenant lifecycle state machine, vendor ops runbook, deploy automation, SaaS smoke test.

**Verification**
- [ ] Tenant provisioning: from API call to "Watchkeeper spawned" in ≤15 min via the self-service portal (M6).
- [ ] Pause/resume: tenant paused loses active Watchkeepers cleanly (Phase 1 M7 retire flow); resume reaches prior state.
- [ ] Deletion: tenant deleted → all data purged per GDPR; Keeper's Log tombstoned with purge manifest.
- [ ] Custom domain: tenant with custom domain accessible via TLS; domain-verification flow works.

**Dependencies**: M1, M3, Phase 4 (HA + encryption + mTLS + secrets).
**Magnitude**: 10–14 days.

---

### ⬜ M5 — Billing integration + usage metering
**Goal**: Vendor charges tenants based on usage; billing accurate, transparent, audit-able.

**Scope**
- [ ] `BillingBackend` Go interface with Stripe as the first implementation.
- [ ] Usage metering pipeline: real-time events from M3 → hourly aggregates → daily snapshots → monthly invoice generation.
- [ ] Billing plan templates: Free, Starter, Pro, Enterprise — each defining quota defaults for M3 dimensions + monthly fee + per-unit overage pricing.
- [ ] Invoice generation: monthly cycle per tenant, PDF + email, usage breakdown per dimension, historical invoice archive in ArchiveStore.
- [ ] Payment failure handling: retry logic; soft-suspend tenant after N failed retries with tenant-admin notifications; hard-suspend → eventual deletion per service agreement.
- [ ] Tax handling: via Stripe Tax or equivalent; tenant's jurisdiction inferred from billing address, user-correctable.
- [ ] Customer-portal billing view: current month usage against quota + projected overage + historical invoices + payment method management.
- [ ] Audit: every billing-affecting event (quota breach, plan change, invoice generation, payment) in Keeper's Log with tenant_id.

**Artifacts**: `billing/` Go package with Stripe implementation, metering pipeline, billing plans config, invoice generator, customer-portal billing section, tax plumbing.

**Verification**
- [ ] Monthly cycle: tenant's usage for a simulated month is metered, aggregated, invoiced; tenant sees invoice in portal; Stripe receives charge.
- [ ] Overage: tenant exceeds plan quota mid-month; per-unit overage pricing applied; tenant sees projected overage in portal before invoice.
- [ ] Payment failure: simulated card decline; retry logic runs; tenant-admin notified; eventual soft-suspend triggers as configured.
- [ ] Plan change: tenant upgrades mid-month; pro-rated billing correct; new quotas in effect immediately.

**Dependencies**: M3, M4.
**External prerequisite**: Stripe (or equivalent) account for the vendor; tax service account.
**Magnitude**: 7–10 days.

---

### ⬜ M6 — Customer self-service portal
**Goal**: Customers sign up, configure their tenant, invite users, spawn their first Watchkeeper — all without vendor engineering involvement.

**Scope**
- [ ] Signup flow: email + password (fallback) or SSO (Google / Microsoft / GitHub — delegated to the vendor's central IdP). Tenant auto-provisioned.
- [ ] First-time setup wizard: choose a plan (M5), connect first messenger workspace (Slack / Teams / etc. — OAuth flow), pick an initial Manifest template from marketplace (M8), spawn first Watchkeeper with one click.
- [ ] Tenant admin surfaces (extending the Phase 3 M4 editor UI app):
  - User management: invite, remove, change role (from M2 presets or custom).
  - Billing: view from M5.
  - Integrations: connect/disconnect messengers, VCS adapters, Jira/Confluence, LLM provider credentials.
  - Marketplace: browse + install.
  - Manifest editor: existing Phase 3 M4 functionality, now tenant-scoped.
  - Keeper's Log viewer: read-only audit trail for this tenant.
- [ ] Email transactional flows: signup verification, invites, billing alerts, quota alerts, incident notifications.
- [ ] Help center + contextual docs: every screen links to relevant runbook section.
- [ ] Onboarding analytics: funnel tracking (tenant created → first Watchkeeper spawned → first real usage) — used by vendor success team, NOT exposed to customers (no admin dashboard, but vendor-internal observability is OK).

**Artifacts**: SvelteKit app extensions, email service integration, help center content, vendor-internal onboarding funnel dashboard.

**Verification**
- [ ] Cold signup: new customer signs up, completes wizard, spawns first Watchkeeper in ≤15 min, no vendor engineering intervention.
- [ ] Invite flow: tenant admin invites second user, second user accepts, both can operate per their roles.
- [ ] Plan upgrade: tenant admin upgrades plan through portal; effective immediately (M5).
- [ ] Email flows: all transactional emails deliver, link to correct pages, localized if configured.

**Dependencies**: M2, M4, M5, Phase 3 M4.
**Magnitude**: 8–12 days.

---

### ⬜ M7 — Vendor-side SOC2 Type II readiness
**Goal**: Vendor corp has the controls and evidence to pass a SOC2 Type II audit as the SaaS platform operator, distinct from customer-side SOC2 work (Phase 3 M11).

**Scope**
- [ ] Vendor's own access controls: internal staff access to production via bastion + MFA + time-bound elevation + audit.
- [ ] Change management: every production change through PR + code review + CI + signed release + deploy approval + runbook update.
- [ ] Incident response: classified incident types, notification SLAs, post-mortems in a standard template, quarterly review.
- [ ] Vendor-corp evidence collection: continuous-compliance tooling that gathers access logs, change records, backup completions, SLO breaches, security patches for audit prep.
- [ ] BAA / DPA templates ready (for customers in regulated industries).
- [ ] Vendor-side security monitoring: IDS/IPS, vulnerability scanning, dependency auditing, penetration test cycle.
- [ ] Customer-facing status page: real-time SLO posture per service; incident history; scheduled maintenance windows.
- [ ] Customer-trust page: current certifications, controls summary, subprocessor list, data-residency options.

**Artifacts**: production-access-control config, change-management workflow docs, incident-response runbook, compliance-evidence collector, BAA/DPA templates, status page, trust page.

**Verification**
- [ ] Evidence bundle generated for a mock SOC2 Type II audit covering 3-month period; auditor (teammate) verifies presence of expected controls + evidence.
- [ ] Incident drill: simulated production incident triggers notification SLA, post-mortem process, status page update — all in spec.
- [ ] Access review: quarterly access review drill completes; orphaned access revoked within the review cycle.

**Dependencies**: Phase 4 (all hardening milestones).
**External prerequisite**: relationship with a SOC2 audit firm; vendor-corp internal compliance coordinator.
**Magnitude**: 7–10 days.

---

### ⬜ M8 — Public cross-org marketplace
**Goal**: Tenants can publish templates and tools to a shared, cross-tenant catalog; tenants can discover and install published artifacts.

**Scope**
- [ ] Publishing flow: tenant admin flags a Manifest template or a tool for publishing; goes through moderation (M8 scope); approved artifacts appear in the public catalog.
- [ ] Three trust tiers (per Phase 6 decision):
  - **Vendor-signed**: platform vendor maintains a curated set — like a "featured" section; default-visible to all tenants.
  - **Community-signed**: community contributors with verified identity; opt-in tier per tenant (config flag `marketplace.show_community: true`); default off.
  - **Unsigned**: explicit-only; tenant admin must enable per artifact.
- [ ] Artifact metadata: publisher tenant / vendor / community identity, description, tags, rating (M8 scope), usage stats, security-scan results.
- [ ] Reputation signals: usage count, rating stars, signed-by, last-update, incident-history (if an artifact had security issues, visible).
- [ ] Moderation queue: submitted artifacts enter a vendor-moderator queue; moderators (initially vendor staff; later community moderators) review code + description + security scans + legal (IP); approve / reject with reason / request changes.
- [ ] Security scanning: static analysis + dependency-vuln scan + capability-declaration lint on every submission; results visible to moderators + published alongside the artifact.
- [ ] Takedown + revocation: moderators can remove artifacts for policy violations; tenants who've installed get a warning + can roll back; authors can also withdraw their own artifacts.
- [ ] Legal framework: publisher terms of service (vendor's IP protections, liability disclaimer, customer's right to their IP) + consumer terms (no warranty, use at own risk, report issues).
- [ ] Cross-tenant billing consideration: free publishing for vendor-signed; community-signed may charge the consumer tenant via M5 metering.

**Artifacts**: marketplace submission API, moderation dashboard (in editor UI), trust-tier config, security-scan pipeline, reputation tracking, takedown tooling, legal-document templates.

**Verification**
- [ ] Publishing: tenant A publishes a Manifest template; enters moderation; moderator approves; template visible in all tenants' marketplace browse.
- [ ] Trust tiers: tenant B has `show_community: false`; community-signed artifact from tenant A not visible in B's browse.
- [ ] Installation: tenant C installs a vendor-signed tool from marketplace; tool works in their deployment via existing Tool Registry mechanism.
- [ ] Takedown: artifact flagged for policy violation; moderator removes; installed tenants receive Slack warning; revocation propagates.
- [ ] Reputation: usage count increments correctly across tenants; ratings aggregate fairly.
- [ ] Malicious artifact: submission containing an obviously-bad pattern (e.g., tries to exfiltrate tokens) blocked by security scan; reviewed as incident.

**Dependencies**: M1–M3, Phase 3 M5 (internal marketplace), Phase 5 M8 (vendor-tool marketplace).
**Magnitude**: 8–12 days.

---

### ⬜ M9 — Runtime-extension SDK (WASM-sandboxed harness plugins)
**Goal**: Third parties write runtime-level extensions to the harness in any WASM-targetable language. Sandboxed execution, capability-gated host calls.

**Scope**
- [ ] Plugin interface: WASM modules export a set of standard entry points (`on_turn_start`, `on_tool_invoke`, `on_tool_result`, `on_notebook_write`, `on_manifest_change`, etc.); host calls are capability-gated (the plugin declares needed capabilities, operator approves at install).
- [ ] WASM runtime integration: `wazero` (Go WASM runtime) or equivalent embedded in the harness; fuel-limited execution; memory-limited execution; no network/fs access except through host calls.
- [ ] Host-call surface:
  - `keep_read`, `keep_write` (capability-gated per entry).
  - `notebook_read`, `notebook_write` (capability-gated).
  - `http_request` (only to explicitly-whitelisted endpoints per plugin).
  - `log` (structured logs, never PII).
  - `metric_emit`.
- [ ] Plugin manifest: similar shape to Tool Manifest — capabilities, schema, signing, version, WASM hash.
- [ ] SDK documentation + examples: reference plugins in Go, Rust, AssemblyScript; covered use cases (custom auto-injection logic, custom approval-card rendering, custom reflection prompt templates, custom metric computation).
- [ ] Tool Registry extension: plugins join the existing multi-source overlay (built-in → platform → private → local) with the same signing / approval / hot-reload mechanics.
- [ ] Plugin marketplace (extending M8): plugins publishable alongside tools and templates.
- [ ] Safety: plugins cannot bypass capability broker; resource limits enforced at WASM boundary; crashes contained (plugin panic kills plugin but not harness).

**Artifacts**: `wasmhost/` Go package, host-call surface implementation, plugin manifest schema, 3+ reference plugin examples, SDK docs, marketplace integration.

**Verification**
- [ ] Go-authored plugin loaded, runs an `on_turn_start` hook visible in trace logs.
- [ ] Rust-authored plugin works identically.
- [ ] Capability enforcement: plugin attempts `keep_write` without declared capability → refused.
- [ ] Runaway plugin: infinite loop killed by fuel limit; harness keeps running.
- [ ] Resource isolation: plugin allocating unbounded memory hits WASM memory limit; harness keeps running.
- [ ] Marketplace: vendor publishes a plugin via M8 flow; customer installs via capability approval; plugin active.

**Dependencies**: M1, M8, Phase 5 M7 (SDK foundations).
**Magnitude**: 12–16 days.

---

### ⬜ M10 — Dynamic peer discovery (federation registry)
**Goal**: Keep instances discover each other through a registry rather than static config. Replaces Phase 5 M9's static peering.

**Scope**
- [ ] Federation registry service: Go binary deployed separately, holds a list of Keep endpoints with public metadata (trust-group tags, pattern-type offerings, contact, public-key fingerprint).
- [ ] Registration flow: Keep instances register themselves (public endpoint + mTLS pubkey fingerprint + trust-group tag); admin approval on registry side before listing.
- [ ] Discovery flow: Keep queries registry for peers matching a trust-group tag; returns list of candidate endpoints; Keep initiates mutual-TLS + signature verification; adds to active peer list.
- [ ] Trust-group membership: registry admin gates who can claim a trust-group; prevents random deployments from claiming to be part of a corporate trust group.
- [ ] Vendor-operated registry + self-hosted option: vendor hosts a public registry for community trust groups; enterprise customers can run their own registries for their corp-wide trust group.
- [ ] Registry schema: tenant + deployment + trust_group + endpoint + pubkey_fingerprint + pattern_type_offerings + last_seen.
- [ ] Fallback: Keep deployments that prefer static config (Phase 5 M9 style) continue to work; registry is additive, not mandatory.
- [ ] Airgap: registry completely unavailable in airgap mode; static config is the only path (already the case from Phase 4/5).

**Artifacts**: `fedregistry/` Go service, registry schema + API, registration flow + admin dashboard, discovery-integration in Keep federation code.

**Verification**
- [ ] Keep instance registers; admin approves; listed in registry for its trust group.
- [ ] Discovery: another Keep queries registry by trust-group tag; receives endpoint list; establishes federation; exchanges patterns.
- [ ] Impersonation prevention: deployment tries to register under a trust group it doesn't belong to → refused at admin approval step.
- [ ] Static-config fallback: Phase 5 M9 static-config setup still works alongside registry-discovered peers.

**Dependencies**: Phase 5 M9, M1 (tenancy informs registry data model).
**Magnitude**: 5–7 days.

---

### ⬜ M11 — Differential-privacy tier for anonymization
**Goal**: Opt-in stronger anonymization guarantee for federations in sensitive industries, on top of Phase 5 M10 k-anonymity.

**Scope**
- [ ] ε-differential-privacy option on the Phase 5 M10 pipeline: per-deployment configurable ε budget; randomized response / Laplace noise injection on numeric pattern features.
- [ ] Budget tracking: ε consumed per tenant per federation per time window; when budget exhausted, federation pauses patterns until next window.
- [ ] Utility/privacy tradeoff documentation: explicit table of what ε values deliver what kind of privacy vs. what pattern utility.
- [ ] UI surface: tenant admin enables DP in marketplace browse of "federation anonymization policies"; sees explicit warning about potential pattern-utility degradation.
- [ ] Composition rule: if multiple patterns from the same row get federated, ε budget composes properly across them (prevents budget-stacking attacks).
- [ ] Audit: DP-enabled federations have budget history in Keeper's Log; tenants can query their privacy-budget usage.

**Artifacts**: DP-tier anonymization implementation, budget tracker, tenant config extension, utility/privacy docs, audit schema.

**Verification**
- [ ] DP-enabled pattern federation: sent pattern has noise injection; raw underlying row non-recoverable from pattern alone.
- [ ] Budget exhaustion: tenant exhausts ε budget within window; subsequent patterns refused until budget resets.
- [ ] Composition: multiple patterns from same row respect composition rule; total ε never exceeds budget.
- [ ] Utility trade-off: consumer side sees degraded pattern precision at low ε vs high ε — measured and documented.

**Dependencies**: Phase 5 M10.
**Magnitude**: 6–9 days.

---

### ⬜ M12 — Phase 6 integration demo + ecosystem acceptance
**Goal**: Acceptance gate for the full ecosystem. Three tenants operating independently, SaaS self-service onboarding, public marketplace with cross-tenant consumption, runtime plugin in action, dynamic federation.

**Scope**
- [ ] SaaS cluster deployment: vendor-operated cluster provisioned; 3 tenants onboarded through self-service flow (M6) with ≤15 min to first Watchkeeper each.
- [ ] Multi-tenancy isolation proof: penetration test scenario attempting cross-tenant access via every interface; all attempts refused.
- [ ] Billing demo: simulated month of usage across 3 tenants on different plans; invoices generated; quota breaches handled per plan rules.
- [ ] Public marketplace demo:
  - Tenant A publishes a Manifest template; moderator approves.
  - Tenant B discovers and installs it.
  - Tenant C installs a vendor-signed tool.
  - Tenant C enables community tier; installs a community-signed plugin.
- [ ] Runtime plugin demo: third-party-authored WASM plugin installed by a tenant; plugin intercepts turn start, adds custom metric, visible in Grafana.
- [ ] Dynamic federation demo: two tenants (on same cluster or different clusters) federate via registry; one promotes anonymized pattern; other ingests; pattern used in Watchkeeper decision.
- [ ] Differential privacy demo: federation with DP enabled; pattern shape visibly noisier; consumer's decision quality trade-off observable.
- [ ] Vendor-side compliance demo: SOC2 evidence bundle generated for 3-month mock window; mock auditor reviews.
- [ ] `make phase6-smoke` runs condensed versions of each demo in CI.
- [ ] Phase 6 runbook finalized: customer onboarding, tenant admin, marketplace publishing, plugin authoring, federation setup, billing ops, incident response for SaaS.

**Artifacts**: SaaS demo deploy, pen-test scenario, billing demo, marketplace demo, plugin demo, federation demo, DP demo, vendor-compliance evidence, smoke test, runbook.

**Verification**
- [ ] All demos pass end-to-end without intervention.
- [ ] Penetration test reveals no cross-tenant leaks.
- [ ] Billing accuracy within 1% across 3-tenant simulated month.
- [ ] Phase 6 runbook completed by a teammate who didn't build Phase 6.
- [ ] `make phase6-smoke` passes green in CI.

**Dependencies**: M1–M11.
**Magnitude**: 7–10 days.

---

## 5. Risks (Phase 6-specific)

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Cross-tenant isolation gap (any interface) | Med | Critical | Isolation enforced at multiple layers (RLS, filesystem, capability namespace, messenger bindings); pen-test scenarios in M12; bounty-style review by external security team before SaaS GA. |
| Noisy-neighbor impact despite M3 quotas | Med | High | Per-tenant host pools; Postgres statement timeouts + connection limits; metric-driven quota adjustments; customer-visible "unusually loaded neighbor" indicator for plans that don't include dedicated infra. |
| SaaS compliance gaps (SOC2 Type II) found during audit | Med | High | Mock-auditor drill quarterly; continuous-compliance collector fills gaps proactively; vendor-corp internal compliance coordinator owns readiness. |
| Billing inaccuracy leads to customer disputes or revenue loss | Med | High | Real-time metering + end-of-day rollup cross-checks; monthly reconciliation; clear usage breakdown in customer portal; Stripe integration tested against known edge cases. |
| Public marketplace hosts malicious artifact that passes moderation | Low | Critical | Multi-layer: security scan + capability-declaration lint + signature + moderator review + rapid takedown + customer-side capability enforcement (even a bad tool can't do anything outside its declared capabilities). |
| WASM plugin bypasses sandbox | Low | Critical | Use a well-audited WASM runtime (`wazero`); plugin capabilities declared + broker-enforced; runaway-resource limits at WASM boundary; plugins treated as untrusted even with approved capabilities. |
| Community-tier marketplace attracts bad-faith contributions (spam, name-squatting, trademark infringement) | High | Med | Community tier is opt-in per tenant (default off); moderator queue has dedicated staff time; automated detection for common patterns; clear ToS + DMCA-equivalent takedown. |
| Federation registry impersonation / Sybil attack | Med | High | Admin-approved registration; trust-group membership gated; public-key pinning; registry operators are accountable (vendor or customer corp, not anonymous). |
| Differential-privacy parameters misconfigured, rendering patterns useless OR leaking | Med | High | Presets only (not raw ε); utility/privacy trade-off table in UI; config changes require tenant-admin approval; periodic audit of DP-enabled federations. |
| Onboarding funnel high drop-off (customers sign up but don't activate) | Med | High | Onboarding analytics (vendor-side only); targeted help-center content; email nudges; human success-team touch for stuck customers. |
| Phase 6 magnitude overruns badly | High | High | Split into Phase 6a (M1–M7, tenancy + SaaS basics) and Phase 6b (M8–M12, ecosystem) — explicit scope-stop points before continuing. |

---

## 6. Cross-cutting Constraints (additions to Phase 1–5)

- **Tenant isolation is non-negotiable.** Every cross-tenant code path is a potential product-ending vulnerability. All development enforces tenant_id at every boundary; pen-test scenarios are CI-gated.
- **SaaS and self-hosted share one codebase.** A feature only available in SaaS is a design smell; lift it to the shared platform unless operationally meaningful.
- **Customer-portal and editor UI is still one web app.** Admin dashboard remains rejected; all web surfaces live in the single SvelteKit app that started in Phase 3.
- **WASM plugins are never trusted.** Capability declaration + broker enforcement + resource limits apply regardless of sign status.
- **Public marketplace trust tiers are explicit.** No tenant accidentally consumes unsigned artifacts; unsigned-artifact installation always requires admin confirmation.
- **Airgap ≠ multi-tenant.** Airgap mode implies single-tenant; enforced at config load.
- **Federation registry is optional.** Static peering from Phase 5 M9 remains a supported path for deployments that don't want to depend on a registry.

---

## 7. Definition of Done (Phase 6)

- [ ] Three tenants operating independently on one instance; pen-test scenarios confirm complete isolation.
- [ ] Self-service customer signup: new customer onboards to first-Watchkeeper within 15 minutes without vendor engineering intervention.
- [ ] Billing cycle runs correctly across three tenants on different plans; invoices delivered; payment processed; quota breaches handled.
- [ ] Vendor-side SOC2 Type II evidence bundle generated; mock auditor accepts.
- [ ] Public marketplace: 10+ published artifacts (templates, tools, plugins) consumed across tenants.
- [ ] WASM plugin from a third-party author installed and active in a tenant; capability enforcement verified.
- [ ] Dynamic federation: two tenants peer via registry; pattern flows; discovery works.
- [ ] Differential-privacy tier: enabled on a federation with measurable utility/privacy trade-off.
- [ ] Phase 6 runbook walked through by a second engineer without assistance.
- [ ] `make phase6-smoke` passes green in CI.

---

## 8. External Prerequisites

- [ ] **Kubernetes cluster (or equivalent orchestrator)** for SaaS cluster deployment. *(before M4)*
- [ ] **Stripe account + tax service** for the vendor. *(before M5)*
- [ ] **Email service** (SendGrid, SES, or equivalent) for customer-portal transactional emails. *(before M6)*
- [ ] **SOC2 audit firm relationship** established. *(before M7 readiness)*
- [ ] **Custom domain + TLS cert management** for SaaS tenants with custom domains. *(before M4 production)*
- [ ] **Moderation staff resourcing** for marketplace (initially vendor staff; community moderators phase in later). *(before M8 public launch)*
- [ ] **Federation registry hosting** (vendor-operated or customer-operated). *(before M10 production)*
- [ ] **External security review** (pen test + SaaS compliance review) before SaaS GA. *(before M12 final)*

---

## 9. Out of Scope — post-Phase-6 (product-direction territory)

Phase 6 completes the business-concept vision. The following are product-direction topics, not planned in this roadmap:

| Item | Reason for post-Phase-6 |
|---|---|
| Vertical specializations (healthcare, finance, legal Watchkeeper packages) | Domain-specific product lines; handled as individual initiatives by respective customer-facing teams. |
| On-device / edge harnesses (mobile agents, IoT Watchkeepers) | Deployment-target expansion; requires its own architecture work. |
| Per-customer model fine-tuning | LLM-infrastructure play; substantial commitment with its own roadmap. |
| Voice / phone messenger adapters | New modality; specialty project. |
| Federated identity across tenants (user in tenant A accesses resources in tenant B) | Non-trivial multi-tenant-identity work; its own future phase if demand emerges. |
| Cross-tenant K2K (agents in different tenants talk without federation) | Rejected — federation is the cross-tenant path. |
| Research into emergent / beyond-reactive agent behaviors | Research track, not productization. |
| Fully decentralized marketplace (no vendor gatekeeper) | Governance + trust model would need a separate design pass; possibly community-driven. |
| Admin dashboard UI | Rejected across all phases; editor-UI hosts every needed surface. |
| HIPAA certification of the vendor SaaS offering | Business-specific; customer responsibility for their own controls; vendor certification is a corporate initiative, not a roadmap feature. |
