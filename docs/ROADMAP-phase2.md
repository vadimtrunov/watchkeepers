# Watchkeeper Phase 2 — Party Grows Implementation Roadmap

**Status**: Planning
**Created**: 2026-04-22
**Scope reference**: [watchkeeper-business-concept.md](./watchkeeper-business-concept.md) (Phase 2), [ROADMAP-phase1.md](./ROADMAP-phase1.md)
**Next phase**: [ROADMAP-phase3.md](./ROADMAP-phase3.md)
**Deliverable type**: implementation roadmap — not executable code.

**Progress symbols**: `⬜` not started · `🟨` in progress · `✅` done · `🚫` blocked.

---

## 1. Executive Summary

Phase 1 shipped a minimal Party: Watchmaster + one role-specialized Watchkeeper (Coordinator), with Keep + Notebook + Tool Registry + tool-authoring self-modification. Phase 2 grows the Party: multiple agents live and interact, the platform ships the capabilities needed to compose any role, and agents can tune their own personality and toolset within a mechanically-enforced safety boundary.

Success metric (from the business concept): **3–5 Watchkeepers working in concert + measurable reduction in review cycle time over a 2-week baseline-vs-instrumented window**.

### The framing shift

Phase 2 does **not** build "roles" into the platform. Platform ships **capabilities** (GitHub adapter, Confluence adapter, doc-sync tools, code-review tools, Keeper-to-Keeper messaging). Operators compose roles by writing Manifests that combine these capabilities. "Code Reviewer", "Tech Writer", "PM Assistant" exist as **Manifest templates in a catalog**, not as milestones in core. Adding a new role in Phase 2+ is operator work, not platform work.

### The safety model

Phase 2 introduces self-tuning. To stay safe without shadow-mode or regression tests, the model is:

- **Immutable core** — a structured portion of every Manifest that is mechanically un-modifiable by the agent or by lead-approved self-tuning. Only a platform admin can edit it.
- **Version history** — every Manifest has a full trail; nothing is lost on a self-tune.
- **Conversational retune** — if the lead feels a Watchkeeper "was better last Tuesday", they say so to the Watchmaster in plain language; Watchmaster reads history, proposes a rollback, lead approves, rolled back.

That's the entire safety net. No formal behavioral regression tests. No dual-runtime shadow deployments. Simplicity is the feature.

---

## 1.1 Status Dashboard

| #   | Milestone                                              | Status | Magnitude | Notes                           |
| --- | ------------------------------------------------------ | ------ | --------- | ------------------------------- |
| M1  | Keeper-to-Keeper communication                         | ⬜     | 7–10d     |                                 |
| M2  | Constrained prompt self-tuning                         | ⬜     | 5–7d      | depends on M3                   |
| M3  | Immutable core + state history + conversational retune | ⬜     | 3–5d      | unblocks M2                     |
| M4  | GitHub adapter + code-review tool suite                | ⬜     | 7–10d     | requires GitHub credentials     |
| M5  | Confluence adapter + doc-sync tool suite               | ⬜     | 6–8d      | requires Confluence credentials |
| M6  | Manifest template catalog                              | ⬜     | 2–3d      |                                 |
| M7  | Notebook Phase 2 upgrades                              | ⬜     | 5–7d      |                                 |
| M8  | Tool signing mandatory                                 | ⬜     | 3–5d      |                                 |
| M9  | Keep backup/restore automation + retention             | ⬜     | 3–5d      |                                 |
| M10 | Phase 2 integration demo + metric harness              | ⬜     | 5–7d      | acceptance gate                 |

Total: ~46–67 days for one team. Milestones within Phase 2 use the same `M#` naming as Phase 1 — disambiguate by file when cross-referencing (e.g. "Phase 2 M4").

---

## 2. Key Architectural Decisions (additions to Phase 1)

| Concern                      | Decision                                                                                                                                                                                                                         | Rationale                                                                                             |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| Roles                        | **Not a platform concept.** Platform ships capabilities (adapters + tools); operators compose roles via Manifests using those capabilities.                                                                                      | Keeps the core lean; adding a new role is configuration, not a code change.                           |
| Role catalog                 | A **template library** of reference Manifests (`templates/manifests/`), copy-and-customize. Not runtime-hardcoded.                                                                                                               | Useful starting points without baking roles into the binary.                                          |
| Immutable core of a Manifest | Explicit `immutable_core` sub-object with 5 buckets: `role_boundaries`, `security_constraints`, `escalation_protocols`, `cost_limits`, `audit_requirements`. Self-tuning validators reject any change to it at the schema layer. | Turns the invariant from the business concept into mechanics, not convention.                         |
| Safety model for self-tuning | Immutable core + Manifest version history + operator-led conversational rollback via Watchmaster. No shadow-mode. No behavioral regression tests.                                                                                | Simpler; matches natural human oversight; relies on mechanisms already designed rather than new ones. |
| Self-tuning scope            | Constrained: `personality`, `language`, `toolset_acls`. Not `system_prompt` (raw). Not `immutable_core`. Raw `system_prompt` self-edits → Phase 3.                                                                               | Phase 2 risk surface stays narrow.                                                                    |
| K2K channels                 | Bots communicate through dedicated Slack private channels per conversation; invisible to humans unless they opt in.                                                                                                              | Matches business-concept "Keeper-to-Keeper traffic... invisible to humans unless they opt in."        |
| Escalation chain             | Peer → human lead → Watchmaster. Token-budget exceed auto-escalates to the human lead.                                                                                                                                           | Mechanical safety valve against runaway dialogues.                                                    |
| K2K capability gating        | K2K never lends capability. When agent A asks B to do X, B needs its own capability for X in its Manifest.                                                                                                                       | Prevents capability laundering through K2K.                                                           |
| Tool signing                 | Mandatory for `built-in`, `platform`, `private:git`, `private:hosted`. Only `local` may remain unsigned (loud audit).                                                                                                            | Phase 1 was optional; Phase 2 raises the bar now that multi-agent churn is a reality.                 |
| Messenger surface            | Slack-only in Phase 2 (no new adapters).                                                                                                                                                                                         | All effort on depth, not breadth. Additional messengers → Phase 3.                                    |
| Admin dashboard UI           | **Not built.** CLI + Slack-native + Grafana + Keeper's Log cover every operator and lead need.                                                                                                                                   | Rejected as duplicating existing surfaces.                                                            |

---

## 3. Scope

