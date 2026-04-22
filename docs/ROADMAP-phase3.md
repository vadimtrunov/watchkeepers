# Watchkeeper Phase 3 — Platform Implementation Roadmap

**Status**: Planning
**Created**: 2026-04-22
**Scope reference**: [watchkeeper-business-concept.md](./watchkeeper-business-concept.md) (Phase 3), [ROADMAP-phase1.md](./ROADMAP-phase1.md), [ROADMAP-phase2.md](./ROADMAP-phase2.md)
**Next phase**: [ROADMAP-phase4.md](./ROADMAP-phase4.md)
**Deliverable type**: implementation roadmap — not executable code.

**Progress symbols**: `⬜` not started · `🟨` in progress · `✅` done · `🚫` blocked.

---

## 1. Executive Summary

Phase 1 shipped the core loop (Watchmaster + Coordinator + Keep + Notebook + tool-authoring). Phase 2 grew the Party (K2K + constrained self-tuning + GitHub/Confluence capabilities + role templates). Phase 3 is the **platform phase**: multi-model routing, a web Manifest editor so non-engineers can author roles, a second VCS (GitLab) and a second messenger (Telegram), full self-modification (including `system_prompt`), cross-agent learning, automated prompt iteration, and compliance posture (SOC2-ready audit, GDPR export/delete).

**Framing shift**: Phase 3 is where the platform leaves "works for our pilot" territory and becomes something operators can actually productize. But **Phase 3 is still self-hosted, single-tenant (one instance = one org), no RBAC, no SaaS, no airgap**. Those belong to Phase 4. Phase 3 is about _depth_ — more providers, more integrations, more self-mod — not about enterprise-grade deployability.

Success metric (from business concept): **customers creating custom Watchkeeper roles without engineering support**. The Phase 3 acceptance demo is a non-engineer using the Manifest editor UI to author and spawn a new Watchkeeper end-to-end.

---

## 1.1 Status Dashboard

| #   | Milestone                                           | Status | Magnitude | Notes                                           |
| --- | --------------------------------------------------- | ------ | --------- | ----------------------------------------------- |
| M1  | Multi-model routing foundation                      | ⬜     | 6–9d      | `LLMProvider` router + cost/quality/speed prefs |
| M2  | OpenAI Codex provider                               | ⬜     | 4–6d      |                                                 |
| M3  | Google Gemma provider (self-hosted via vLLM/Ollama) | ⬜     | 5–7d      |                                                 |
| M4  | Manifest editor UI (SvelteKit)                      | ⬜     | 10–14d    | non-engineer role authoring                     |
| M5  | Internal role marketplace / catalog                 | ⬜     | 4–6d      | extends Phase 2 M6                              |
| M6  | GitLab adapter + code-review tool suite             | ⬜     | 5–7d      | symmetric with Phase 2 M4                       |
| M7  | Telegram adapter                                    | ⬜     | 4–6d      | second `MessengerAdapter`                       |
| M8  | Raw `system_prompt` self-edit                       | ⬜     | 6–8d      | highest-risk self-mod; extra gates              |
| M9  | Inter-Watchkeeper lesson sharing                    | ⬜     | 4–6d      | Notebook → Keep promotion for lessons           |
| M10 | Automated prompt iteration                          | ⬜     | 5–7d      | metric-triggered retune/revert proposals        |
| M11 | Compliance: SOC2 audit export + GDPR export/delete  | ⬜     | 6–8d      | log forwarding, data subject APIs               |
| M12 | Phase 3 integration demo + non-engineer acceptance  | ⬜     | 5–7d      | acceptance gate                                 |

Total: ~64–91 days for one team. 12 milestones.

---

## 2. Key Architectural Decisions (additions to Phase 1 + Phase 2)

