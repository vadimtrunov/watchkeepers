# Watchkeeper Phase 5 — Grand Party Implementation Roadmap

**Status**: Planning
**Created**: 2026-04-22
**Scope reference**: [watchkeeper-business-concept.md](./watchkeeper-business-concept.md) (Phase 4 "Grand Party"), [ROADMAP-phase4.md](./ROADMAP-phase4.md)
**Next phase**: [ROADMAP-phase6.md](./ROADMAP-phase6.md)
**Deliverable type**: implementation roadmap — not executable code.

**Progress symbols**: `⬜` not started · `🟨` in progress · `✅` done · `🚫` blocked.

---

## 1. Executive Summary

Phase 4 hardened the platform for enterprise deployment (SSO, HA, encryption, airgap, HIPAA). Phase 5 is the **Grand Party** — the business concept's original Phase 4 theme: Watchkeepers that **anticipate, not just react**. Platform learns across time and across deployments; agents propose their own specialization; vendors can contribute tools to a marketplace.

Success metric (from business concept): **Watchkeepers proactively preventing issues, measurable in incident/delay reduction**. Phase 5 acceptance is a 2-week window in the demo environment where the proactive-prevention metric harness counts demonstrably-prevented issues.

### Framing

Phase 5 has four distinct themes, all foundational to "proactive intelligence":

- **Theme A — Predictive intelligence + proactive intervention.** Detect patterns in Keeper's Log + metrics; propose or invoke preventive actions; track outcomes.
- **Theme B — Extended self-modification / specialization.** Agents iterate their own Manifests across rounds based on outcomes; agents propose spawning specialist siblings when they notice sub-task divergence.
- **Theme C — Tool contribution SDK + third-party marketplace.** Vendors (not just customers) contribute tools into the Tool Registry via documented flow with signing and moderation.
- **Theme D — Cross-org learning via Keep federation.** Anonymized patterns from one deployment improve others through Keep-to-Keep federation — no separate hub service, the mechanism lives inside Keep.

### Deliberately not in Phase 5

- **No new public REST/gRPC API.** Existing Keep + core RPCs are the integration surface. Third parties write tools and Manifests; they don't talk to a new API layer.
- **No runtime-extension SDK.** Third-party runtime plugins (Go code in harness host) → Phase 6. Phase 5 SDK covers only TS-tool contributions.
- **No multi-tenancy.** One instance = one org still. Multi-tenancy → Phase 6.
- **No SaaS.** Self-hosted, federated Keeps as the cross-org mechanism — not a vendor-hosted central service.

### Phase 5 interaction with Phase 4 airgap mode

Cross-org learning (Theme D) **requires** network peer access; it is disabled automatically in airgap mode. All other Phase 5 themes work in airgap (no external deps). This is enforced at config-load time.

---

## 1.1 Status Dashboard

| #   | Milestone                                                              | Status | Magnitude | Notes              |
| --- | ---------------------------------------------------------------------- | ------ | --------- | ------------------ |
| M1  | Pattern detection engine (Watchmaster subscribes to Keep event stream) | ⬜     | 6–9d      |                    |
| M2  | Anomaly detection + alert flow per role                                | ⬜     | 5–7d      |                    |
| M3  | Proactive intervention framework                                       | ⬜     | 6–9d      |                    |
| M4  | Outcome tracking + evolution feedback loop                             | ⬜     | 4–6d      |                    |
| M5  | Multi-round self-tuning (iterative Manifest evolution)                 | ⬜     | 6–8d      |                    |
| M6  | Role bifurcation / specialist spawning                                 | ⬜     | 5–7d      |                    |
| M7  | Tool contribution SDK (docs + examples + vendor onboarding)            | ⬜     | 5–7d      |                    |
| M8  | Third-party tool marketplace (extends Phase 3 M5)                      | ⬜     | 5–7d      | moderation flow    |
| M9  | Keep federation foundation (`cross_org` scope + peer discovery)        | ⬜     | 6–9d      | disabled in airgap |
| M10 | Anonymization pipeline + pattern extraction                            | ⬜     | 6–8d      |                    |
| M11 | Federation back-propagation + opt-in subscription                      | ⬜     | 4–6d      |                    |
| M12 | Phase 5 integration demo + proactive-prevention metric harness         | ⬜     | 6–8d      | acceptance gate    |

Total: ~64–91 days for one team. 12 milestones.

---

## 2. Key Architectural Decisions (additions to Phase 1–4)