### In

- Keeper-to-Keeper messaging infrastructure (dedicated channels, tool suite, event subscription, escalation chain, token-budget enforcement).
- Constrained prompt self-tuning via the Phase 1 self-modification approval flow.
- Explicit `immutable_core` Manifest field with mechanical enforcement.
- Manifest version-history introspection and conversational rollback via Watchmaster.
- GitHub adapter and a code-review-oriented tool suite (platform tool registry), usable by any Manifest.
- Confluence adapter and a doc-sync-oriented tool suite (platform tool registry), usable by any Manifest.
- Manifest template catalog (`templates/manifests/`) with reference Manifests for common roles (code-reviewer, tech-writer, pm-assistant, incident-responder); operators copy and customize.
- Notebook Phase 2 upgrades: auto-inheritance on retire→spawn, reflection sampling on tool success, lesson consolidation, personality preset catalog (structured fields).
- Tool signing made mandatory for all non-local sources.
- Keep backup/restore automated; `keepers_log` cold-storage rotation; retention enforced; restore drill.
- Phase 2 integration demo with 3 Watchkeepers in concert; `review_cycle_time` metric harness over a 2-week window.

### Out (Phase 3+ or rejected)

- Additional messengers: MS Teams, Telegram, Discord (Phase 3).
- Additional built-in roles as milestones — all future roles are Manifest templates, never platform scope.
- GitLab adapter (Phase 3; GitHub covers Phase 2 pilots).
- Admin dashboard UI (rejected).
- Raw `system_prompt` self-edit (Phase 3).
- Multi-model routing (Phase 3).
- SSO, RBAC, multi-tenancy (Phase 3).
- HA / Keep replication (Phase 3).
- Formal behavioral regression test harness (rejected in favor of immutable core + history).
- Shadow-mode dual-runtime deployment (rejected as too complex for the safety delivered).

---

## 4. Milestones

### M1 — Keeper-to-Keeper communication [ ]

**Goal**: Watchkeepers can talk to each other naturally, subscribe to each other's events, and escalate when a peer conversation stalls.

**Scope**

- [x] **M1.1** Dedicated K2K channels: one private Slack channel per K2K conversation, visible only to participating bots (humans opt in via CLI `wk channel reveal <convo_id>`).
  - [x] **M1.1.a** K2K conversation domain + persistence: `core/pkg/k2k/` package with `Conversation` struct, `Repository` interface, in-memory + Postgres impls; new migration adding `k2k_conversations` table (id PK, slack_channel_id nullable, participants text[], subject, status enum open|archived, token_budget int, tokens_used int, opened_at, closed_at, correlation_id, organization_id for RLS); CRUD: `Open(participants, subject, budget)`, `Get(id)`, `List(filter)`, `Close(id, reason)`, `IncTokens(id, n)`. Unit tests cover open→close lifecycle, concurrent token-budget updates, and per-org RLS scoping consistent with Phase 1 M2.1.d. No Slack wiring here.
  - [x] **M1.1.b** Slack adapter channel primitives: extend the existing Slack adapter with `CreateChannel(name, isPrivate) (channelID, error)`, `InviteToChannel(channelID, userIDs)`, `ArchiveChannel(channelID)`, `RevealChannel(channelID, userID)`. Idempotent `CreateChannel` (existing channel-name returns its ID). Typed errors for Slack-side failures. Mock adapter for unit tests; one contract test against the slack-go stub server. No K2K wiring here — pure capability surface, depends on nothing from M1.1.a.
  - [x] **M1.1.c** `wk channel reveal` CLI + K2K lifecycle wiring (depends on M1.1.a + M1.1.b): `wk channel reveal <conversation_id>` subcommand looks up `slack_channel_id` from M1.1.a `k2k_conversations` and calls M1.1.b `RevealChannel` with the caller's Slack user ID. K2K lifecycle wiring: on `Open()`, generate channel name `k2k-<conv-id-prefix>`, call `CreateChannel(private=true)`, `InviteToChannel(bot_ids)`, persist `slack_channel_id`; on `Close()`, call `ArchiveChannel`. Integration test exercises full open→reveal→close lifecycle through the mock Slack adapter from M1.1.b.