| Concern                            | Decision                                                                                                                                                                                | Rationale                                                                                                                                    |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| LLM providers                      | `LLMProvider` gains 2 concrete implementations: **Codex** (OpenAI API) and **Gemma** (self-hosted via vLLM or Ollama). Claude Code remains the default.                                 | Phase 1 built the abstraction; Phase 3 cashes it in. Codex covers hosted proprietary breadth; Gemma covers self-hosted/cost-controlled path. |
| Routing                            | Per-task `LLMProvider` selection via Manifest-declared **preferences** (`cost_tier`, `quality_floor`, `speed_target`) + router logic + fallback chains.                                 | Gives operators a simple way to tune cost/quality per role without writing routing code.                                                     |
| Role authoring surface             | **Manifest editor UI** — the one web surface Phase 3 adds. Distinct from a rejected admin dashboard: this composes new Watchkeepers, not monitors existing ones.                        | Phase 3 DoD requires non-engineer role creation. Slack + CLI are not enough for that workflow.                                               |
| UI stack                           | **SvelteKit** — SSR, lightweight, minimal ceremony. Can be swapped per team preference; architecture is framework-agnostic behind the Keep + Watchmaster APIs.                          | Default chosen for lean operator footprint.                                                                                                  |
| Role marketplace                   | **Internal only** in Phase 3 — searchable, versioned, annotated catalog of Manifest templates within a single customer's Keep.                                                          | Public cross-org marketplace → Phase 4 (requires multi-tenant trust model first).                                                            |
| Second VCS                         | **GitLab** added as a platform capability, same tool-suite shape as Phase 2 M4 GitHub.                                                                                                  | Covers large fraction of pilot customers who prefer GitLab for ops / self-hosted reasons.                                                    |
| Second messenger                   | **Telegram** added as second `MessengerAdapter` implementation.                                                                                                                         | Validates the abstraction outside Slack. Simpler protocol than Slack Manifest API. MS Teams / Discord → Phase 4.                             |
| Full self-mod                      | Raw `system_prompt` self-edit enabled with **extra gates**: mandatory shadow-compare against previous version for N turns before auto-activation. Immutable core enforcement unchanged. | Biggest behavioral blast radius of any self-mod; safety net is proportional.                                                                 |
| Cross-agent learning               | Lessons can be **shared across Watchkeepers** in the same role via Keep-mediated approval. Notebook remains per-agent by default.                                                       | Turns accumulated experience into org knowledge; opt-in, auditable.                                                                          |
| Automated prompt iteration         | When instrumented metrics degrade beyond a threshold, Watchmaster auto-proposes a revert or retune. Lead still approves — no auto-apply.                                                | Keeps humans in the loop but removes the "I didn't notice" failure mode.                                                                     |
| Compliance                         | **SOC2-ready audit export** (log forwarding to Splunk/Datadog/SIEM in standardized formats) + **GDPR export/delete** APIs. HIPAA → Phase 4.                                             | Minimum posture for pilot customers in regulated industries.                                                                                 |
| No RBAC                            | Explicitly not built. One instance = one org; operators of that instance are trusted. Access control is filesystem + network, not application-level.                                    | Simplifies the product; avoids YAGNI; per operator feedback.                                                                                 |
| No enterprise hardening in Phase 3 | SSO, HA, encryption-at-rest, multi-tenancy, airgap → Phase 4. Phase 3 remains self-hosted docker-compose.                                                                               | Separates "make it more capable" from "make it enterprise-deployable at scale."                                                              |

---

## 3. Scope

### In

- Multi-model routing: `LLMProvider` router + Codex + Gemma; per-task preference selection; fallback chains; per-provider cost tracking.
- Manifest editor UI (SvelteKit): form-based + YAML-view Manifest composer with schema validation, live capability-declaration linting, preview, and submission through the existing approval flow.
- Internal role marketplace: searchable versioned catalog of Manifest templates within a customer's Keep; lead browses, customizes, spawns.
- GitLab adapter + code-review tool suite (platform tool registry) — symmetric with Phase 2 GitHub.
- Telegram adapter — second `MessengerAdapter` implementation.
- Raw `system_prompt` self-edit with shadow-compare gate + immutable core enforcement + lead approval + rollback via conversational retune.
- Inter-Watchkeeper lesson sharing: agent proposes a Notebook lesson be promoted to a shared-role knowledge channel; approved promotion makes it visible (via Keep, not Notebook) to other agents with the same role.
- Automated prompt iteration: Watchmaster monitors instrumented metrics (e.g., `review_cycle_time`, tool-error rate, lead-complaint signal), auto-proposes revert or retune when thresholds are breached.
- Compliance: SOC2-ready audit export (log forwarding, standard SIEM formats, retention-tagged); GDPR: data subject export, data subject deletion including Notebook entries and Keep records tied to a human identity.
- Phase 3 integration demo including a non-engineer acceptance scenario: non-engineer uses Manifest editor UI to author and spawn a new Watchkeeper without engineer assistance.

### Out (Phase 4+ or rejected)

- SSO (SAML, OIDC) → Phase 4.
- HA / Keep replication → Phase 4.
- Encryption-at-rest guarantees → Phase 4.
- Multi-tenancy (one instance serving multiple orgs) → Phase 4.
- Airgap support (zero external calls, Mattermost/RocketChat adapter, self-hosted LLM as first-class default) → Phase 4.
- SaaS multi-tenant deployment → Phase 4+.
- MS Teams / Discord / Bitbucket adapters → Phase 4.
- Public cross-org role marketplace → Phase 4.
- HIPAA-ready controls → Phase 4.
- Admin dashboard UI → rejected (Phase 2 decision).
- RBAC → rejected.

---

## 4. Milestones

### M1 — Multi-model routing foundation [ ]

**Goal**: Router sits between Watchkeepers and `LLMProvider` implementations; routing decisions driven by Manifest preferences; cost tracked per provider.

**Scope**

- [ ] **M1.1** `LLMRouter` Go component taking a task context (Watchkeeper Manifest + turn metadata) and returning the chosen provider.
- [ ] **M1.2** Manifest schema extension: `llm_preferences` sub-object with `cost_tier ∈ {low, medium, high}`, `quality_floor ∈ {fast, balanced, high}`, `speed_target ∈ {best_effort, low_latency}`, and optional `preferred_provider`.
- [ ] **M1.3** Router policy engine: deterministic selector based on preferences + provider-capability matrix (each `LLMProvider` declares tiers/qualities it can serve). Not ML-based; just rules.
- [ ] **M1.4** Fallback chains: per-task configuration of the ordered provider list; on failure (timeout, rate-limit, error), falls through.
- [ ] **M1.5** Per-provider cost tracking: each invocation's token spend tagged with provider id; rollup in Keep cost tables; Watchmaster `report_cost` gains per-provider breakdown.
- [ ] **M1.6** Provider health: router tracks recent error rates per provider; demotes unhealthy providers until recovered.
- [ ] **M1.7** Backwards compatibility: Manifests without `llm_preferences` route to the default (Claude Code), unchanged behavior.