| Concern                     | Decision                                                                                                                                                                                                                  | Rationale                                                                                                                              |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| Pattern detection location  | **Watchmaster-side analyzer**, subscribing to Keep's event stream (`keepclient.subscribe`). Not a separate service.                                                                                                       | Watchmaster already "watches the other agents"; pattern detection is a natural extension. Keeps service count stable.                  |
| Proactive interventions     | Every auto-proposal goes through the existing approval flow; no auto-apply without human approval.                                                                                                                        | Consistent with Phase 1/2/3 safety model; humans stay in the loop.                                                                     |
| Self-mod evolution          | Multi-round self-tuning builds on Phase 3 M8 + M10; agent proposes small Manifest increments, outcomes tracked by M4 harness, next round informed by prior outcomes.                                                      | No new approval flow — reuse Phase 2 M2 + Phase 3 M8 paths.                                                                            |
| Role bifurcation            | New Manifest drafted from parent's, modified toward the sub-task; goes through the Phase 1 M7 spawn flow. Parent optionally relinquishes scope to child.                                                                  | Reuses spawn flow; new Manifest is just a new Watchkeeper.                                                                             |
| SDK shape                   | **Tool contribution only** — documented flow, examples, signing (from Phase 2 M8), submission to marketplace. **No new runtime SDK, no new public API.**                                                                  | Matches operator directive; minimizes versioning/deprecation burden.                                                                   |
| Third-party marketplace     | Extends Phase 3 M5 internal marketplace: now accepts vendor-submitted tools with a moderation queue; per-deployment operator opt-in to vendors they trust.                                                                | Internal-only stays default; vendor contributions opt-in per deployment (sub-marketplace model).                                       |
| Cross-org learning location | **Keep federation** — new `scope=cross_org` namespace + peer Keep discovery + Keep-to-Keep sync. **No separate pattern hub service.**                                                                                     | Operator directive — "с киип завязать". Keeps architecture coherent; Keep already has scope-based RLS and replication from Phase 4 M2. |
| Federation trust model      | **Trust groups** — each deployment configures a list of peer Keep endpoints + mutual TLS pinning + per-peer opt-in for push/pull. Federation is peer-to-peer within a trust group, not star-topology vendor-hub.          | Avoids centralizing power with the vendor; federation model matches enterprise trust patterns (corp federations, partner networks).    |
| Anonymization               | **Keep egress layer** — when a row transitions to `scope=cross_org`, it passes through an anonymization pipeline that strips configured-sensitive fields and applies k-anonymity / differential-privacy-style guarantees. | Single choke point for all cross-org data flow; auditable; configurable.                                                               |
| Airgap compatibility        | Cross-org learning explicitly disabled when `airgap=true`. Self-tuning + pattern detection + SDK work fine in airgap.                                                                                                     | Phase 4 airgap guarantee preserved.                                                                                                    |
| Outcome-driven self-mod     | Self-tune proposals that show measurable **negative** outcome after activation are auto-rolled-back within the cooldown window by Watchmaster (still via approval flow — human confirms the auto-rollback proposal).      | Closes the loop between Phase 3 M10 auto-proposals and actual effect.                                                                  |

---

## 3. Scope

### In

- Watchmaster-side pattern detection engine subscribing to Keeper's Log + metrics; detects anomalies, trends, bottlenecks.
- Proactive intervention framework: agents propose preventive actions based on detected patterns; approvals route through existing flow.
- Outcome tracking harness: did the prediction come true? did the intervention work? feedback drives self-mod.
- Multi-round self-tuning: iterative Manifest evolution across multiple approval cycles, each informed by outcomes of the last.
- Role bifurcation: agents can propose spawning a specialist sibling when they detect sub-task specialization opportunity.
- Tool contribution SDK: documentation + examples + submission flow for third-party vendors writing TS tools against the platform registry.
- Third-party tool marketplace: extends Phase 3 M5 internal marketplace with vendor-submitted content, moderation queue, per-deployment vendor opt-in.
- Cross-org learning via Keep federation: `scope=cross_org` namespace, peer Keep discovery, anonymization pipeline, pattern extraction, back-propagation, per-peer opt-in.
- Proactive-prevention metric harness measuring demonstrably-prevented issues over a time window.

### Out (Phase 6+ or rejected)