- [x] **M1.2** `keepclient.list_peers()` returning active Watchkeepers with role description, language, capabilities, availability.
- [ ] **M1.3** Built-in tools added to every Watchkeeper's base toolset (`peer.ask`, `peer.reply`, `peer.close`, `peer.subscribe`, `peer.broadcast`); decomposed into independently-shippable leaves below because the five tools have distinct API shapes (sync request-reply vs lifecycle finalize vs pub-sub vs fan-out), distinct dependencies (broadcast needs `role_filter` resolver over M1.2 `list_peers`; subscribe needs an event-stream seam not present in M1.1.\*), and distinct test surfaces.
  - [x] **M1.3.a** `peer.ask` + `peer.reply` request-reply pair (depends on M1.1.c + M1.2): new `core/pkg/peer/` package shipping `peer.Tool` built-ins `Ask(ctx, target, subject, body, timeout) (conversationID, replyBody, error)` and `Reply(ctx, conversationID, body) error`. Add `k2k_messages` table (id PK, conversation_id FK → `k2k_conversations`, sender_watchkeeper_id, body text, direction enum `request|reply`, created_at, organization_id for RLS); extend `core/pkg/k2k/repository.go` with `AppendMessage(ctx, params) (Message, error)` + `WaitForReply(ctx, conversationID, since time.Time, timeout) (Message, error)` (in-memory uses cond-var; Postgres uses `LISTEN/NOTIFY` on a per-conversation channel, with a polling fallback). `peer.Ask` resolves `target` (watchkeeper id or role name) via `keepclient.list_peers` from M1.2, calls `k2k.Lifecycle.Open` to mint the conversation + Slack channel, appends the request message, blocks until reply or `timeout`. `peer.Reply` looks up the conversation, appends a `reply`-direction message, signals the waiting `Ask`. Tool-registry entry under `built-in` source with capability `peer:ask` / `peer:reply`. Unit tests cover: happy ask→reply round-trip, timeout fires `ErrPeerTimeout`, unknown target surfaces `ErrPeerNotFound`, capability-broker enforcement via the **acting** agent's Manifest, defensive-copy of body, source-grep audit ban (no `keeperslog.` in `peer/ask.go`/`peer/reply.go` — audit emission is M1.4's seam). Projected ≈900 LOC / ≈10 files.
  - [x] **M1.3.b** `peer.close` lifecycle finalize (depends on M1.3.a): `peer.Close(ctx, conversationID, summary) error` built-in in `core/pkg/peer/close.go`. Composes `k2k.Lifecycle.Close(ctx, id, reason)` from M1.1.c (which archives the Slack channel + transitions `k2k_conversations.status` to `archived`) with a one-line `Summary` write into the conversation row (`k2k_conversations.close_summary text` column via new migration `031_k2k_close_summary.sql` — number bumped from 030 because M1.3.a + M3.1 both landed on 030). Tool-registry entry under `built-in` source with capability `peer:close`. Idempotent: closing an already-archived conversation is a no-op returning nil (mirrors M1.1.b's `ErrAlreadyArchived` translation). Surfaces `ErrPeerNotFound` for unknown conversation ids; surfaces `ErrPeerClosePermission` when the acting agent is not a participant. Unit tests cover idempotent double-close, unknown conversation, non-participant rejection, summary persistence round-trip, capability enforcement, source-grep audit ban. No Keep knowledge-chunk write here — that is M1.7's responsibility (archive-on-summary writer). Projected ≈600 LOC / ≈7 files.
  - [ ] **M1.3.c** `peer.subscribe` event-stream seam (depends on M1.1.c; introduces the event-stream interface M1.4 will consume): `peer.Subscribe(ctx, target, eventTypes []string) (<-chan Event, error)` built-in in `core/pkg/peer/subscribe.go`. Introduces a `peer.EventBus` interface (`Publish(ctx, event) error`, `Subscribe(ctx, filter SubscribeFilter) (<-chan Event, CancelFunc, error)`) with in-memory + Postgres-`LISTEN/NOTIFY` impls in `core/pkg/peer/eventbus_memory.go` / `eventbus_postgres.go`; new migration `031_peer_events.sql` adds `peer_events` table (id, watchkeeper_id, event_type, payload jsonb, created_at, organization_id for RLS) and a `peer_event_published` Postgres trigger that fires `NOTIFY peer_events`. Filter shape: `{TargetWatchkeeperID, EventTypes []string}`. The returned channel is closed on ctx cancellation; cancel-leak test pins zero goroutines after 100 subscribe/cancel cycles. Tool-registry entry under `built-in` source with capability `peer:subscribe`. Unit tests cover happy publish→subscribe delivery, multi-type filter, ctx-cancel closes channel, slow-consumer drop policy (bounded buffer + counter on dropped events), per-org RLS isolation on the Postgres impl, capability enforcement, source-grep audit ban. M1.4 will consume `EventBus.Publish` to emit the K2K event taxonomy; this leaf ships only the seam + the consumer-facing built-in. Projected ≈900 LOC / ≈11 files.
  - [ ] **M1.3.d** `peer.broadcast` fan-out (depends on M1.3.a + M1.2): `peer.Broadcast(ctx, roleFilter, body) (BroadcastResult, error)` built-in in `core/pkg/peer/broadcast.go`. New `peer.RoleFilter` resolver (`Resolve(ctx, filter RoleFilter) ([]WatchkeeperID, error)` with fields `Roles []string`, `Languages []string`, `Capabilities []string`, `ExcludeSelf bool`) that fans out over `keepclient.list_peers` from M1.2 and applies the filter in-memory. `peer.Broadcast` resolves the target set, then calls `peer.Ask` from M1.3.a with `timeout=0` (fire-and-forget; no reply collection) for each target in parallel via a bounded worker pool (default 8). `BroadcastResult` carries per-target `{watchkeeper_id, conversation_id, error}` so partial failures are observable without aborting the fan-out. Tool-registry entry under `built-in` source with capability `peer:broadcast`. Unit tests cover: empty filter surfaces `ErrPeerRoleFilterEmpty`, no-match surfaces `ErrPeerNoTargets`, partial failures collected into result not surfaced as top-level error, worker-pool bound enforced (a 100-target broadcast with bound=8 has ≤8 in-flight asks at any moment, pinned via a counting fake), `ExcludeSelf` honoured, capability enforcement, source-grep audit ban. Projected ≈700 LOC / ≈8 files.
- [ ] **M1.4** Standard K2K event taxonomy in `keeperslog`: `k2k_message_sent`, `k2k_message_received`, `k2k_conversation_opened`, `k2k_conversation_closed`, `k2k_over_budget`, `k2k_escalated`.
- [ ] **M1.5** Token-budget per conversation (configurable default, per-Watchkeeper override via Manifest). Overage triggers `k2k_over_budget` + automatic escalation to the Watchkeepers' leads.
- [ ] **M1.6** Escalation saga: peer timeout or budget overage → human lead in Slack; if lead unresponsive for N minutes → Watchmaster.
- [ ] **M1.7** Conversation archive on close: channel archived; summary written as a single Keeper's Log entry + a Keep knowledge chunk for future retrieval.
- [ ] **M1.8** Capability boundary: every tool invoked as a result of a K2K request still checks against the **acting** agent's Manifest ACLs — K2K can never lend capability.

**Artifacts**: Go `peer/` package, `keepclient.list_peers`, Slack channel management extensions, escalation saga, capability-gating tests.

**Verification**

- [ ] Watchkeeper A calls `peer.ask(B, ...)`; B receives it in its queue; B's reply reaches A within expected latency; both sides have correlated Keeper's Log entries.
- [ ] Subscription: agent B emits `review_completed`; subscribed agent A receives it and acts.
- [ ] Budget test: force a K2K conversation past budget; `k2k_over_budget` fires; escalation reaches the lead within the saga's SLA.
- [ ] Capability-laundering test: agent A asks agent B to do X; B's Manifest lacks capability for X; invocation rejected with the same error as a direct attempt by B.
- [ ] Archive test: closed channel disappears from active list; summary entry exists in Keep; channel re-opening requires operator action.

**Dependencies**: Phase 1 M10 (need stable CLI + observability).
**Magnitude**: 7–10 days.

---

### M2 — Constrained prompt self-tuning [ ]

**Goal**: A Watchkeeper can propose changes to its own `personality`, `language`, or `toolset_acls`, approved through the existing self-modification flow. Any attempt to touch `system_prompt` or `immutable_core` is mechanically refused.

**Scope**

- [ ] **M2.1** Built-in tool `self_tune(field, new_value, reason)` on every Watchkeeper. Tool schema restricts `field ∈ {personality, language, toolset_acls}`. Any other value fails at the schema layer.
- [ ] **M2.2** Self-tuning proposal creates a new Manifest version via `keepclient.put_manifest_version` with `status=proposed`, `reason` (mandatory), `proposer=<agent_id>`, `parent_version_id=<current>`.
- [ ] **M2.3** Validation pass (before the approval card is posted): Watchmaster-as-AI-reviewer verifies only allowed fields changed; verifies `immutable_core` byte-identical to parent; produces a plain-language diff summary for the approval card.
- [ ] **M2.4** Routes through the existing approval flow (git-pr or slack-native, per deployment configuration).
- [ ] **M2.5** Approved version hot-applies on the next turn (same pattern as tool hot-load, with grace period for in-flight interactions).
- [ ] **M2.6** Per-Watchkeeper quota: max N self-tuning proposals per day (default 3), enforced at the tool level.
- [ ] **M2.7** Self-tune events: `self_tune_proposed`, `self_tune_approved`, `self_tune_rejected`, `self_tune_applied`, `self_tune_blocked_immutable`, `self_tune_quota_exceeded`.

**Artifacts**: `self_tune` built-in tool + schema, Watchmaster AI-reviewer extensions for Manifest diffs, approval-card templates.

**Verification**

- [ ] Agent proposes a personality tweak; Watchmaster posts an approval card with a readable diff; lead clicks Approve; new version applies on next turn; behavior change observable in replies.
- [ ] Agent tries `self_tune(field='system_prompt', ...)` → tool-schema rejection; attempt logged.
- [ ] Agent tries `self_tune(field='toolset_acls', new_value=<contains capability listed in immutable_core.role_boundaries>)` → validator rejection.
- [ ] Agent hits quota limit; subsequent proposal rejected with informative error.

**Dependencies**: M3 (immutable core must exist before self-tuning can reference it).
**Magnitude**: 5–7 days.

---

### M3 — Immutable core + Manifest state history + conversational retune [ ]

**Goal**: Make the immutable boundary mechanical; expose Manifest history as a first-class surface; let leads roll back through conversation, not CLI.

**Scope**

- [x] **M3.1** Manifest schema: `immutable_core` sub-object with 5 buckets:
  - `role_boundaries` — explicit list of capabilities the Watchkeeper is NOT allowed to have, regardless of what self-tuning proposes.
  - `security_constraints` — data-handling rules, forbidden data destinations, classification floors.
  - `escalation_protocols` — when and to whom to escalate; cannot be disabled.
  - `cost_limits` — max token spend per task / day / week.
  - `audit_requirements` — what must be logged; cannot be reduced.
- [ ] **M3.2** Set at spawn by a platform admin (config file or Watchmaster flow); changeable only via direct Manifest edit + core restart. Never modifiable by the agent, the lead, or any self-tuning path.
- [x] **M3.3** `manifest_version` schema: add `reason`, `previous_version_id`, `proposer` columns (make explicit; Phase 1 may have them implicit by timestamp).
- [ ] **M3.4** Watchmaster tools:
  - `manifest.history(watchkeeper, limit)` — returns versions with timestamps, reasons, proposers.
  - `manifest.diff(watchkeeper, v1, v2)` — returns a human-readable diff.
  - `manifest.rollback(watchkeeper, target_version, reason)` — creates a new Manifest version that is a copy of `target_version`, routed through approval.
  - `manifest.merge_fields(watchkeeper, base_version, source_version, fields)` — creates a proposal taking specified fields from `source_version` on top of `base_version`. Routed through approval.
- [ ] **M3.5** Slack UX: lead says "coordinator was better last Tuesday" to Watchmaster → Watchmaster calls `manifest.history`, finds the version active on that date, calls `manifest.diff` vs current, posts the diff in chat, asks "revert to that version?" with Approve/Reject. On approve: `manifest.rollback` → approval card → Manifest applied.
- [ ] **M3.6** Self-tuning validator (enforced in M2): any proposal touching `immutable_core` fields refused before reaching the approval card.

**Artifacts**: schema migration for `immutable_core` + `manifest_version` metadata, Watchmaster manifest tools, Slack UX templates for history navigation, validator tests.

**Verification**

- [ ] Enforcement: self-tuning proposal touching `immutable_core.cost_limits` refused; `self_tune_blocked_immutable` logged.
- [ ] Rollback: lead says "rollback coordinator to last Friday's version" to Watchmaster; Watchmaster identifies target version, proposes rollback, lead approves, new Manifest version active and byte-identical to target.
- [ ] Merge: lead says "keep current tools but take personality from the version before the Tuesday change"; Watchmaster produces a merged proposal; lead approves; applied version has specified composition.
- [ ] Admin-only editability: attempt to modify `immutable_core` through the self-tuning API path fails; direct database edit + restart succeeds and is logged.

**Dependencies**: Phase 1 M2 (Manifest versioning table).
**Magnitude**: 3–5 days.

---

### M4 — GitHub adapter + code-review tool suite [ ]

**Goal**: Platform ships GitHub integration plus tools oriented for code review. Operators spawn any Manifest that uses these tools to create a code-reviewing agent. **No "Code Reviewer" role exists in core.**

**Scope**

- [ ] **M4.1** GitHub adapter (Go): uses `go-github`; installation-token flow for GitHub Apps; PAT fallback for simpler deployments. Credentials via the Phase 1 secrets interface.
- [ ] **M4.2** Tool suite in platform Tool Registry (TypeScript):
  - `github.list_open_prs(repo, filters)`
  - `github.fetch_pr(repo, pr_number)` — full context: diff, description, linked issues, prior reviews, CI status, comments, changed files.
  - `github.post_review_comment(repo, pr_number, path, line, body)`
  - `github.post_pr_comment(repo, pr_number, body)`
  - `github.request_changes(repo, pr_number, body)`
  - `github.approve_pr(repo, pr_number, body)`
  - `github.fetch_ci_status(repo, pr_number)`
  - `github.list_recent_commits(repo, branch, since)`
- [ ] **M4.3** Capability declarations: read-only tools have `github:read`; mutating tools have `github:write` or `github:review`. Operator grants per-Watchkeeper via Manifest.
- [ ] **M4.4** Webhook intake: GitHub webhook receiver in the core emits events `github.pr_opened`, `github.pr_updated`, `github.review_submitted`, etc., consumable via K2K `peer.subscribe` or directly from the core event bus.
- [ ] **M4.5** Rate limiting + circuit breakers on the adapter layer (not exposed to agents; transparent retries + backoff).

**Artifacts**: Go `github/` adapter, TS tool implementations in platform tool registry, capability dictionary entries (`github:read`, `github:write`, `github:review`), webhook receiver.

**Verification**

- [ ] Manifest using `github.fetch_pr` + `github.post_review_comment` spawned; agent reads a real PR in the test repo and posts an actionable review comment.
- [ ] Capability enforcement: agent with only `github:read` invoking `post_review_comment` is refused by capability broker.
- [ ] Rate-limit test: burst of 100 requests handled with backoff; no throttling errors bubble up to the agent.
- [ ] Webhook test: PR opened in test repo → `github.pr_opened` event emitted → subscribed Watchkeeper receives it.

**Dependencies**: Phase 1 M9 (Tool Registry).
**External prerequisite**: GitHub App installed on target test repo, or PAT with appropriate scopes.
**Magnitude**: 7–10 days.

---

### M5 — Confluence adapter + doc-sync tool suite [ ]

**Goal**: Platform ships Confluence integration and tools oriented for detecting stale docs and drafting updates. Same pattern as M4.

**Scope**

- [ ] **M5.1** Confluence adapter (Go): REST client; OAuth or API-token auth; credentials via secrets interface.
- [ ] **M5.2** Tool suite in platform Tool Registry:
  - `confluence.list_pages(space, filters)`
  - `confluence.fetch_page(page_id)` — content + metadata (last modified, author, labels, inbound link count).
  - `confluence.propose_page_update(page_id, new_content, reason)` — creates a draft (unpublished); never auto-publishes.
  - `confluence.diff_page_against_source(page_id, source_ref)` — compares documented contract to current source (e.g., doc vs OpenAPI spec, doc vs runbook).
  - `confluence.list_stale_pages(space, threshold_days)` — pages older than threshold with no activity.
  - `confluence.list_orphaned_pages(space)` — pages without inbound links.
- [ ] **M5.3** Capability declarations: read tools `confluence:read`; draft tools `confluence:draft`. `confluence:publish` exists but is **never** granted by default — only explicit operator grant after trust is built.
- [ ] **M5.4** Dry-run support: `propose_page_update` supports `dry_run=true` returning the proposed diff without creating the draft — consumed by the approval flow from Phase 1 M9.

**Artifacts**: Go `confluence/` adapter, TS tools in registry, capability dictionary entries.

**Verification**

- [ ] Manifest using doc-sync tools spawned; agent lists stale pages in test space, proposes an update, lead sees the draft in Confluence (unpublished).
- [ ] Code↔doc diff: an API doc compared against an OpenAPI spec; divergence highlighted in the proposed update payload.
- [ ] Capability enforcement: `confluence:read` cannot invoke `propose_page_update`; `confluence:draft` cannot invoke `publish`.

**Dependencies**: Phase 1 M9.
**External prerequisite**: Confluence API credentials + test space; seeded sample stale page; an OpenAPI spec in a known location for the diff test.
**Magnitude**: 6–8 days.

---

### M6 — Manifest template catalog [ ]

**Goal**: Ship a small catalog of example Manifests so common role patterns have a tested starting point. Templates are documentation-style artifacts — the platform still has zero hardcoded roles.

**Scope**

- [ ] **M6.1** `templates/manifests/` directory with YAML Manifests for:
  - `code-reviewer.yaml` — uses GitHub tools from M4.
  - `tech-writer.yaml` — uses Confluence tools from M5.
  - `pm-assistant.yaml` — uses Jira (Phase 1 M8) + Confluence tools.
  - `incident-responder.yaml` — uses Jira + Slack + K2K to Coordinator.
- [ ] **M6.2** Every template fills: `immutable_core` example values (least-privilege recommendations), `personality` example, `language` default, `toolset_acls` with least-privilege capability list, `system_prompt` scaffolding role intent.
- [ ] **M6.3** Template linter in CI: every template passes the same capability-declaration + Manifest schema checks as live Manifests.
- [ ] **M6.4** Watchmaster tools:
  - `template.list()` — returns catalog.
  - `template.show(name)` — returns full Manifest.
  - `template.propose_spawn(name, customizations)` — drafts a concrete Manifest from template + overrides, enters the Phase 1 spawn flow.

**Artifacts**: `templates/manifests/` directory with 4 reference files, Watchmaster template tools, CI linter rule.

**Verification**

- [ ] Lead DMs Watchmaster "spawn a code reviewer for repo X"; Watchmaster picks `code-reviewer.yaml`, customizes, drafts Manifest, approval, live.
- [ ] Every template lints clean.
- [ ] Diff of a template vs. a corresponding real running Manifest is under a small threshold (templates reflect current best practice — automated check can flag drift).

**Dependencies**: M4, M5.
**Magnitude**: 2–3 days.

---

### M7 — Notebook Phase 2 upgrades [ ]

**Goal**: Close the deferred items from Phase 1 Notebook design. Scale experience; sharpen signal-to-noise; make personality structurable.

**Scope**

- [ ] **M7.1** **Auto-inheritance on retire→spawn**: when a new Watchkeeper is spawned with the same `role_id` (derived from template or an explicit field) as a recently retired one, its Notebook is seeded from the predecessor's latest archive **by default**. Opt-out flag at spawn. `notebook_inherited` event logged; lead receives a 24h digest summarizing inherited entries.
  - [x] **M7.1.a** `role_id` schema foundation: add migration `032_watchkeepers_role_id.sql` introducing nullable `watchkeeper.watchkeeper.role_id text` column (no FK; derived from Manifest template slug or explicit spawn field) with a partial index `idx_watchkeeper_role_id_retired ON watchkeeper.watchkeeper(role_id, retired_at DESC) WHERE retired_at IS NOT NULL AND archive_uri IS NOT NULL` to back the M7.1.b predecessor lookup. Extend `keepclient.Watchkeeper` (`core/pkg/keepclient/read_watchkeeper.go`) with `RoleID *string \`json:"role_id"\`` projected on the existing `GetWatchkeeper` / `ListWatchkeepers` paths plus a server-side `handleInsertWatchkeeper` accept of `role_id` (legacy callers omitting the field stay nil). Acceptance: migration up/down clean against `scripts/migrate-schema-test.sh`; `keepclient.GetWatchkeeper` round-trips `role_id`; insert handler persists supplied `role_id`; existing rows project `nil`. No saga / inheritance behavior in this leaf.
  - [ ] **M7.1.b** Predecessor-lookup DAO + endpoint (depends on M7.1.a): add `keepclient.LatestRetiredByRole(ctx, organizationID, roleID string) (*Watchkeeper, error)` in a new `core/pkg/keepclient/read_latest_retired_by_role.go` plus server handler `handleGetLatestRetiredByRole` mounted at `GET /v1/watchkeepers/latest-retired-by-role?role_id=...` returning the most recent `retired_at IS NOT NULL AND archive_uri IS NOT NULL` row for the caller's tenant (using the M3.5.a `claim.OrganizationID` filter pattern), 404 when no predecessor exists, typed `keepclient.ErrNoPredecessor` sentinel on the client side. Acceptance: contract test against `httptest` server returns the freshest retired peer; cross-tenant role match returns 404; legacy claim returns `403 organization_required`; <500 LOC across handler + client + tests.
  - [ ] **M7.1.c** `NotebookInheritStep` saga step + opt-out (depends on M7.1.b): add `core/pkg/spawn/notebookinherit_step.go` implementing `saga.Step` that runs **before** `NotebookProvisionStep`, calls `keepclient.LatestRetiredByRole` on the new Watchkeeper's `role_id`, and on hit seeds the fresh notebook by calling a new `notebook.ImportFromArchiveURI(ctx, dst, archiveURI)` helper (thin wrapper around the existing `core/pkg/notebook/import_from_archive.go` flow) before `NotebookProvisionStep` opens the per-agent file. Add `NoInherit bool` to `saga.SpawnContext` (`core/pkg/spawn/saga/spawn_context.go`) plumbed from a `--no-inherit` flag on the Watchmaster spawn tool; when true, the step is a no-op. Emit `notebook_inherited` audit event (Keeper's Log) with `predecessor_watchkeeper_id`, `archive_uri`, `entries_imported` count on success. Compensator reuses `NotebookProvisionStep.Compensate` (no extra archive churn — inherited file is owned by the provision step). Acceptance: saga unit test spawns with matching retired predecessor → new notebook contains predecessor entries + `notebook_inherited` logged; `--no-inherit` path leaves notebook empty + no audit event; no predecessor → step is a no-op + no audit event; fault-injection test confirms compensator chain unchanged.
  - [ ] **M7.1.d** 24h inherited-entries digest job (depends on M7.1.c): add `core/pkg/notebook/inherit_digest.go` periodic job (modeled on `periodic_backup.go`) that scans `notebook_inherited` audit rows from the last 24h, groups by lead human, and posts a Slack DM digest via the existing Watchmaster Slack channel naming inherited entry counts + predecessor → successor pairs. Schedule entry point in `cmd/watchkeeper` (or wherever `periodic_backup` is registered) with 24h tick and `--inherit-digest-enabled=true` default. Acceptance: unit test with seeded audit rows produces expected digest payload for two leads; tick scheduler test verifies idempotent runs (no duplicate DMs within the 24h window — uses a `last_run_at` marker row in `notebook.inherit_digest_runs` table introduced by migration `033_inherit_digest_runs.sql`); empty 24h window produces no DM; <800 LOC across job + migration + tests.