**Artifacts**: Go `llmrouter/` package, Manifest schema migration, provider-capability registry, cost-tracking extensions.

**Verification**

- [ ] Manifest with `cost_tier=low` routes to the cheapest available provider; `quality_floor=high` routes to the highest-quality one; conflicting preferences resolved by documented precedence.
- [ ] Fallback: primary provider returns 503; router retries on next in chain; task completes.
- [ ] Cost breakdown: `wk budget show` shows per-provider spend across a test run.
- [ ] Unhealthy provider demotion: force error rate > threshold for provider X; router stops selecting X until recovery window elapses.

**Dependencies**: Phase 1 M5 (`LLMProvider` interface).
**Magnitude**: 6–9 days.

---

### M2 — OpenAI Codex provider [ ]

**Goal**: Second `LLMProvider` implementation wrapping the OpenAI API / Codex product.

**Scope**

- [ ] **M2.1** `llm/codex/` Go package implementing `LLMProvider` against OpenAI's API (or Codex agent endpoints, depending on the product shape at implementation time).
- [ ] **M2.2** Credential handling through the secrets interface; no vendor env vars in core.
- [ ] **M2.3** Capability matrix declared: supported task tiers (`balanced`, `high`), latency profile, cost per 1k tokens.
- [ ] **M2.4** Tool-use translation: OpenAI tool-use schema ↔ our Tool Manifest schema; the harness sees the same interface regardless of provider.
- [ ] **M2.5** Streaming support matching the harness JSON-RPC streaming protocol.
- [ ] **M2.6** Integration test against a mock OpenAI API (contract tests) + smoke test against the real API (gated by env credential).

**Artifacts**: `llm/codex/` package, contract tests, cost-matrix entries.

**Verification**

- [ ] Manifest with `preferred_provider=codex` routes to this provider; a simple turn completes end-to-end.
- [ ] Tool-use: an agent invokes a tool through the Codex provider; the tool invocation is identical to what it would be through Claude Code provider.
- [ ] Streaming: completions arrive incrementally in harness; matches existing Claude Code behavior.

**Dependencies**: M1.
**External prerequisite**: OpenAI API key with Codex access.
**Magnitude**: 4–6 days.

---

### M3 — Google Gemma provider (self-hosted inference) [ ]

**Goal**: Third `LLMProvider` implementation running open-weight Gemma models via a self-hosted inference backend (vLLM or Ollama).

**Scope**

- [ ] **M3.1** `llm/gemma/` Go package speaking the OpenAI-compatible endpoint exposed by vLLM and Ollama (both follow the shape).
- [ ] **M3.2** Inference backend selection: docker-compose service (`ollama` or `vllm`) with Gemma model pre-loaded; operator can swap backends via config.
- [ ] **M3.3** Capability matrix: `cost_tier=low` (self-hosted so no per-token cost, just hardware), `quality_floor=fast` or `balanced` depending on Gemma size loaded.
- [ ] **M3.4** Tool-use translation: Gemma via vLLM/Ollama supports function-calling; wrap to our Tool Manifest schema.
- [ ] **M3.5** Performance guidance in runbook: GPU/CPU requirements by model size, expected throughput; fall back to Codex or Claude Code for high-tier tasks if Gemma insufficient.
- [ ] **M3.6** Hardware-check smoke: `wk llm gemma check` reports inference backend health, model loaded, latency sample.

**Artifacts**: `llm/gemma/` package, docker-compose inference service(s), runbook section on self-hosted inference.

**Verification**

- [ ] Manifest with `preferred_provider=gemma` routes here; turn completes using self-hosted inference.
- [ ] Tool-use works; compare results qualitatively to Codex/Claude Code on the same prompt.
- [ ] Backend swap (vLLM ↔ Ollama) works via config without code changes.

**Dependencies**: M1.
**External prerequisite**: inference-capable host (GPU recommended for larger Gemma variants; CPU acceptable for small variants at reduced throughput).
**Magnitude**: 5–7 days.

---

### M4 — Manifest editor UI (SvelteKit) [ ]

**Goal**: Non-engineer can compose a new Watchkeeper Manifest through a web UI and submit it through the existing approval flow — no CLI, no YAML hand-editing required.

**Scope**

- [ ] **M4.1** SvelteKit app in `/editor/` — served as its own docker-compose service; authenticates against Keep via the same capability-token mechanism as CLI.
- [ ] **M4.2** **Form mode** for non-engineers: guided wizard with sections (Role description, Personality / Language, Capabilities, Immutable core, System prompt — scaffolded with templates).
- [ ] **M4.3** **YAML mode** for engineers: raw YAML editor (Monaco) with JSON-schema-backed validation, auto-complete from the capability dictionary, live linting.
- [ ] **M4.4** Toggle between form ↔ YAML; changes round-trip.
- [ ] **M4.5** Template picker: browse role templates from the catalog (M5), pick one, customize from there.
- [ ] **M4.6** Preview panel: shows the effective system prompt composition, list of capabilities granted with human-readable descriptions, estimated per-day token budget from `llm_preferences`.
- [ ] **M4.7** Submit: creates a Manifest version with `status=proposed`, routes through the existing approval flow (slack-native or git-pr per deployment).
- [ ] **M4.8** Browse existing Watchkeepers: list, view Manifest, view version history (read-only; editing goes through the approval flow).
- [ ] **M4.9** Minimal auth: operator logs in with an operator credential issued by the core (same mechanism as `wk` CLI). No RBAC per our Phase 3 decision — anyone with operator access to the box has full access to the editor.