- Public REST/gRPC API (rejected — use existing Keep + core RPCs).
- Runtime-extension SDK (third-party Go plugins in harness host) → Phase 6.
- Public cross-org marketplace (anyone browses anyone's tools) → Phase 6 with multi-tenancy.
- SaaS multi-tenant deployment → Phase 6.
- Multi-tenancy in single instance → Phase 6.
- RBAC → Phase 6 (only motivated by multi-tenancy).
- Vendor-hosted central pattern hub → rejected; cross-org happens via Keep federation.
- Admin dashboard UI → rejected (consistent with all prior phases).

---

## 4. Milestones

### ⬜ M1 — Pattern detection engine

**Goal**: Watchmaster continuously observes the event stream and metric series, surfaces detected patterns (trends, anomalies, bottlenecks) to an in-process query API other milestones consume.

**Scope**

- [ ] Watchmaster-side `analyzer/` component subscribing to Keeper's Log via `keepclient.subscribe(filter)` — real-time stream + historical window queries.
- [ ] Metric stream ingestion: reads Prometheus metrics + event counts; builds time-series features per Watchkeeper and per role.
- [ ] Pattern types detected (Phase 5 minimum set):
  - Trend detection — linear/exponential growth/decline in a metric.
  - Anomaly detection — simple thresholds + rolling z-score; per-metric configurable sensitivity.
  - Periodicity detection — repeating cycles in event counts.
  - Correlation detection — two metrics moving together (e.g., PR-review-time up with tool-error-rate up).
- [ ] Pattern API: `analyzer.detect(scope, metric, window)` returns detected patterns with confidence scores; consumable by other Watchmaster tools (M2, M3).
- [ ] Back-pressure safe: stream processor drops lowest-value events under load.
- [ ] Keeper's Log events: `pattern_detected`, `anomaly_detected`, `trend_detected`, `correlation_detected` with structured payload.

**Artifacts**: Go `analyzer/` package in Watchmaster scope, pattern-type implementations, API surface for downstream milestones.

**Verification**

- [ ] Seed simulated metric trajectory showing linear growth; trend detected within expected window.
- [ ] Inject anomaly spike; anomaly detected above rolling noise floor.
- [ ] Correlation test: two metrics co-move in seed data; correlation detected, spurious correlations (random noise) not detected.
- [ ] Back-pressure: burst of 100k events; analyzer degrades gracefully, no crash, drops logged with counts.

**Dependencies**: Phase 1 M2 (Keep), Phase 2 M10 (metric harness), Phase 4 M2 (Keep HA makes stream reliable).
**Magnitude**: 6–9 days.

---

### ⬜ M2 — Anomaly detection + alert flow per role

**Goal**: Detected anomalies and trends reach the human lead as contextualized Slack alerts, not as raw metrics.

**Scope**

- [ ] Watchmaster gains a long-running cron job (every N minutes) invoking `analyzer.detect` across configured patterns per Watchkeeper.
- [ ] Alert templates per pattern type: plain-language description of what was detected, which Watchkeeper, relevant metric snapshot, suggested next step ("consider proactive intervention", "review Manifest", "check for runaway lesson").
- [ ] Alert throttling per (Watchkeeper, pattern type): no more than N alerts per day; rollup digest if exceeded.
- [ ] Lead acknowledge flow: Slack buttons `[Investigate] [Ignore] [Silence for 24h]`; silence state stored and honored.
- [ ] Correlation with Phase 3 M10 auto-iteration: if an anomaly suggests self-tune, route through M10's automated iteration; if it suggests intervention, route to M3.
- [ ] Event taxonomy: `alert_raised`, `alert_acknowledged`, `alert_silenced`, `alert_rolled_up`.

**Artifacts**: Watchmaster cron extension, alert templates, throttling logic, silence-state store in Keep.

**Verification**

- [ ] Seed an anomaly; lead receives a contextualized Slack alert within the scheduler's SLA.
- [ ] Throttle test: force 10 anomalies of same type in short window; first N delivered, rest rolled up into a digest.
- [ ] Silence test: `[Silence for 24h]` works; subsequent alerts suppressed; silence expires on schedule; Keeper's Log records.
- [ ] Correlation with M10: anomaly suggesting Manifest regression triggers M10 auto-proposal.

**Dependencies**: M1, Phase 3 M10.
**Magnitude**: 5–7 days.

---

### ⬜ M3 — Proactive intervention framework

**Goal**: Watchkeepers propose preventive actions before detected patterns materialize into incidents; humans approve before any mutation.

**Scope**

- [ ] Built-in tool `propose_intervention(pattern_ref, proposed_action, rationale, expected_outcome)` available to Watchkeepers whose Manifest grants `intervention:propose` capability (off by default, granted per role).
- [ ] Intervention schema: target (what to affect — another Watchkeeper, a tool version, a Manifest field, a Keep knowledge chunk), action (revert, retune, update, send-message-to-lead, escalate), expected measurable outcome.
- [ ] Approval flow: Watchmaster posts the intervention as an approval card with pattern snapshot, proposed action, expected outcome, and reversal plan. Approve/Reject/Modify.
- [ ] Auto-execute on approval: capability broker validates the acting Watchkeeper (or Watchmaster) has the necessary capability; action takes effect.
- [ ] **Intervention audit**: every approved + executed intervention recorded with full context — pattern that triggered it, approver, action, expected outcome, reversal plan. Joins with M4 outcome tracking.
- [ ] Intervention cooldown per Watchkeeper-target pair: no more than N interventions per target per day.
- [ ] Keeper's Log events: `intervention_proposed`, `intervention_approved`, `intervention_rejected`, `intervention_executed`, `intervention_reverted`.

**Artifacts**: `propose_intervention` built-in tool, Watchmaster extensions, intervention-audit schema in Keep.

**Verification**

- [ ] Seed pattern; Watchkeeper with `intervention:propose` proposes an action; Watchmaster posts card; lead approves; action executes; all events audited.
- [ ] Capability enforcement: Watchkeeper without `intervention:propose` cannot call the tool.
- [ ] Cooldown: 11th intervention against the same target within a day refused with informative error.
- [ ] Reversal: each intervention records a reversal plan usable by M4 feedback loop.

**Dependencies**: M1, M2, Phase 1 M6 (capability broker).
**Magnitude**: 6–9 days.

---

### ⬜ M4 — Outcome tracking + evolution feedback loop

**Goal**: Close the loop — did the prediction come true? did the intervention work? feed outcomes into self-tuning evolution.

**Scope**

- [ ] Outcome-tracking schema in Keep: every `pattern_detected`, `intervention_executed`, `self_tune_applied` gets a corresponding `outcome_observation` record at a configurable horizon (N turns, or N hours).
- [ ] Outcome metrics: did the predicted trend continue? did the anomaly resolve? did the intervention produce the expected measurable outcome?
- [ ] Outcome classifier: simple rules (prediction horizon met / prediction failed / inconclusive). No ML in Phase 5; deterministic rules against the metric at the observation horizon.
- [ ] Feedback channel: outcomes feed into Manifest evolution (M5 uses outcome history to inform next self-tune proposal) and into alert-template tuning (false-positive alerts surface for review).
- [ ] Outcome report: weekly Watchmaster-generated summary to admin — "N predictions, X confirmed, Y false, Z inconclusive; M interventions, K successful".
- [ ] Auto-revert: intervention whose outcome is measurably negative triggers an auto-revert proposal at its cooldown window close (lead approves).

**Artifacts**: Keep schema migration for outcome records, outcome-classifier rules package, weekly report generator.

**Verification**

- [ ] Intervention with observable-at-horizon outcome: after horizon, outcome classified correctly (success / fail / inconclusive).
- [ ] Weekly report generated, correctly counts outcomes across pattern types and interventions.
- [ ] Auto-revert: simulated intervention with negative outcome triggers auto-revert proposal within cooldown window.
- [ ] M5 integration: multi-round self-tune next proposal references outcomes of prior rounds in its rationale.

**Dependencies**: M3.
**Magnitude**: 4–6 days.

---

### ⬜ M5 — Multi-round self-tuning (iterative Manifest evolution)

**Goal**: Watchkeepers iterate their own Manifests across rounds, each round informed by outcomes from the previous.

**Scope**

- [ ] Self-tune proposal schema extended: `parent_proposal_id`, `outcome_summary_of_parent`, `iteration_round`. Watchmaster's AI reviewer includes these in the approval card.
- [ ] Harness built-in tool `self_tune_iteration(prior_outcome, increment, rationale)` — proposes a small additional change based on observed outcome of prior round; same approval flow.
- [ ] Evolution depth limit: no more than N iterations from any single seed Manifest without lead-requested "reset" (default N=5; after 5 iterations, lead must explicitly greenlight continuation).
- [ ] Iteration history surfaced to lead via Watchmaster: `manifest.iteration_history(watchkeeper)` — shows the chain of iterations, each round's prior outcome, current state.
- [ ] Abandoned-iteration detection: if an iteration chain shows three consecutive negative outcomes, Watchmaster auto-proposes a revert to an earlier round.
- [ ] Interaction with Phase 3 M8 shadow-compare: raw `system_prompt` edits still use shadow-compare; iteration on `personality`/`language`/`toolset_acls` uses the lighter flow from Phase 2 M2.

**Artifacts**: self-tune schema extension, iteration-history tooling, abandonment heuristic.

**Verification**

- [ ] Agent runs 3 rounds of self-tune iteration; each round's approval card shows prior outcome and current increment rationale.
- [ ] Depth-limit: 6th iteration without reset refused; lead sees "please reset or approve continuation" prompt.
- [ ] Abandoned-iteration: seed 3 consecutive negative outcomes; Watchmaster auto-proposes revert to pre-iteration round.
- [ ] Interaction test: iteration on personality uses light flow; iteration on system_prompt uses shadow-compare flow.

**Dependencies**: M4, Phase 2 M2, Phase 3 M8.
**Magnitude**: 6–8 days.

---

### ⬜ M6 — Role bifurcation / specialist spawning

**Goal**: An agent that notices it's handling two distinct sub-task patterns can propose spawning a specialist sibling to take over one.

**Scope**

- [ ] Pattern detection extension: analyzer detects when one Watchkeeper's workload splits along identifiable axes (e.g., Coordinator handling both "quick-status-check" and "complex-multi-step-coordination" tasks).
- [ ] Built-in tool `propose_bifurcation(sub_task_profile, proposed_specialist_manifest_draft, rationale)` — requires capability `role:bifurcate` (off by default).
- [ ] Draft Manifest generation: Watchmaster-as-reviewer takes parent Manifest + sub-task profile → drafts a specialist Manifest (narrower toolset_acls, specialized personality, same immutable_core except `role_boundaries` narrowed).
- [ ] Approval flow: lead sees parent Manifest + proposed specialist draft + which sub-tasks would migrate to specialist; approve spawns via Phase 1 M7 flow.
- [ ] Post-spawn traffic routing: parent may emit `peer.broadcast` redirects for matching sub-task requests to the new specialist; or the Watchmaster gently reintroduces the specialist in relevant channels.
- [ ] Anti-sprawl metric: `watchkeepers_per_role` tracked; soft-limit alerts when crossing threshold (default 5 active Watchkeepers per role).
- [ ] Keeper's Log: `bifurcation_proposed`, `bifurcation_approved`, `bifurcation_rejected`, `specialist_spawned`, `specialist_traffic_migrated`.

**Artifacts**: analyzer extension for sub-task clustering, `propose_bifurcation` tool, Watchmaster draft-generation, anti-sprawl metric.

**Verification**

- [ ] Seed Coordinator workload with two clearly different sub-task clusters; analyzer detects, Coordinator proposes bifurcation.
- [ ] Lead approves; specialist Watchkeeper spawned with narrower Manifest; Keeper's Log chain visible.
- [ ] Post-spawn, matching sub-tasks route to specialist (observed via correlation IDs).
- [ ] Anti-sprawl: 6th Watchkeeper of same role spawn attempt emits warning; proceeds only with lead confirmation.

**Dependencies**: M1, Phase 1 M7, Phase 3 M5 (template catalog as draft source).
**Magnitude**: 5–7 days.

---

### ⬜ M7 — Tool contribution SDK (docs + examples + vendor onboarding)

**Goal**: Third-party vendors (not only customer operators) can write TS tools, get them signed, and submit to the marketplace. Documented, example-driven, reproducible.

**Scope**

- [ ] SDK documentation site (static site, could be the same SvelteKit editor from Phase 3 M4 with a `/sdk` section, or a separate docs site — operator decides deployment):
  - Tool anatomy: `manifest.json`, `tool.ts`, `tool.test.ts`, capability declarations.
  - Capability dictionary reference.
  - Schema validation rules; common lint failures with fixes.
  - Signing procedure for vendors (extends Phase 2 M8 signing).
  - Testing locally against a mock platform.
  - Submission flow for the marketplace (M8).
- [ ] Reference examples: at least 6 tools covering the main integration shapes (read-only, mutating, webhook-driven, scheduled, K2K-integrated, Keep-reading).
- [ ] Local-dev harness: `wk sdk dev` — spins up a local sandbox where a vendor can iterate on their tool without a full platform deployment. Mocks Keep, messenger, LLM.
- [ ] Test harness for vendor: `wk sdk test <tool-path>` — runs lint + typecheck + capability-declaration check + vitest + signing-dry-run; same gates as the full tool-authoring CI.
- [ ] Vendor onboarding flow: vendor generates a vendor key pair; submits public key for platform verification; signs their tools with their private key; platform accepts per-operator-opt-in (M8).

**Artifacts**: SDK docs site, 6 reference example tools, `wk sdk` CLI subcommand, vendor key-pair generation docs, vendor onboarding page.

**Verification**

- [ ] A third-party (simulated by a teammate who didn't build the platform) follows the SDK docs from zero and ships a signed tool passing local validation within 2 hours.
- [ ] All 6 reference examples pass `wk sdk test` cleanly.
- [ ] Signing dry-run: vendor without a registered public key is refused at submission.

**Dependencies**: Phase 1 M9, Phase 2 M8, Phase 3 M5.
**Magnitude**: 5–7 days.

---

### ⬜ M8 — Third-party tool marketplace

**Goal**: Vendors submit signed tools to a moderation queue; operators opt-in per vendor for their deployment; approved tools become visible in the Phase 3 M5 marketplace UI.

**Scope**

- [ ] Marketplace submission API: vendor POSTs a signed tool bundle with metadata (name, description, tags, vendor key fingerprint). Returns submission ID; enters moderation queue.
- [ ] Moderation dashboard (extension of Manifest editor UI from Phase 3 M4): platform admin (or designated reviewer) sees pending submissions; reviews code + metadata; approves / rejects with reason.
- [ ] Operator-level vendor opt-in: per-deployment config lists trusted vendors by key fingerprint; only tools signed by trusted vendors appear in that deployment's marketplace.
- [ ] Default vendor set: empty. Operators explicitly add vendors. Platform-default vendors (if any — e.g., the platform vendor itself) clearly marked.
- [ ] Marketplace UI extensions (builds on Phase 3 M5):
  - Filter: platform / customer-authored / vendor-authored.
  - Vendor badge + fingerprint on each tool listing.
  - "Adopt this tool" flow: operator picks a vendor tool, spawns a Watchkeeper using it, or adds to an existing Manifest's toolset.
- [ ] Revocation: vendor key revoked → all tools by that vendor flagged; operators receive a warning in Slack; opt-out is one CLI command.
- [ ] Keeper's Log events: `vendor_tool_submitted`, `vendor_tool_moderated`, `vendor_tool_adopted`, `vendor_key_revoked`.

**Artifacts**: submission API, moderation dashboard in editor UI, per-deployment vendor-trust config, marketplace UI filter extensions, revocation flow.

**Verification**

- [ ] Vendor submits a signed tool; moderation queue shows it; admin approves; tool appears in marketplace for deployments that trust the vendor.
- [ ] Deployment not trusting the vendor: tool not visible in that deployment's marketplace.
- [ ] Revocation: revoke vendor key; all their tools flagged within one sync cycle; operator receives Slack warning.
- [ ] Moderation rejection: vendor receives reason; can resubmit.

**Dependencies**: M7, Phase 3 M5.
**External prerequisite**: moderation staff process (documented; could be one platform admin in Phase 5).
**Magnitude**: 5–7 days.

---

### ⬜ M9 — Keep federation foundation (`cross_org` scope + peer discovery)

**Goal**: Keep gains a `scope=cross_org` namespace and the ability to peer with other Keep instances. Deployment opt-in; per-peer opt-in; mutual TLS pinning.

**Scope**

- [ ] Keep schema: `scope ∈ {org, user:<id>, agent:<id>, cross_org}` — new fourth scope. RLS policy: `cross_org` rows readable by the acting agent if Manifest has `federation:read` capability; never writable by agents directly (writes happen via anonymization pipeline in M10).
- [ ] Federation config: list of peer Keep endpoints with mutual TLS certs pinned; per-peer push/pull direction config; trust-group tag.
- [ ] Peer discovery: simple for Phase 5 — static list in config. Dynamic discovery (registry-based) → Phase 6.
- [ ] Federation service inside Keep: periodic sync per-peer; pushes rows in `cross_org` scope to peers who've opted to pull; pulls rows from peers this deployment has opted to follow.
- [ ] Sync integrity: each `cross_org` row carries source-deployment-id (hashed, not human-readable) + signature; receiver verifies; mismatched signature refuses.
- [ ] Airgap guard: if `airgap=true`, federation refuses to start; boot-time error.
- [ ] Per-peer cost metrics: bytes synced in/out, records synced, last-sync time, error counts.
- [ ] Keeper's Log events: `peer_sync_started`, `peer_sync_completed`, `peer_sync_failed`, `cross_org_row_received`, `cross_org_row_sent`.

**Artifacts**: schema migration adding `cross_org`, federation service inside Keep binary, config templates, peer mTLS setup docs.

**Verification**

- [ ] Two test deployments (A and B) configured as peers; A pushes a seeded `cross_org` row; B receives, verifies signature, stores it in its own `cross_org` scope.
- [ ] Mutual TLS: peering attempt without proper certs refused.
- [ ] RLS: agent in B with `federation:read` can query `cross_org` rows from A; agent without capability cannot.
- [ ] Airgap: `airgap=true` + federation config present → refuses to boot with clear error.

**Dependencies**: Phase 1 M2, Phase 4 M2 (Keep HA), Phase 4 M5 (mTLS).
**Magnitude**: 6–9 days.

---

### ⬜ M10 — Anonymization pipeline + pattern extraction

**Goal**: The only path a row can reach `scope=cross_org` is via an anonymization pipeline that strips sensitive fields and applies pattern-level aggregation.

**Scope**

- [ ] **Promotion-to-cross-org is explicit**: an org-scoped pattern becomes cross-org only through a deliberate `promote_to_federation(pattern, anonymization_policy)` action routed through Watchmaster approval — same model as Phase 1 M6 `promote_to_keep`.
- [ ] Anonymization policies (configurable per deployment; strictest by default):
  - Field redaction: list of fields always stripped (person names, emails, URLs, IDs, free-text fields).
  - Pattern abstraction: the row is reduced to an abstract pattern (e.g., "PRs of type X tend to need reviewer Y's skills" becomes "code-review tasks involving [redacted-repo-type] tend to benefit from [redacted-skillset]").
  - K-anonymity guarantee: only patterns observed across K ≥ 5 distinct org-instances (after peer sync) reach back-propagation consumers.
- [ ] **Anonymization-is-Keep-egress-only**: rows cannot bypass the pipeline. Enforced at schema level (insert trigger on `cross_org` rows requires a signed pipeline-execution reference).
- [ ] Pattern extraction tool `promote_pattern(observation_id)` callable by Watchkeepers with `federation:propose` capability; Watchmaster reviews + approves + runs pipeline.
- [ ] Operator-level review before first cross-org push: on first promotion in a deployment, lead reviews not just this pattern but the anonymization policy itself — confirms what's being stripped. Subsequent promotions use the confirmed policy.
- [ ] Per-row audit: `cross_org_row.source_pattern` references the original org-scoped row (kept local, never federated); promotion history traceable.
- [ ] Keeper's Log events: `federation_promotion_proposed`, `federation_promotion_approved`, `federation_promotion_executed`, `anonymization_failed`.

**Artifacts**: anonymization pipeline Go package (in Keep binary), policy config schema, `promote_pattern` tool, insert trigger enforcing pipeline, audit schema for promotions.

**Verification**

- [ ] Direct insert of a `cross_org` row without pipeline reference refused by trigger.
- [ ] Pipeline redaction: row with personal names + emails promoted; resulting `cross_org` row contains none of the original PII.
- [ ] K-anonymity: single-deployment-unique pattern does not reach consumers; pattern observed across 5+ deployments does.
- [ ] First-promotion-in-deployment: lead required to confirm anonymization policy; subsequent promotions skip this confirmation.

**Dependencies**: M9.
**Magnitude**: 6–8 days.

---

### ⬜ M11 — Federation back-propagation + opt-in subscription

**Goal**: Patterns learned across peers flow back into individual deployments as consumable knowledge; deployments opt-in per-peer per-pattern-type.

**Scope**

- [ ] Pattern subscription config: each deployment declares which pattern types it wants to ingest from which peers (trust groups + pattern type filters).
- [ ] Ingestion: pulled `cross_org` rows become queryable by Watchkeepers with `federation:read` capability; surfaced in the same `keep.search` API but with explicit `scope=cross_org` flag.
- [ ] Watchkeeper access: Manifest can declare `federation_access: {pattern_types: [...], peers: [...]}` to scope the read capability further than `federation:read` alone.
- [ ] Applied-pattern tracking: when a local Watchkeeper consumes a cross-org pattern and uses it (e.g., in a decision), the consumption is logged with the cross-org-row reference. Enables later measurement of cross-org value.
- [ ] Reputation signal (Phase 5 simple form): cross-org patterns whose consumption produces good local outcomes (via M4 outcome tracking) get a "useful" mark; patterns never consumed or with negative outcomes fade out of auto-injection.
- [ ] Operator controls: `wk federation peers list | status | pause <peer> | trust <peer> --pattern-types=...`.
- [ ] Phase 5 **does not** auto-apply any cross-org pattern — agent sees them via recall/search, uses them in reasoning, but no blind execution.

**Artifacts**: subscription config, `keep.search` extension for cross-org filter, applied-pattern tracking schema, reputation marking, CLI extensions.

**Verification**

- [ ] Deployment A promotes pattern; Deployment B (subscribed, trust-group-peer) ingests; a Watchkeeper in B recalls it and cites it in its reasoning trace.
- [ ] Subscription filter: deployment subscribed to pattern type X only does not ingest pattern type Y from the same peer.
- [ ] Reputation: pattern consumed + M4 outcome negative → pattern fades from auto-injection within N days.
- [ ] Operator pause: `wk federation pause <peer>` stops sync immediately; resume works.

**Dependencies**: M9, M10, M4.
**Magnitude**: 4–6 days.

---

### ⬜ M12 — Phase 5 integration demo + proactive-prevention metric harness

**Goal**: Acceptance gate. Demonstrate all four themes working together, with the Phase 5 success metric measured.

**Scope**

- [ ] Demo deployment (extends Phase 4 demo): existing Party + a dedicated test scenario that exercises:
  - **Theme A**: anomaly detection on seeded metric trajectory → proactive intervention proposal → lead approves → action → M4 outcome tracking confirms prevention.
  - **Theme B**: multi-round self-tuning on a Watchkeeper across 3 rounds with visible improvement; role bifurcation proposed and approved for a Coordinator noticed to be handling two sub-tasks.
  - **Theme C**: vendor (simulated by teammate) submits a signed tool via SDK; admin moderates and approves; operator opts-in vendor; operator spawns a Watchkeeper using the vendor tool.
  - **Theme D**: two federated deployments (A and B); A promotes anonymized pattern; B ingests; B's Watchkeeper demonstrably uses the pattern in a decision.
- [ ] **Proactive-prevention metric harness**: 2-week window in demo environment.
  - Baseline week: Watchkeepers operate reactively only (intervention capability disabled).
  - Instrumented week: proactive intervention enabled.
  - Metric: count of patterns-that-would-have-become-incidents prevented; count of incidents despite prevention attempt; count of false-positive interventions.
  - Acceptance threshold: at least N > 0 demonstrably-prevented issues over the week, false-positive rate ≤ threshold.
- [ ] `make phase5-smoke` runs condensed versions of all four demonstrations in CI.
- [ ] Runbook appendix: proactive intervention setup, self-mod monitoring, vendor onboarding, federation configuration, airgap-mode caveats.

**Artifacts**: demo scripts, proactive-prevention metric harness, smoke test, runbook additions.

**Verification**

- [ ] All four theme demos run end-to-end without manual intervention.
- [ ] Proactive-prevention metric over 2-week instrumented window exceeds acceptance threshold.
- [ ] `make phase5-smoke` passes green.
- [ ] Second engineer reproduces the demo from runbook without assistance.

**Dependencies**: M1–M11.
**Magnitude**: 6–8 days.

---

## 5. Risks (Phase 5-specific)

| Risk                                                                                  | Likelihood | Impact   | Mitigation                                                                                                                                                                                                                                            |
| ------------------------------------------------------------------------------------- | ---------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pattern detection false positives lead to alert fatigue                               | High       | Med      | Throttling + rollup digest per pattern type; silence-for-24h one click away; weekly outcome report surfaces false-positive rate so tuning is data-driven.                                                                                             |
| Proactive interventions become prescriptive / bossy                                   | Med        | High     | Humans always approve before any mutation; approval card phrases suggestions as proposals not commands; per-lead config can disable auto-alerts entirely.                                                                                             |
| Multi-round self-tune gets stuck in local optimum                                     | Med        | Med      | Depth limit with lead reset requirement; abandoned-iteration auto-revert on 3 consecutive negative outcomes; lead sees full iteration history with outcomes.                                                                                          |
| Role bifurcation causes spawn sprawl                                                  | Med        | Med      | `watchkeepers_per_role` soft-limit + warning; every bifurcation lead-approved; anti-sprawl metric in Grafana.                                                                                                                                         |
| Vendor tool contains malicious code that passes signing + moderation                  | Low        | Critical | Capability declarations enforced at runtime; vendor key revocation one command; marketplace flag + audit; per-deployment trust lists mean only trusted vendors load; signing alone doesn't imply trust.                                               |
| Cross-org anonymization leaks customer IP or PII                                      | Med        | Critical | K-anonymity threshold (default 5); redaction rules + pattern abstraction; first-promotion requires lead to confirm policy; external audit of the anonymization pipeline before first production enablement; per-row audit trail of what was stripped. |
| Federated peer abuses trust (shares bad patterns that influence other deployments)    | Med        | High     | Reputation signal fades bad patterns automatically; per-peer pause; trust-group config explicit; cross-org patterns never auto-applied (agents use them in reasoning, humans still gate actions).                                                     |
| Airgap deployments feel "cut off" from cross-org learning benefits                    | Med        | Low      | Documented: cross-org is optional; airgap-specific alternatives (e.g., vendor delivers pattern packs via signed tarballs) → Phase 6 or operator-requested.                                                                                            |
| SDK vendor onboarding lower-effort than expected → low-quality submissions            | Med        | Med      | Automated gates (same ones as internal tools); moderation queue required before marketplace visibility; rejection-with-reason feedback to vendor.                                                                                                     |
| Outcome tracking horizons miscalibrated (too short → noisy; too long → slow feedback) | Med        | Med      | Per-pattern-type horizon config; weekly report surfaces "outcomes still pending past horizon" as a calibration signal.                                                                                                                                |
| Proactive intervention success hard to attribute (would have happened anyway?)        | Med        | Med      | Baseline vs instrumented windows in demo environment; formal metric harness explicitly models counterfactuals; first-year results treated as directional, not precise.                                                                                |

---

## 6. Cross-cutting Constraints (additions to Phase 1–4)

- **Proactive is opt-in, not automatic.** Every intervention proposal goes through human approval. Phase 5 adds zero auto-execute paths.
- **Cross-org is Keep federation, not a hub.** No vendor-hosted central service. Federation is peer-to-peer within operator-configured trust groups.
- **Anonymization is Keep-egress-only.** Rows cannot reach `scope=cross_org` except through the pipeline with a signed execution reference.
- **Airgap mode excludes federation.** Enforced at config load; federation config in airgap is a boot-time error.
- **No new public API, no new runtime SDK.** Tool contribution SDK only. External integrations use existing Keep + core RPCs.
- **Every self-tune iteration carries its lineage.** Parent proposal, prior outcome, current round — visible in every approval card and in the Manifest history UI.

---

## 7. Definition of Done (Phase 5)

- [ ] Pattern detection engine reliably surfaces trends, anomalies, correlations on the seeded demo dataset.
- [ ] Proactive intervention proposal reaches the lead, gets approved, executes, and outcome is tracked.
- [ ] Multi-round self-tune completes 3 iterations on a test Watchkeeper with documented behavior change.
- [ ] Role bifurcation proposed, approved, specialist spawned, traffic migration observed.
- [ ] Vendor submits a signed tool via SDK; admin moderates and approves; operator adopts it; Watchkeeper uses it in session.
- [ ] Federated deployments A and B: A promotes a pattern; B ingests it; B's Watchkeeper cites it in reasoning trace.
- [ ] Proactive-prevention metric harness runs a 2-week instrumented window and reports N > 0 demonstrably-prevented issues.
- [ ] Airgap mode with federation configured refuses to boot with a clear error.
- [ ] `make phase5-smoke` passes green in CI.
- [ ] Phase 5 runbook walked through by a second engineer without assistance.

---

## 8. External Prerequisites

- [ ] **Second test deployment** configured as a federation peer — for Theme D verification. _(before M9)_
- [ ] **Vendor key pair** generated + signed example tool bundle — for Theme C verification. _(before M7, M8)_
- [ ] **Baseline-window vs instrumented-window metric collection** in the demo environment — 2-week baseline before proactive intervention enabled. _(before M12 final)_
- [ ] **External audit of anonymization pipeline** (external to the implementing team; could be internal security team) — recommended before first production federation enablement. _(before Phase 5 production rollout)_

---

## 9. Out of Scope — deferred to Phase 6 (or rejected)

| Item                                                                   | Target   | Reason                                                                                                                            |
| ---------------------------------------------------------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------- |
| Runtime-extension SDK (third-party Go plugins in harness host)         | Phase 6  | Requires API stability guarantees and plugin-sandboxing; Phase 5 ships only TS-tool contributions.                                |
| Public REST/gRPC API                                                   | Rejected | Existing Keep + core RPCs are the integration surface; new public API would duplicate them.                                       |
| Public cross-org marketplace (anyone browses anyone's tools)           | Phase 6  | Requires multi-tenant trust model; Phase 5 marketplace is per-deployment vendor-opt-in.                                           |
| SaaS multi-tenant deployment, billing, customer onboarding automation  | Phase 6  | Separate operational product.                                                                                                     |
| Multi-tenancy in a single instance                                     | Phase 6  | Phase 5 stays one-instance-one-org.                                                                                               |
| RBAC                                                                   | Phase 6  | Only motivated by multi-tenancy.                                                                                                  |
| Vendor-hosted central pattern hub                                      | Rejected | Keep federation is the architecture; no hub service.                                                                              |
| Dynamic peer discovery (registry-based)                                | Phase 6  | Static config suffices for Phase 5; registry is an ecosystem feature.                                                             |
| Admin dashboard UI                                                     | Rejected | Consistent with all prior phases; editor UI is the only web surface.                                                              |
| Differential-privacy-style anonymization guarantees beyond k-anonymity | Phase 6  | K-anonymity is the Phase 5 bar; stronger guarantees require more research and may degrade pattern utility below useful threshold. |