- [ ] **M7.2** **Reflection sampling on tool success**: configurable sampling rate (default 1-in-50). Success reflections stored as `observation` category; lower default auto-injection weight than `lesson`.
- [ ] **M7.3** **Lesson consolidation**: daily background job clusters semantically similar `lesson` entries; produces a consolidated summary entry; originals marked `superseded_by`; consolidated entry inherits the highest `active_after`. `lessons_consolidated` event with counts.
- [ ] **M7.4** **Personality preset catalog**: optional structured fields layered over the Phase 1 free-text `personality`:
  - `tone_preset ∈ {formal_technical, warm_collaborative, terse_pragmatic, ...}`
  - `emoji_policy ∈ {none, sparing, rich}`
  - `humor_level ∈ {none, dry, playful}`
  - `response_length_target ∈ {concise, moderate, thorough}`
    Renderer composes these with the free-text `personality` into the effective system prompt via a templater. Presets ship as part of M6 template catalog.
- [ ] **M7.5** **Recall latency optimization toward sub-ms p99 at 10k entries**: the Phase 1 M2b verification-bullet-216 benchmark currently asserts p99 < 100 ms because sqlite-vec ships only brute-force KNN through `vec0` at the 1536-dim corpus. Explore one or more paths to drive p99 toward the original sub-millisecond target: (a) int8 / binary quantization via `vec_quantize_*` with a reranking pass, (b) tiered retrieval — keep an "active recent" hot subset in memory and fall back to full corpus only when the hot set misses, (c) swap to a backend with true ANN (HNSW in sqlite-vec when released, or external Qdrant/Milvus behind the existing `Recall` API). Whichever path is chosen, tighten the bench budget in `core/pkg/notebook/recall_bench_test.go` and update the bullet text in ROADMAP-phase1.md §M2b verification line that points at this item.