**Artifacts**: `editor/` SvelteKit app, docker-compose service, auth integration, JSON-schema source for Manifest validation.

**Verification**

- [ ] Non-engineer authors a Manifest end-to-end in form mode, submits, approval passes, bot spawns.
- [ ] Engineer switches to YAML mode, edits, switches back to form — no data lost.
- [ ] Invalid capability (not in dictionary) is caught in the UI before submission.
- [ ] Read-only history view correctly renders diffs between versions.

**Dependencies**: Phase 1 M9 (capability dictionary), Phase 2 M3 (Manifest schema with `immutable_core`), Phase 2 M6 (template catalog).
**Magnitude**: 10–14 days.

---

### M5 — Internal role marketplace / catalog [ ]

**Goal**: The Manifest template catalog from Phase 2 M6 becomes a real marketplace surface: searchable, versioned, annotated with usage stats, browsable from the Manifest editor UI.

**Scope**

- [ ] **M5.1** Keep schema extension: `role_template` and `role_template_version` tables; content references templates on disk + metadata (tags, description, author, created_at, used_by_count).
- [ ] **M5.2** Template ingestion: platform-shipped templates (from Phase 2 M6 `templates/manifests/`) indexed on platform upgrade; customer-authored templates can be added via editor.
- [ ] **M5.3** Search: tag-based + full-text over description/prompt/capabilities.
- [ ] **M5.4** Template versioning: every published edit creates a new version; existing Watchkeepers pin to the version they spawned from; updating a template does not auto-mutate live Watchkeepers.
- [ ] **M5.5** Usage stats: increment `used_by_count` on each spawn; surface a "top templates this quarter" view in UI.
- [ ] **M5.6** **Customer-authored templates** stay internal to that customer (public cross-org marketplace → Phase 4).
- [ ] **M5.7** Watchmaster tools updated: `template.search(query)`, `template.show(name, version)`, `template.propose_spawn(name, version, customizations)`.

**Artifacts**: Keep schema migration, template ingestion job, Manifest editor UI integration, Watchmaster extensions.

**Verification**

- [ ] Operator searches "code review" in editor; finds `code-reviewer.yaml` in catalog with description, tags, usage count.
- [ ] Operator customizes template, submits, spawns; new Watchkeeper pinned to the template version spawned from.
- [ ] Template edited and re-published; existing Watchkeepers continue on their pinned version.
- [ ] Usage stats increment correctly across multiple spawns.

**Dependencies**: Phase 2 M6, M4.
**Magnitude**: 4–6 days.

---

### M6 — GitLab adapter + code-review tool suite [ ]

**Goal**: Platform ships GitLab integration, symmetric with Phase 2 M4 GitHub. Operators can spawn code-review-capable Watchkeepers against GitLab repositories.

**Scope**

- [ ] **M6.1** `gitlab/` Go adapter via `go-gitlab` or direct REST; supports GitLab.com and self-hosted GitLab instances (base URL config).
- [ ] **M6.2** Tool suite (platform Tool Registry):
  - `gitlab.list_open_mrs(project, filters)`
  - `gitlab.fetch_mr(project, iid)`
  - `gitlab.post_mr_comment`, `gitlab.post_mr_review_discussion`
  - `gitlab.request_changes`, `gitlab.approve_mr`
  - `gitlab.fetch_pipeline_status`
- [ ] **M6.3** Capabilities: `gitlab:read`, `gitlab:write`, `gitlab:review`.
- [ ] **M6.4** Webhook intake symmetric with GitHub (`gitlab.mr_opened`, etc.).
- [ ] **M6.5** Manifest template `code-reviewer-gitlab.yaml` added to catalog.
- [ ] **M6.6** Rate-limit + circuit-breaker on adapter layer.

**Artifacts**: `gitlab/` adapter, TS tools in platform registry, capability dictionary entries, template, webhook receiver.

**Verification**

- [ ] Manifest using GitLab tools spawned; agent reads a real MR in the test GitLab project and posts a review discussion.
- [ ] Capability enforcement: `gitlab:read`-only agent refused `post_mr_comment`.
- [ ] Self-hosted GitLab: same tests pass against a self-hosted instance via `base_url` config.
- [ ] Webhook: MR opened in test project → `gitlab.mr_opened` event emitted.

**Dependencies**: Phase 1 M9.
**External prerequisite**: GitLab project + access token (GitLab.com or self-hosted).
**Magnitude**: 5–7 days.

---

### M7 — Telegram adapter [ ]

**Goal**: Second `MessengerAdapter` implementation, validating the abstraction outside Slack. Supports bot-user interactions over Telegram Bot API.

**Scope**

- [ ] **M7.1** `messenger/telegram/` Go adapter via `go-telegram-bot-api` or native.
- [ ] **M7.2** `MessengerAdapter` methods:
  - `SendMessage` — DMs and channel posts.
  - `Subscribe` — long-polling or webhook (both configurable; long-polling default for airgap-friendliness).
  - `CreateApp` — Telegram's model is different (one bot per Watchkeeper requires BotFather interaction); Phase 3 documents manual bot creation + `/register_bot` flow in operator runbook. Future full automation via BotFather scripting → Phase 4.
  - `SetBotProfile` — `setMyName`, `setMyDescription`, `setMyCommands`.
  - `LookupUser` — Telegram user IDs.
