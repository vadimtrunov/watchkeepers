# Watchkeeper Ideas Backlog

Parking lot for ideas raised during planning and development. Every idea captured here is analyzed and given a disposition so nothing is lost when ТЗ (detailed specs) are written later.

## Legend

| Status       | Meaning                                                                                                 |
| ------------ | ------------------------------------------------------------------------------------------------------- |
| 🆕 RAW       | Just captured from operator. Not yet analyzed.                                                          |
| 🔍 ANALYZING | Under active analysis — trade-offs being weighed.                                                       |
| ✅ → ROADMAP | Accepted and folded into `ROADMAP-phase1.md`. Entry kept here for history with the milestone reference. |
| ⏸ DEFERRED   | Good idea, but not Phase 1. Tagged with target phase and rationale.                                     |
| ❌ REJECTED  | Decided against. Rationale recorded so it is not re-litigated.                                          |
| 🔁 SPLIT     | Idea split into multiple sub-items; see children.                                                       |

## Working agreement

- Operator throws ideas in any form — one-liner, paragraph, bullet list, voice-note-like stream.
- Each idea is turned into an `IDEA-NNN` entry below with its own fields.
- Analysis happens in-thread; when a disposition is reached, the entry is updated with the decision and, if accepted, a pointer to where it landed in the ROADMAP.
- Before writing ТЗ / detailed specs, scan this file top-to-bottom — all `✅ → ROADMAP` items are already in scope, `⏸ DEFERRED` items should be referenced so phase-2+ work has continuity, `❌ REJECTED` items save you from rediscussing settled questions.

## Entry template

```
### IDEA-NNN — <short title>
**Captured**: YYYY-MM-DD
**Status**: 🆕 RAW
**Raw input**: <operator's words, verbatim or near-verbatim>
**Context**: <where it came up, what triggered it>
**Analysis**:
- <trade-offs>
- <implications for other parts>
- <options considered>
**Disposition**: <decision + where it lives now>
**Lands in**: <ROADMAP milestone / phase-N / rejected>
```

---

## Ideas

### IDEA-001 — Personality + personal memory + self-learning per agent

**Captured**: 2026-04-22
**Status**: 🔁 SPLIT — three distinct mechanisms bundled in one input; each gets its own sub-entry.
**Raw input**: «каждый агент имеет персональность и характер (стиль общения) плюс имеет память и умеет самообучаться — какие-то тулзы не с первого раза сделал переосмыслил и потом не допускает ошибок»
**Context**: first operator brainstorm after the non-engineer customer pivot.
**Why split**: the three concepts have different storage, different failure modes, different approval flows. Shipping them as one "feature" would blur the Definition of Done; splitting lets each land in its right milestone.
**Children**: IDEA-001a (personality), IDEA-001b (structured personal memory), IDEA-001c (reflection-on-failure / lessons learned).
**Disposition**: SPLIT — see children.

---

### IDEA-001a — Personality / communication style per agent

**Captured**: 2026-04-22
**Status**: 🔍 ANALYZING
**Raw input**: «каждый агент имеет персональность и характер (стиль общения)»

**Context**: shapes how Watchkeepers read in Slack DMs and channel messages. A Coordinator pinging a developer should sound different from an Incident Responder paging on-call; within a single org, leads may want a consistent or deliberately differentiated tone.

**Analysis**

- **Storage**: lives in the Manifest — Manifest versioning already gives us audit trail for personality changes.
- **Phase 1 minimal**: one `personality` free-text field in Manifest, merged into `system_prompt` at boot. Operator writes e.g. "terse, technical, no emojis, Russian" and the Watchkeeper reflects it.
- **Phase 2**: structured preset catalog — `tone_preset ∈ {formal_technical, warm_collaborative, terse_pragmatic, ...}` × `language ∈ {ru, en, ...}` × `emoji_policy ∈ {none, sparing, rich}` × `humor_level`. Presets compose into the system prompt via a templater.
- **Self-modification overlap**: agents must **not** drift tone across conversations. Persisting a tone change requires the same approval flow as a prompt tune (Phase 2 territory). Phase 1 only allows lead to edit `personality` by editing the Manifest.
- **Risk**: personality drift overlaps with existing `Prompt drift` risk in ROADMAP §5 — no new row needed.
- **Non-engineer customer path** (from IDEA already in ROADMAP): lead can ask Watchmaster "make this bot warmer" in plain language → Watchmaster proposes a Manifest diff → slack-native approval card.