**Artifacts**: Notebook library extensions, consolidation cron job, templater update, preset catalog, recall-latency optimization path (one of: quantization, tiered retrieval, ANN backend swap).

**Verification**

- [ ] Inheritance: retire Coordinator; spawn new Coordinator with same role; new instance's Notebook contains predecessor's entries; `notebook_inherited` in Keeper's Log; 24h digest delivered to lead.
- [ ] Opt-out: spawn with `--no-inherit` flag; new Notebook empty.
- [ ] Sampling: simulate 100 successful tool calls with rate 1-in-50; ~2 reflections written; classified as `observation` with lower auto-injection weight.
- [ ] Consolidation: seed 20 semantically-similar lessons; run consolidation; 1 summary entry remains active, 20 marked superseded.
- [ ] Presets: spawn with `tone_preset=terse_pragmatic`; agent's replies measurably shorter than the same scenario with `tone_preset=warm_collaborative`.
- [ ] Recall latency: `make notebook-bench` reports p99 below the tightened budget on a 10k-entry 1536-dim corpus; the chosen approach (quantization, tiering, ANN backend) is documented in `docs/lessons/M2b.md` with measured numbers.

**Dependencies**: Phase 1 M2b, Phase 1 M5.
**Magnitude**: 5–7 days.

---