- [ ] **M7.3** Human identity mapping: `human` row keyed by Telegram user ID (separate column from Slack user ID).
- [ ] **M7.4** Command structure: Telegram's `/command` UX mapped to Watchmaster's DM-chat pattern.
- [ ] **M7.5** K2K over Telegram: supported via bot-to-bot private groups; escalation to lead via the lead's DM.
- [ ] **M7.6** Runbook: Telegram-specific workspace bootstrap, token management, rate-limit guidance.

**Artifacts**: `messenger/telegram/` adapter, runbook section, human-identity schema update.

**Verification**

- [ ] A Watchkeeper spawned against Telegram: operator issues `/spawn coordinator` to Watchmaster bot; new Watchkeeper bot appears, responds to DMs.
- [ ] K2K test: two bots in a private Telegram group exchange messages; escalation to lead's DM triggers on budget breach.
- [ ] Cross-messenger check: the same logical Watchkeeper can be exposed to both Slack and Telegram if the deployment requires it (out of Phase 3; note as Phase 4 if demand arises).

**Dependencies**: Phase 1 M4 (Messenger adapter interface).
**External prerequisite**: Telegram bot token (per Watchkeeper; generated via BotFather).
**Magnitude**: 4–6 days.

---

### M8 — Raw `system_prompt` self-edit [ ]

**Goal**: Remove the Phase 2 restriction on `system_prompt` self-tuning. Agent can propose edits to its own `system_prompt` with extra gates beyond what Phase 2 M2 provides.

**Scope**

- [ ] **M8.1** Extend `self_tune` built-in tool: now accepts `field=system_prompt` (previously refused at schema layer).
- [ ] **M8.2** **Extra gate: shadow-compare window**. On approval of a `system_prompt` change, the new Manifest does **not** auto-activate. It enters a `shadow_pending` state. For N turns (default 20), the old Manifest stays active and produces the real responses; the new Manifest runs in shadow (receives the same inputs, its responses logged but not sent). Watchmaster compares old vs. shadow each turn: tool call diff, length diff, semantic similarity score from an LLM judge. At the end of the window, Watchmaster posts a comparison report to the lead with per-turn diffs; lead either promotes to active or rejects. Default reject if lead silent for M hours.
- [ ] **M8.3** Immutable core enforcement unchanged — even `system_prompt` edits cannot subvert boundaries declared in `immutable_core.role_boundaries` / `security_constraints` / `escalation_protocols`.
- [ ] **M8.4** Self-tuning quota in M2 applies; `system_prompt` edits count more heavily (default: cost 3 of the daily quota of 3).
- [ ] **M8.5** Rollback via conversational retune (Phase 2 M3) works transparently — rollback creates a new Manifest pinned to a previous version.

**Artifacts**: `self_tune` schema extension, shadow-compare runtime, comparison-report templates, approval-card extensions.

**Verification**

- [ ] Agent proposes `system_prompt` edit; approval card shows the diff; lead approves; Manifest enters `shadow_pending`.
- [ ] During shadow window: operator turns 1..20; per-turn diffs logged; comparison report posted to lead at window close.
- [ ] Lead promotes: new Manifest active; behavior change observable.
- [ ] Lead rejects or times out: `shadow_pending` Manifest discarded; old Manifest remains.
- [ ] Immutable core violation attempt (system_prompt tries to override a role_boundaries constraint): validator refusal.

**Dependencies**: Phase 2 M2, Phase 2 M3.
**Magnitude**: 6–8 days.

---

### M9 — Inter-Watchkeeper lesson sharing [ ]

**Goal**: A Notebook lesson can be **promoted to a shared-role knowledge channel** in Keep, making it visible to other Watchkeepers in the same role. Notebooks remain per-agent by default; sharing is always opt-in and audited.

**Scope**

- [ ] **M9.1** Keep schema: `shared_lesson` table scoped by `role_id`; contains lesson content, source agent, promotion approver, `active_after`, `evidence_log_ref`.
- [ ] **M9.2** Built-in tool `propose_lesson_share(lesson_id)` available to every Watchkeeper. Routes through Watchmaster approval (lead sees the lesson, approves sharing).
- [ ] **M9.3** Harness auto-recall extension: on each turn, harness recalls top-K from Notebook **and** top-K from `shared_lesson` for this agent's role. Shared lessons have their own weight.
- [ ] **M9.4** Shared-lesson lifecycle: lead or admin can `forget` a shared lesson with mandatory reason; `forget` propagates (subsequent retrievals exclude it).
- [ ] **M9.5** Coordinator's daily briefing extended to include a "new shared lessons (past 24h) for your team's roles" section.
- [ ] **M9.6** Shared lessons are **not** promoted to Keep as business knowledge — they remain in the role-scoped namespace. Existing Notebook → Keep promotion (Phase 1 M6 `promote_to_keep`) stays for distinct use case (personal observation → org knowledge).

**Artifacts**: Keep schema extension, `propose_lesson_share` tool, harness recall extension, approval card template.

**Verification**

- [ ] Agent A proposes sharing a lesson; Watchmaster posts approval card to lead; approved → lesson visible to agent B (same role) via auto-recall on next turn.
- [ ] Agent A and Agent B in different roles: lesson shared by A does NOT surface to B.
- [ ] `forget` on shared lesson propagates: subsequent recalls in all agents of that role exclude it.
- [ ] Daily briefing lists new shared lessons for the lead's teams.