**Final disposition**: ✅ → ROADMAP. Migrated 2026-04-22.

- Manifest schema adds two fields: `personality` (free-text) and `language` (ISO code, operator-answered 2026-04-22 — kept as a first-class field, not folded into free-text).
- Watchmaster toolset gains `adjust_personality(watchkeeper, new_personality)` and `adjust_language(watchkeeper, new_language)` — both draft a Manifest version bump and go through lead approval (slack-native or git-pr).
- Preset tone catalog, emoji/humor axes → ⏸ Phase 2.

**Lands in**: ROADMAP M2 (Manifest schema fields), M6 (Watchmaster tools), M7 (Spawn Flow respects personality/language on boot).

---

### IDEA-001b — Structured personal memory per agent (Notebook)

**Captured**: 2026-04-22
**Status**: 🔍 ANALYZING (revised after architectural correction — see IDEA-002)
**Raw input**: «плюс имеет память»

**Architectural correction (2026-04-22)**: The Keep is a shared service for **organizational** knowledge (people, processes, documents). Personal per-agent memory is **not** in Keep — it lives in a separate component, **Notebook**, with its own domain and API. This entry is rewritten accordingly.

**Context**: every Watchkeeper has its own long-term memory — what it has learned about its role, its lead's preferences, ongoing tasks, past interactions, lessons from failures. Separate from Keep, separate from Manifest (which lives in git).

**Analysis**

- **Component**: **Notebook** — per-agent personal memory. **Embedded** (SQLite + `sqlite-vec`), co-located with the harness process. No network service, no API credential management per agent — file lives at `$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite`. See IDEA-002 for the architectural split.
- **Library surface** (linked into the harness process):
  - `notebook.remember(category, subject, content)` — write an entry.
  - `notebook.recall(query, category?, k=5)` — semantic retrieval, local, sub-millisecond.
  - `notebook.forget(entry_id, reason)` — soft-delete; emits event to Keeper's Log via Keep client.
  - Access is physically scoped: another agent runs in a different process with a different file — cross-agent reads are filesystem-blocked.