### M8 — Tool signing mandatory for non-local sources [ ]

**Goal**: Raise the trust bar: in Phase 2 every tool in `built-in`, `platform`, `private:git`, `private:hosted` sources must be signed and verified on load. Only `local` may remain unsigned (loud audit continues).

**Scope**

- [ ] **M8.1** Core config: `tool_signing.required_for` defaults to all non-local sources.
- [ ] **M8.2** Platform release CI: signs `watchkeeper-tools` release artifacts with the platform key; `built-in` tools signed at core-binary build time.
- [ ] **M8.3** Customer-side procedure: key-pair generation, public key pinning in operator config, tool-signing on PR merge or hosted-storage write.
- [ ] **M8.4** `wk tool sign <folder> --key <path>` CLI for operators upgrading from Phase 1.
- [ ] **M8.5** `wk tool sources verify` reports every currently-loaded tool's signature status.
- [ ] **M8.6** Migration path: Phase 1 deployments keep permissive default for one minor version; hard-fail behind a config flag so operators flip when ready.

**Artifacts**: updated `toolregistry/` with strict verification, `wk tool sign` CLI, platform release CI update, migration docs + runbook section.

**Verification**

- [ ] Unsigned tool in `platform` source → refused with `signature_verification_failed`; Slack alert to admin.
- [ ] Phase 1 upgrade drill: existing unsigned private tools migrated without downtime.
- [ ] `wk tool sources verify` flags any unsigned or expired-signature tool.
- [ ] Revoked key: load attempt after revocation refused.