**Dependencies**: Phase 1 M2b (Notebook), Phase 1 M6 (Watchmaster approval flow), Phase 2 M1 (K2K for bot-to-bot interactions).
**Magnitude**: 4–6 days.

---

### M10 — Automated prompt iteration [ ]

**Goal**: When instrumented metrics degrade beyond threshold, Watchmaster auto-proposes a retune or revert. Lead always approves — no auto-apply.

**Scope**

- [ ] **M10.1** Metric-watcher service in Watchmaster scope: subscribes to per-Watchkeeper metrics emitted by the metric harness (Phase 2 M10) — `review_cycle_time`, `tool_error_rate`, `lead_complaint_signal` (explicit `/complaint` slash command), `token_spend_vs_quota`.
- [ ] **M10.2** Threshold config in Manifest (or global defaults): per-metric threshold with direction (e.g., `tool_error_rate > 0.15` for 6 hours).
- [ ] **M10.3** On threshold breach: Watchmaster auto-proposes either (a) revert to the last known-good Manifest version (if the current version is recent), or (b) a specific retune targeting the suspected cause (e.g., high error rate → propose tightening `personality` toward more conservative behavior).
- [ ] **M10.4** Auto-proposal goes through normal approval flow — lead sees a card with: metric breach context (chart snapshot), proposed change, rationale. Approve / Reject / Modify buttons.
- [ ] **M10.5** Auto-proposal cooldown: per Watchkeeper, no more than one auto-proposal per N hours (default 24) to prevent proposal storms.
- [ ] **M10.6** `auto_iteration_proposed` and related events in Keeper's Log.

**Artifacts**: Watchmaster metric-watcher extension, proposal templates, approval-card variant.

**Verification**

- [ ] Synthetic metric breach: inject `tool_error_rate=0.3` for test Watchkeeper; within the watcher cycle, auto-proposal appears in the lead's queue.
- [ ] Approve → revert / retune applied; Watchkeeper's behavior shifts.
- [ ] Cooldown: second synthetic breach within cooldown window does not trigger a second proposal.
- [ ] Lead override: modify the proposed change before approving; modified version applies.

**Dependencies**: Phase 2 M10 (metric harness), Phase 2 M2 (self-tuning).
**Magnitude**: 5–7 days.

---

### M11 — Compliance: SOC2-ready audit + GDPR export/delete [ ]

**Goal**: Minimum compliance posture for pilot customers in regulated contexts. SOC2-ready audit forwarding and GDPR data-subject APIs.

**Scope**

- [ ] **M11.1** **Audit forwarding**: pluggable `AuditSink` interface with implementations for: `stdout-json` (default), `syslog`, `splunk-hec`, `datadog-logs`. Every Keeper's Log entry mirrored to the configured sink with retention-friendly tags (`event_type`, `actor`, `correlation_id`, `severity`).
- [ ] **M11.2** **Retention policy enforcement**: `keepers_log` retention config already in Phase 2 M9; extended here with per-event-type retention (e.g., `auth_event` retained 7y, `debug_event` 30d) as required by SOC2.
- [ ] **M11.3** **GDPR export API**: `POST /gdpr/export` takes a human identity (email / Slack user ID / Telegram user ID); returns a bundle containing: all Keep rows referencing that human, all Manifest versions they approved, all Notebook entries authored in their conversations, all Keeper's Log entries mentioning them. Cryptographic manifest for verifiability.
- [ ] **M11.4** **GDPR delete API**: `POST /gdpr/delete` takes a human identity; purges all personally-identifying fields across Keep tables; tombstones Keeper's Log entries (keeps audit record but replaces payload with hash); archives Notebooks tied to that human's conversations. Immutable `keepers_log.event_type=gdpr_delete_executed` with the purge manifest.
- [ ] **M11.5** CLI: `wk gdpr export <id> --out <bundle>`, `wk gdpr delete <id> --confirm`.
- [ ] **M11.6** Runbook: SOC2 audit trail overview, retention configuration, GDPR workflow, sample audit questions with pointers to evidence.

**Artifacts**: `auditsink/` package with 4 implementations, GDPR API (HTTP), CLI extensions, runbook sections.

**Verification**

- [ ] Configure `datadog-logs` sink; Keeper's Log events appear in Datadog within seconds.
- [ ] Retention: event older than configured retention is dropped from hot storage + moved to cold per Phase 2 M9.
- [ ] GDPR export bundle is complete, self-contained, cryptographically verifiable.
- [ ] GDPR delete tombstones Keeper's Log without breaking `log_tail`; tombstones show correctly; post-delete query for that human's name returns zero rows.
- [ ] Dry-run mode for `gdpr delete`: reports what would be deleted without executing.

**Dependencies**: Phase 1 M2 (Keep schema), Phase 2 M9 (retention).
**External prerequisite**: target audit sink endpoint (if forwarding); test human identity with seed data.
**Magnitude**: 6–8 days.

---

### M12 — Phase 3 integration demo + non-engineer acceptance [ ]

**Goal**: Acceptance gate. Demonstrate Phase 3 end-to-end, including the key success metric: a non-engineer authors and spawns a new Watchkeeper using only the Manifest editor UI.

**Scope**