- **Schema**: entry keyed by `(agent_id, category, subject)` with content, embedding, `created_at`, `last_used_at`, `relevance_score`, `superseded_by`, `evidence_log_ref` (pointer into Keeper's Log), `tool_version` (see IDEA-001c). Categories: `lesson`, `preference`, `observation`, `pending_task`, `relationship_note`.
- **Retrieval auto-injection**: harness auto-calls `recall` each turn against conversation context and injects top-K entries into the prompt. Agent does not have to remember to call `recall`.
- **Retention / consolidation**: ⏸ Phase 2. MVP: all entries with age-based relevance decay.
- **Lead override**: `wk notebook show <wk>`, `wk notebook forget <wk> <entry-id> --reason=...`.
- **Lifecycle on retire**: when a Watchkeeper is retired, harness dumps the SQLite file → tarball → uploaded to the Notebook-archive object-store bucket (MinIO / S3). Keeper's Log event `notebook_archived` with archive URI. **Never stored in Keep** — Keep is business knowledge only.
- **Inheritance (Phase 1)**: manual operator action via `wk notebook import <new_agent> <archive-uri>`. No auto-inherit in Phase 1. Auto-policy (same-role, same-lead → auto-seed) → ⏸ Phase 2.
- **Promotion to Keep**: some entries (e.g., "team X does dailies at 10:30") are really org knowledge, not personal. Agent can propose `promote_to_keep(entry_id)` → Watchmaster approval → content moves into Keep as org-scoped knowledge. This is a natural emergent feature; implement in Phase 1 as a simple tool, auto-routed to lead approval via slack-native.
- **Risk**: agent's Notebook grows noisy. Mitigated by relevance decay + Phase 2 consolidation.
- **Physical deployment (Phase 1)**: Notebook is **not** a service — it is a library linked into the harness. Data file is per-agent. Archive bucket is an object-store service (MinIO in dev / customer S3 in prod). See IDEA-002 for full service map.

**Final disposition**: ✅ → ROADMAP. Migrated 2026-04-22.

- New M2b milestone: Notebook embedded library (SQLite + sqlite-vec), `remember` / `recall` / `forget` / archive / import, `ArchiveStore` interface with LocalFS default.
- Harness auto-injection of top-K Notebook entries per turn (M5).
- `wk notebook show | forget | export | import | archive | list` CLI (M10).
- Retire-flow archives Notebook; Keeper's Log event `notebook_archived`.
- `promote_to_keep` tool (Notebook → Keep approval flow via Watchmaster) — M6 scope.
- Consolidation, successor-inheritance auto-policy, sampling → ⏸ Phase 2.

**Depends on**: IDEA-002 disposition.
**Lands in**: pending.

---

### IDEA-001c — Reflection-on-failure / lessons learned

**Captured**: 2026-04-22
**Status**: 🔍 ANALYZING (revised after architectural correction — lessons live in Notebook, not Keep)
**Raw input**: «умеет самообучаться — какие-то тулзы не с первого раза сделал переосмыслил и потом не допускает ошибок»

**Context**: agent fails a tool invocation → reflects on why → stores a lesson in its Notebook → retrieves it next time a similar context appears → avoids the same mistake. This is the "self-learning" leg of IDEA-001.

**Analysis**

- **Builds on IDEA-001b (Notebook)**: lessons are Notebook entries with `category='lesson'`. Notebook is the substrate; this idea is the writer.
- **Trigger policy** (Phase 1, simple):
  - Tool invocation returns an error → harness auto-invokes a reflection step: "this call failed with <err>; in one paragraph, what was wrong and how to avoid it next time?"
  - Reflection output stored via `notebook.remember(category='lesson', subject=<tool_name + context_fingerprint>, content=<reflection>, evidence_log_ref=<keepers_log_id>, tool_version=<v>)`.
  - On next invocation of the same tool with a similar context, Notebook `recall` surfaces the lesson, it lands in the prompt, the agent adapts.
- **Cost control**: reflection only on error (not on success). Sampling for expensive patterns → ⏸ Phase 2.
- **False-lesson risk**: agent writes a wrong lesson and systematically avoids a correct pattern. Mitigations:
  - Every lesson has `evidence_log_ref` pointing into Keeper's Log — grounded in a real failure, not an opinion.
  - Lead sees new lessons in a digest (cadence decision below); can `forget` with a reason.
  - Heuristic flags repeated avoidance of a valid call for review.
  - **Cooling-off window**: a newly-written lesson has an `active_after` timestamp (default 24h). Until then it is visible to the lead but not auto-injected into prompts. Gives the lead a chance to veto a wrong lesson before it affects behavior.
- **Interaction with tool updates**: if a tool is hot-loaded at a new version, old-version lessons auto-mark as `needs_review` (not deleted). Agent's lesson history about a deprecated version is historical context, not active guidance.
- **Digest cadence** (open question — see bottom): either weekly to lead, or immediate+24h-delay auto-activation. The cooling-off window above is the safer option and is the preferred default.
- **Keeper's Log events** (audit trail still lives in Keep): `lesson_learned`, `lesson_recalled`, `lesson_forgotten`, `lesson_activated`, `lesson_needs_review_after_tool_update`, `lesson_promoted_to_keep`.
- **Emergent property**: repeated same-failure lessons about one tool = signal the tool itself needs fixing. Watchkeeper can escalate to lead via the self-modification flow from M9.

**Proposed disposition**: ✅ → ROADMAP, Phase 1:

- Harness adds auto-reflection on tool error (M5).
- Lesson entries include `evidence_log_ref`, `tool_version`, `active_after` (Notebook schema).
- Cooling-off window: lesson not auto-injected until `active_after` elapsed; lead can approve/veto before activation.
- Digest of new lessons pending activation → plugged into Coordinator's daily briefing (M8).
- CLI `wk notebook show | forget | activate` (M10).
- Sampling reflection on success, consolidation of similar lessons → ⏸ Phase 2.

**Depends on**: IDEA-001b (Notebook) ergo IDEA-002.
**Lands in**: ROADMAP M5 (harness auto-reflection), M2b (lesson schema fields), M8 (digest in Coordinator briefing), M10 (CLI).

---

### IDEA-002 — Keep as standalone service, Notebook as embedded per-agent store

**Captured**: 2026-04-22
**Status**: 🔍 ANALYZING (v2 — revised after operator answers on 2026-04-22)
**Raw input**: «KEEP это общие знания о компании процессах людях и всем таком. кип поднимается как отдельный сервис к которому коннектятся все боты. Но то что мы описываем — это локальные знания бота»
**Revision trigger**: «боты хуй знает где бежать будут. Если notebook это локальная база, можем вообще embedded юзать» + «приватные локально. Плюс запрашиваем пошарить — тогда в гит. Быть может зашивать гитхаб ключи чтобы от них в репе PR делать»

**Context**: current ROADMAP has "Keep = Postgres + pgvector" — a passive DB accessed via DAO from inside the core. Operator clarified that Keep is a **standalone service** with its own API, to which all bots (potentially from multiple deployments) connect; and that personal per-agent knowledge is a **separate** domain, not part of Keep. This is a foundational architectural correction.

**Analysis**

**Two distinct domains**:

| Aspect              | Keep                                                                             | Notebook                                                              |
| ------------------- | -------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| Scope               | Shared across the whole org                                                      | Private to a single Watchkeeper                                       |
| Contents            | People, processes, decisions, documents, incidents, architecture, team structure | Lessons, preferences, pending notes, observations, relationship notes |
| Ownership           | Organization                                                                     | Individual Watchkeeper (archived on retire)                           |
| Access              | All bots in the org, scoped by org/user/agent RLS                                | Only the owning agent; operator via CLI                               |
| Write authority     | Any agent with scope permission, PLUS human leads                                | The owning agent; operator for forget/prune                           |
| Lifecycle           | Permanent; survives Watchkeeper lifecycles                                       | Tied to a Watchkeeper; archived on retire                             |
| Growth              | Grows with org knowledge (large)                                                 | Grows with agent experience (small)                                   |
| Cross-agent queries | Yes (Coordinator asks Keep about team X)                                         | No (Notebook is agent-private)                                        |

**Component shape (v2)**

- **Keep service** (standalone Go process, deployed centrally, exposes API over HTTP/gRPC):
  - `keep.search(query, scope, k)` — retrieval over business knowledge.
  - `keep.store(doc, scope, meta)` — write; some scopes require approval flow.
  - `keep.subscribe(filter)` — change events.
  - `keep.log_append(event)` — write to Keeper's Log (audit).
  - `keep.log_tail(filter)` — read audit.
  - Backed by Postgres + pgvector.
  - Deployed as its own docker-compose service on the platform host. Bots connect over the network.
- **Notebook** — **embedded, not a service**:
  - Bots run on arbitrary hosts (same host as platform, different host, edge, cloud). Notebook is the bot's **local store**, co-located with the harness process.
  - Implementation: **SQLite + `sqlite-vec`** (or `libSQL` if we want replication later). Zero-config. File lives in `$WATCHKEEPER_DATA/notebook.sqlite` per agent.
  - API surface: same conceptual contract as a service would have (`remember` / `recall` / `forget` / `promote_to_keep`), but exposed as a local in-process library — no network hop.
  - **Why embedded**:
    - Zero latency for per-turn `recall` auto-injection.
    - Continues to work when the Keep service is unreachable.
    - Physical isolation: another agent cannot read this file (filesystem perms + separate agent processes).
    - No config, no connection string, no separate backup system needed at runtime.
  - **Archive on retire**: harness dumps the SQLite file → tarball → upload to object store (S3 / local backup bucket). **Not stored in Keep** — Keep is for business knowledge only. A separate `notebook_archive` object-store bucket is the right home.
  - **Backup**: periodic snapshot of the SQLite file to the same object store.
  - **Operator CLI `wk notebook show <agent>`**: core reaches the harness over the existing JSON-RPC channel (M5) and asks it to read-dump the requested entries. No remote file access.
  - **Inheritance / resurrection**: operator-driven via `wk notebook import <new_agent> <archive.tar>` (Phase 1). Auto-inheritance policy (e.g., same-role auto-seed) → ⏸ Phase 2.
  - **`promote_to_keep`**: the one place Notebook calls Keep's API — to propose that a personal note become org knowledge (goes through Watchmaster approval).
- **Keeper's Log**: belongs to Keep (organizational audit, not personal). Notebook events that need auditing are published via `keep.log_append` from the harness — Notebook itself has no audit surface; Keep is the single audit authority.

**Private Tool storage (revision of M9 `hosted` mode)**

Previous draft put hosted private tools in the Keep database. Revised: **Keep is for business knowledge only; tools are not business knowledge.**

- **Private tools live on local filesystem** at `$DATA_DIR/tools/private/` on the platform host. Each tool is a folder with `manifest.json` + `tool.ts` + tests + (optional) signature.
- Same overlay resolver as before (built-in → platform → private → local-patch); the private layer is now a filesystem path, not a DB table.
- **Upgrade safety**: platform upgrade touches the core binary and the `tools-builtin/` that ships with it. `$DATA_DIR/tools/private/` is a persistent volume mounted into docker-compose — platform upgrades never touch it.
- **`promote_share_tool` flow** — how a private tool graduates to a git-backed source:
  - Agent or lead initiates `propose_share_tool(tool_id, target ∈ {platform, customer_git_repo})`.
  - Core reads the tool folder from `$DATA_DIR/tools/private/<tool>/`, creates a branch in the target git repo, opens a PR.
  - Auth for the git operation uses **credentials baked into the deployment** — either:
    - **GitHub App** installed on the target repo (recommended: scoped, revocable, auditable per-install).
    - **PAT** (bot-account personal access token) stored in the secrets interface (simpler for MVP; less auditable).
  - Phase 1 supports both; operator picks at config time. Credentials live in the core's secrets interface — no vendor env vars in code.
  - Keeper's Log events: `tool_share_proposed`, `tool_share_pr_opened`, `tool_share_pr_merged` (on webhook), `tool_share_pr_rejected`.
- **What if the agent lacks `share` capability**: `propose_share_tool` is a privileged capability, default **off**; lead explicitly grants it when trust is built. Until then, lead can manually stage any private tool into a git repo via `wk tool export <name> --to-repo <url>`.

**Why this split**

- **Keep central, Notebook embedded** matches the actual data profile: Keep is an org-wide, cross-agent, cross-session queryable knowledge store (needs a real server); Notebook is per-agent, co-located, single-writer, latency-sensitive (embedded is strictly better).
- **Blast radius**: Notebook running in-process with the bot means "bot healthy ⟺ bot's memory healthy" — no partial-outage mode where a bot is up but amnesiac. Keep outage degrades cross-agent queries but per-bot work continues.
- **Edge-ready**: bots on arbitrary hosts (edge, cloud, on-prem) do not need to carry their own Postgres; SQLite + sqlite-vec is a single file.
- **Security**: Notebook access is enforced by OS filesystem permissions and harness process isolation — no network credentials to manage per agent. Keep access is network-authenticated via short-lived capability tokens from the core.
- **Dual-language principle** (business concept): privileged organizational operations (Keep writes, audit) remain in the compiled core; personal operations (Notebook) remain local to the agent's process space where their scope is physically bounded.

**Physical deployment (Phase 1)**

- `docker-compose` adds **one** network-visible service: `keep` (Go binary + Postgres). Harness processes (every Watchkeeper) connect to Keep over HTTP/gRPC.
- Notebook is **not** a docker-compose service. It is a library compiled into the harness; its data lives in a per-agent volume `$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite` mounted into the harness container.
- Object store for Notebook archives: one more compose service (`minio` or similar S3-compatible) — small, single instance; operator can point to external S3 for production.
- No direct DB access from harness or from Watchkeeper runtime to Keep — always through Keep API.
- Core's lifecycle manager, spawn flow, and Watchmaster go through Keep API, not direct DAO. This is a refactor of current M2/M3 scope.

**Manifest lives in git, not Keep** (from business concept). Unchanged. Manifest source-of-truth is version-controlled; Keep may cache a rendered copy for retrieval but authority is git.

**Impact on ROADMAP if accepted** (not writing into ROADMAP yet — pending confirmation):

- **M2** renames to "Keep service" — schema, API, RLS, Keeper's Log, outbox. No Notebook here.
- **New M2b (small)** — Notebook library (embedded SQLite + sqlite-vec), archive/import tools, operator CLI surface. Small milestone, 2–3 days, sits between M2 and M3.
- **M3** (Go core services) loses direct-DB access patterns; every Keep touch goes through Keep client package.
- **M5** (Harness): agent harness links the Notebook library directly (in-process); opens a Keep client for organizational queries. Auto-injection uses Notebook; explicit Keep search remains an intentional tool call.
- **M7** (Spawn Flow): spawn provisions a per-agent Notebook file and a Keep-side `watchkeeper` row; on retire, archives the Notebook file to object store.
- **M9** (Tool Registry): `hosted` mode is now **filesystem-based**, not DB-backed. Much simpler. Private tools sit at `$DATA_DIR/tools/private/` on the platform host; `promote_share_tool` with baked git credentials handles graduation to git sources.
- **M10** (CLI/ops): `wk notebook show | forget | export | import`, `wk notebook archive | list`, plus Keep-side operator commands. Object-store backup added to runbook alongside Postgres backup.
- docker-compose gains `keep` and `minio` (or similar object store) services; Notebook is not a service.
- Makefile targets: `make keep-*`, `make notebook-*` (operate on local per-agent files).
- Keeper's Log remains the single audit authority, owned by Keep.

**Operator answers (2026-04-22)**

1. **Two processes from day one** — ✅ accepted.
2. **Postgres clustering** — moot now: Notebook is embedded, not Postgres. Keep has its own Postgres.
3. **Hosted Tool Registry** — revised: lives in **filesystem** (`$DATA_DIR/tools/private/`), not in Keep. Promotion to shared git sources via baked GitHub credentials + auto-PR.
4. **`deployment_id`** — pending (see open question below).
5. **Notebook inheritance** — recommended Phase 1: archived on retire, manual import only; auto-inherit → Phase 2.
6. **Language** — separate field on Manifest (applies to IDEA-001a).

**Operator answers to A–D (2026-04-22)**

- **A — `deployment_id` column**: ❌ rejected. Operator framed Keep as business knowledge of the org the bots serve, not infrastructure metadata. `deployment_id` is an infra concept; it has no place in business schema. If an org ever needs staging-vs-prod isolation, they run **two separate Keep instances** — Keep's schema stays pure-domain.
- **B — `promote_share_tool` auth**: ✅ both supported. PAT is the Phase 1 quickstart default; GitHub App is documented as the production-recommended path (scoped, revocable, auditable per-install).
- **C — object store**: ✅ revised. No MinIO (license concerns). Phase 1 default is **local filesystem** (`$DATA_DIR/archives/notebook/`) as a docker-compose volume. `ArchiveStore` Go interface with two implementations: `LocalFS` (default) and `S3Compatible` (points at any S3-API endpoint — AWS S3, Cloudflare R2, Wasabi, SeaweedFS, Garage, customer-owned). No proprietary dependency in core.
- **D — Notebook engine**: ✅ SQLite + `sqlite-vec` locked for Phase 1.

**Final disposition**: ✅ → ROADMAP. Migrated 2026-04-22.

- M2 scope tightened to Keep service only.
- New M2b milestone added for Notebook embedded library.
- M5/M6/M7/M8/M9/M10 revised accordingly.
- §2 Architectural Decisions and §3 Scope In updated.
- §5 Risks gained three entries (SQLite file corruption, IP leakage via promotion, PAT compromise).
- §6 Cross-cutting constraint added: Keep contains only business knowledge of the organization; infra metadata lives elsewhere.
- §8 External Prerequisites gained credentials item.

**Landed in**: ROADMAP-phase1.md — see commit/revision 2026-04-22 (third pass).

---

---