**Dependencies**: Phase 1 M9.
**External prerequisite**: platform signing key pair; customer public keys pinned per `private:*` source.
**Magnitude**: 3–5 days.

---

### M9 — Keep backup/restore automation + retention [ ]

**Goal**: Operate Keep reliably as it grows; automate what was manual in Phase 1 runbook.

**Scope**

- [ ] **M9.1** Cron-scheduled Keep backup to `ArchiveStore` (LocalFS for dev, S3-compatible for prod). Full + incremental; operator-configurable schedule.
- [ ] **M9.2** `keepers_log` retention: move entries older than a configurable threshold to a cold `ArchiveStore` bucket. `keep.log_tail` reads transparently across hot + cold.
- [ ] **M9.3** Backup integrity validator: periodic job restores a random recent backup to a scratch Postgres, runs sanity queries, emits `backup_verified` or `backup_corrupt` event.
- [ ] **M9.4** Restore drill: `make keep-restore-drill` spins up an isolated Keep from the latest backup, runs smoke queries, tears down. Monthly by default.
- [ ] **M9.5** `knowledge_chunk` growth monitoring: Prometheus metric; dashboard alert when crossing threshold; runbook guidance on compaction.

**Artifacts**: backup cron jobs, retention migration, cold-storage read-through layer on `log_tail`, restore drill script, Grafana panel.

**Verification**

- [ ] Scheduled backup runs successfully; operator-triggered `wk keep backup` equally works.
- [ ] Cold-storage read-through: query an event older than the hot threshold; returns correctly via `log_tail`.
- [ ] Restore drill completes green on a fresh clone of prod.
- [ ] Forcibly corrupting the latest backup triggers the integrity validator alert within one cycle.

**Dependencies**: Phase 1 M2, Phase 1 M10.
**Magnitude**: 3–5 days.

---

### M10 — Phase 2 integration demo + metric harness [ ]

**Goal**: Acceptance gate — three Watchkeepers live together, K2K conversations happen, and a business metric moves measurably.

**Scope**

- [ ] **M10.1** Demo deployment: three Watchkeepers running simultaneously in the dev workspace:
  1. Coordinator (from Phase 1, unchanged).
  2. GitHub-capable bot (spawned from `code-reviewer.yaml`, wired to a real test GitHub repo).
  3. Confluence-capable bot (spawned from `tech-writer.yaml`, wired to a Confluence test space).
- [ ] **M10.2** Scripted K2K scenario: new PR opens → GitHub-capable bot reviews → Coordinator subscribed to `review_completed` receives the event → Coordinator pings the PR author in Slack; all three bots visible in logs with correlated events.
- [ ] **M10.3** Metric harness: `review_cycle_time` — median time from PR-opened to PR-merged, emitted to Prometheus. Baseline window of 2 weeks with bots passive (read-only); instrumented window of 2 weeks with bots active; delta reported in a Grafana panel.
- [ ] **M10.4** Phase 2 smoke test: `make phase2-smoke` runs the full scenario end-to-end against stubbed GitHub + Confluence + real Keep + real Notebook.
- [ ] **M10.5** Runbook appendix: spawning additional bots from templates, debugging K2K stalls, navigating Manifest history, performing a conversational rollback.

**Artifacts**: demo scripts, metric harness, Grafana dashboard panel, smoke-test flow, runbook additions.

**Verification**

- [ ] Scripted scenario passes end-to-end without manual intervention.
- [ ] Metric harness reports a delta (positive or negative) over the instrumented window.
- [ ] A second engineer reproduces the demo from the runbook without assistance.

**Dependencies**: M1–M9.
**Magnitude**: 5–7 days.

---

## 5. Risks (Phase 2-specific)