- [ ] **M12.1** Demo deployment: existing Phase 2 Party (Coordinator + GitHub-capable + Confluence-capable) **plus**:
  - A Watchkeeper using the Codex provider (high-quality tier).
  - A Watchkeeper using the Gemma self-hosted provider (cost-controlled tier).
  - A GitLab-capable Watchkeeper pointed at a test GitLab project.
  - A Watchkeeper exposed over Telegram rather than Slack.
- [ ] **M12.2** **Non-engineer acceptance scenario** (the core success metric):
  - Non-engineer (simulated by an engineer following a script designed for a non-engineer) logs into Manifest editor UI, picks a template, customizes via form mode, submits, lead approves, Watchkeeper goes live. No CLI, no YAML editing, no code.
- [ ] **M12.3** Scripted demonstration of each new Phase 3 capability in sequence:
  - `llm_preferences` routing: two Watchkeepers with different cost tiers visibly use different providers.
  - System_prompt self-edit with shadow-compare: trigger, show 20-turn shadow, show comparison report, lead approves.
  - Inter-Watchkeeper lesson sharing: agent proposes share, approves, second agent in same role uses the lesson in a subsequent turn.
  - Automated prompt iteration: synthetic metric breach → auto-proposal → lead approves revert.
  - GDPR export + delete: export a seeded human's data, then delete, re-export returns empty bundle.
- [ ] **M12.4** Phase 3 smoke test: `make phase3-smoke` runs a condensed version of the above in CI.
- [ ] **M12.5** Runbook appendix: everything Phase 3-specific — multi-model routing config, Gemma self-hosted inference ops, Telegram workspace setup, Manifest editor UI admin, compliance evidence collection.

**Artifacts**: demo scripts, non-engineer acceptance script, Phase 3 smoke test, runbook additions.

**Verification**

- [ ] Non-engineer scenario completes without the operator touching CLI or editing YAML.
- [ ] All Phase 3 capabilities demonstrated in the scripted run.
- [ ] `make phase3-smoke` passes green in CI.
- [ ] Second engineer reproduces the demo + non-engineer scenario from the runbook without assistance.

**Dependencies**: M1–M11.
**Magnitude**: 5–7 days.

---

## 5. Risks (Phase 3-specific)

| Risk                                                                                            | Likelihood | Impact   | Mitigation                                                                                                                                                                                                                     |
| ----------------------------------------------------------------------------------------------- | ---------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Non-engineer Manifest editor produces broken Manifests                                          | High       | Med      | Form mode strongly guided; schema validation + capability-dictionary lookups block submission; preview panel shows effective composition; rejection by AI reviewer before reaching lead includes a human-readable explanation. |
| Router picks wrong provider for a task (quality regression)                                     | Med        | Med      | Per-Watchkeeper `preferred_provider` override; metric-watcher catches degradation and auto-proposes; operator can pin provider in Manifest.                                                                                    |
| Self-hosted Gemma inference under-provisioned, high latency                                     | Med        | Med      | Runbook hardware guidance; `wk llm gemma check` surfaces latency; router demotes Gemma on sustained latency; fallback chain moves traffic to cloud providers.                                                                  |
| `system_prompt` self-edit promotes a version that regresses in ways shadow-compare didn't catch | Med        | High     | Shadow-compare is time-bounded; post-promotion monitoring via automated prompt iteration (M10) catches regressions within hours; conversational rollback is one sentence.                                                      |
| Telegram rate limits or spam filters throttle bots at scale                                     | Med        | Med      | Adapter-level rate limiting; message batching for fan-out; operator runbook documents Telegram Business API option for high-volume deployments.                                                                                |
| GitLab self-hosted auth variety breaks adapter                                                  | Med        | Med      | Adapter tested against GitLab.com and a self-hosted reference instance; auth flow pluggable (token, OAuth, CI-token); documented matrix of tested versions.                                                                    |
| GDPR delete breaks Keeper's Log append-only invariant                                           | Low        | Critical | Tombstoning preserves the row; payload replaced with hash + purge-manifest reference; verified by audit tests.                                                                                                                 |
| Inter-Watchkeeper lesson sharing amplifies a false lesson                                       | Med        | Med      | Sharing is always lead-approved; cooling-off window inherited from source Notebook entry; `forget` propagates across all recipients.                                                                                           |
| Role marketplace fills with low-quality customer-authored templates                             | Med        | Low      | Internal-only in Phase 3 (no cross-org exposure); editor-side lint requires description + tags; usage stats surface which templates are actually productive.                                                                   |
| Automated prompt iteration proposal storm overwhelms lead                                       | Med        | Med      | Per-Watchkeeper cooldown (default 24h); grouping of related proposals in a single approval card; lead can set threshold globally in config.                                                                                    |
| Manifest editor UI auth loose → anyone with network access takes over the box                   | Med        | High     | Editor auth mirrors CLI's capability-token mechanism; no public-exposure default; runbook mandates reverse proxy with network controls; `wk editor expose` command defaults to localhost-only.                                 |
| SOC2 audit retention configuration drift between hot and cold storage                           | Med        | Med      | `keepers_log` retention enforced by Keep; audit sink mirrors retention tags; periodic retention-audit job reports gaps.                                                                                                        |

---

## 6. Cross-cutting Constraints (additions to Phase 1 + 2)