| Risk                                                                                     | Likelihood | Impact   | Mitigation                                                                                                                                                                                                   |
| ---------------------------------------------------------------------------------------- | ---------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| K2K conversations run away despite token budgets                                         | Med        | Med      | Escalation triggers on budget breach; metric `k2k_conversations_over_budget` with dashboard alert; post-mortem every escalation in the first month.                                                          |
| Self-tuning drifts personality in a direction the lead doesn't notice for weeks          | Med        | Med      | Manifest version history is always available; Coordinator's daily briefing includes a "Manifest changes in the past 24h across all Watchkeepers" section; conversational rollback is one sentence away.      |
| One agent convinces another via K2K to do something the first's Manifest forbids         | Med        | High     | Every capability-requiring action still goes through the capability broker, which checks the **acting** agent's Manifest. K2K can only request, never grant. Integration test explicitly for this.           |
| GitHub API rate limits throttle a Watchkeeper reviewing at scale                         | Med        | Med      | Per-repo+per-minute throttle in the adapter; exponential backoff; Prometheus metric on remaining rate-limit budget; soft-cap alert.                                                                          |
| Confluence publish-by-mistake leaks drafts                                               | Low        | High     | Tools only draft; `confluence:publish` capability is never granted by default; dry-run supported; lead reviews every draft before publishing manually.                                                       |
| Manifest template catalog goes stale as capabilities evolve                              | Med        | Low      | Template linter in CI; Phase 3 can add template regeneration from live Manifests.                                                                                                                            |
| Tool-signing rollout breaks Phase 1 deployments on upgrade                               | Med        | High     | Default remains permissive for one minor version; hard-fail behind config flag; migration CLI + clear runbook section; upgrade drill in operator playbook.                                                   |
| Backup corruption goes undetected for weeks                                              | Low        | Critical | Monthly restore drill; incremental backups verified on write; integrity validator runs continuously on the latest backup.                                                                                    |
| Immutable core misconfigured at spawn (too loose)                                        | Med        | High     | Watchmaster spawn flow validates `immutable_core` against a platform-level policy schema; admin gets a warning on unusually permissive fields; spawn from template always inherits least-privilege defaults. |
| Notebook auto-inheritance carries irrelevant lessons to a new agent with different scope | Med        | Med      | Inheritance is opt-in-by-default for same-role only; opt-out flag at spawn; first-24h digest to lead summarizes inherited lessons; `forget` one command away.                                                |
| Lead never uses conversational retune and the feature rots                               | Low        | Med      | Runbook front-loads the feature; smoke-test demo exercises it; if telemetry shows zero use after 30 days, re-design with the operator.                                                                       |

---

## 6. Cross-cutting Constraints (additions to Phase 1)

- **No roles in core.** Platform ships capabilities; operators compose roles via Manifests. Any proposal that adds a "role" as a Phase-2+ platform milestone is rejected by default.
- **Immutable core is mechanical, not conventional.** Self-tuning validators reject `immutable_core` modifications at the schema layer, not via policy comment.
- **K2K never bypasses capability gating.** When agent A asks agent B to do X, B still needs the capability for X in B's own Manifest. Capability laundering through K2K is impossible by construction.
- **K2K conversations are auditable.** Every message, subscription, escalation, and budget breach lands in Keeper's Log with a correlation ID.
- **History over tests.** Phase 2 safety for self-tuning = immutable core + Manifest version history + conversational retune. No formal regression tests, no shadow deployments.
- **No admin dashboard.** CLI + Slack-native + Grafana + Keeper's Log are the four operator surfaces. Any proposal to build a fifth is rejected by default.

---

## 7. Definition of Done (Phase 2)

- [ ] 3 Watchkeepers spawned from 3 distinct Manifests — Coordinator (from Phase 1), GitHub-capable, Confluence-capable — operating simultaneously.
- [ ] K2K conversation between any pair works end-to-end; escalation tested; archive summarized into Keep.
- [ ] Agent successfully self-tunes `personality`; lead approves; new Manifest version applies; `immutable_core` untouched and enforcement verified.
- [ ] Lead performs a conversational rollback of a Watchkeeper's Manifest through Watchmaster in Slack; rollback reversal is one Slack exchange end-to-end.
- [ ] GitHub-capable bot reviews a real PR in the dev test repo (posts a review comment).
- [ ] Confluence-capable bot drafts a doc update in the Confluence test space (unpublished).
- [ ] `review_cycle_time` metric over the 2-week instrumented window vs. the 2-week baseline shows a measurable delta (direction recorded; positive, negative, or statistically-indistinguishable are all valid results — the bar is that the metric was collected and compared).
- [ ] Every tool loaded at runtime in `built-in`, `platform`, `private:git`, `private:hosted` is signed and verified.
- [ ] Keep backup + restore drill passes green; cold-storage retention works; log tail reads span hot and cold seamlessly.
- [ ] Phase 2 runbook walked through by a second engineer without assistance.

---

## 8. External Prerequisites

- [ ] **GitHub App** installed on target test repo, or PAT with review scopes provisioned. _(before M4)_
- [ ] **Confluence test space** with API credentials; sample stale page seeded; reference OpenAPI spec available for the code↔doc diff test. _(before M5)_
- [ ] **Platform signing key pair** generated; public key shipped with platform release. _(before M8)_
- [ ] **Customer public keys** pinned per `private:*` source they own. _(before M8 — per-deployment)_
- [ ] **Durable ArchiveStore endpoint** configured (AWS S3, Cloudflare R2, SeaweedFS, Garage, or equivalent) — LocalFS no longer acceptable for production. _(before M9)_
- [ ] **Baseline review-cycle-time measurement** captured over a 2-week window before Phase 2 bots go live. _(before M10 instrumented window)_

---

## 9. Out of Scope — deferred to Phase 3 and beyond (or rejected)

| Item                                                                                                            | Target   | Reason                                                                     |
| --------------------------------------------------------------------------------------------------------------- | -------- | -------------------------------------------------------------------------- |
| MS Teams / Telegram / Discord messenger adapters                                                                | Phase 3  | Prove Phase 2 value on Slack first.                                        |
| GitLab adapter                                                                                                  | Phase 3  | GitHub covers the majority of pilot customers.                             |
| Admin dashboard UI                                                                                              | Rejected | Duplicates CLI + Slack + Grafana + Keeper's Log surfaces.                  |
| Raw `system_prompt` self-edit                                                                                   | Phase 3  | Blast radius too wide; constrained self-tuning is Phase 2's bar.           |
| Multi-model routing                                                                                             | Phase 3  | `LLMProvider` abstraction ready; multi-provider implementation is Phase 3. |
| SSO, RBAC, multi-tenancy                                                                                        | Phase 3  | Platform-phase territory per business concept.                             |
| HA / Keep replication                                                                                           | Phase 3  | docker-compose remains Phase 2's target.                                   |
| Formal behavioral regression test harness                                                                       | Rejected | Replaced by immutable core + history + conversational retune.              |
| Shadow-mode dual-runtime deployment                                                                             | Rejected | Too complex for the safety delivered by the simpler model.                 |
| Additional roles as platform milestones (PM Assistant, Incident Responder, Security Sentinel, Onboarding Guide) | N/A      | Not milestones — they are Manifest templates; operators compose.           |
| Manifest editor UI / role marketplace                                                                           | Phase 3  | Platform-phase feature for customer self-service.                          |
| Proactive intelligence / predictive analytics                                                                   | Phase 4  | Explicitly Phase 4 in the business concept.                                |
| Cross-org learning (anonymized pattern sharing)                                                                 | Phase 4  | Phase 4 per business concept.                                              |