- **No RBAC in Phase 3.** One instance = one org; operators of that instance are trusted. Access control is at the network and filesystem layer, not application layer. Any proposal to add role-based access control is pushed to Phase 4 (where multi-tenancy motivates it).
- **No admin dashboard.** The Manifest editor UI exists because it does something CLI + Slack cannot (form-driven role authoring for non-engineers). Any proposal to add monitoring/observability UI is rejected — Grafana + Keeper's Log + CLI still cover that.
- **Self-hosted only.** Phase 3 deployments are docker-compose. SaaS multi-tenant is Phase 4+.
- **LLM access through `LLMRouter`.** Watchkeepers never name a specific provider in their code or prompts; they declare preferences and the router picks.
- **Shadow-compare is the extra gate for `system_prompt` edits.** No other self-tuning field needs shadow-compare; raw `system_prompt` is the only one with high enough blast radius to justify the overhead.
- **Sharing is always opt-in and auditable.** Neither inter-Watchkeeper lesson sharing nor Notebook → Keep promotion happens without explicit lead approval; every promotion has an immutable audit trail.

---

## 7. Definition of Done (Phase 3)

- [ ] Multi-model routing live: at least 3 providers (Claude Code, Codex, Gemma) selectable by `llm_preferences`; fallback chains tested; per-provider cost tracked.
- [ ] Manifest editor UI live; non-engineer authors a complete Manifest end-to-end in form mode; Watchkeeper spawned; no CLI or YAML touched.
- [ ] Role marketplace populated with at least 4 platform-shipped templates + 1 customer-authored template; search works; version pinning works.
- [ ] GitLab-capable Watchkeeper reviews a real MR in the test GitLab project.
- [ ] Telegram-exposed Watchkeeper responds to DMs and handles a K2K conversation.
- [ ] A Watchkeeper successfully completes a `system_prompt` self-edit cycle: propose → approve → shadow-compare (20 turns) → comparison report → lead promotes → new version active.
- [ ] Inter-Watchkeeper lesson sharing works: agent A promotes a lesson; lead approves; agent B (same role) uses the shared lesson on its next turn (observed in prompt inspection).
- [ ] Automated prompt iteration triggers on synthetic metric breach; lead receives a well-formed auto-proposal; approval applies the change.
- [ ] SOC2-ready audit forwarding works against at least one real sink (Datadog/Splunk/equivalent); retention policies enforced.
- [ ] GDPR export returns a complete, verifiable bundle for a seeded human; delete purges that human's PII + tombstones Keeper's Log correctly.
- [ ] `make phase3-smoke` passes in CI.
- [ ] Phase 3 runbook walked through by a second engineer without assistance.

---

## 8. External Prerequisites

- [ ] **OpenAI API key** with access to Codex or suitable OpenAI models. _(before M2)_
- [ ] **Inference-capable host** for Gemma (GPU recommended; CPU acceptable at small model sizes with reduced throughput). _(before M3)_
- [ ] **Signing certificate or domain** for Manifest editor UI if exposed beyond localhost. _(before M4 production)_
- [ ] **GitLab project** (GitLab.com or self-hosted) with access token; sample MR seeded. _(before M6)_
- [ ] **Telegram bot token(s)** generated via BotFather — one per Watchkeeper that will be exposed on Telegram. _(before M7)_
- [ ] **Audit sink endpoint** (Datadog, Splunk HEC, or equivalent) for SOC2-ready forwarding. _(before M11 production)_
- [ ] **Test human identity with seed data** across Keep + Notebook + Keeper's Log for GDPR export/delete verification. _(before M11 verification)_
- [ ] **Baseline + instrumented metric windows** for the Phase 3 demo's acceptance scenarios. _(before M12 final)_

---

## 9. Out of Scope — deferred to Phase 4 and beyond (or rejected)

| Item                                            | Target             | Reason                                                                                                            |
| ----------------------------------------------- | ------------------ | ----------------------------------------------------------------------------------------------------------------- |
| SSO (SAML / OIDC)                               | Phase 4            | Enterprise hardening bucket; one-instance-one-org makes it low-priority for Phase 3.                              |
| HA / Keep replication                           | Phase 4            | docker-compose remains Phase 3's target; HA is enterprise-deployability work.                                     |
| Encryption-at-rest guarantees                   | Phase 4            | Same bucket as SSO/HA — enterprise hardening.                                                                     |
| Multi-tenancy (one instance, many orgs)         | Phase 4            | Phase 3 stays one-instance-one-org per operator.                                                                  |
| SaaS multi-tenant deployment                    | Phase 4+           | Enterprise hardening must land first; SaaS brings its own ops concerns.                                           |
| Airgap support                                  | Phase 4            | Requires self-hosted LLM as first-class, Mattermost/RocketChat messenger adapter, zero obligatory external calls. |
| MS Teams, Discord, Bitbucket adapters           | Phase 4            | GitLab + Telegram cover Phase 3's pilot demand.                                                                   |
| Public cross-org role marketplace               | Phase 4            | Needs multi-tenant trust model first.                                                                             |
| HIPAA-ready controls                            | Phase 4            | Healthcare-specific compliance work; beyond Phase 3 scope.                                                        |
| Admin dashboard UI                              | Rejected           | CLI + Slack + Grafana + Keeper's Log still cover monitoring. Manifest editor UI is the only web surface.          |
| RBAC                                            | Rejected (Phase 3) | One-instance-one-org; operators are trusted. May revisit in Phase 4 under multi-tenancy.                          |
| Proactive intelligence / predictive analytics   | Phase 4            | Explicitly Phase 4 in the business concept.                                                                       |
| Cross-org learning (anonymized pattern sharing) | Phase 4            | Phase 4 per business concept.                                                                                     |
