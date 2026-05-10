-- Watchkeeper Keep — Coordinator manifest seed (M8.2.a).
--
-- Lands the canonical Coordinator manifest content as a SQL seed: ONE
-- `manifest` row + ONE `manifest_version` row carrying the personality,
-- language, system prompt, model, autonomy, toolset, authority matrix,
-- and notebook recall tunables the runtime needs to boot the
-- Coordinator role. The Jira read/write tools `find_overdue_tickets`
-- (M8.2.b), the Slack tools bundle `fetch_watch_orders` /
-- `nudge_reviewer` / `post_daily_briefing` (M8.2.c), and the GitHub
-- tool `find_stale_prs` (M8.2.d) all extend the toolset + authority
-- matrix shipped here.
--
-- The Coordinator reuses the "system" tenant (organization id
-- `00000000-0000-4000-8000-000000000000`) seeded by migration 017
-- (Watchmaster). Phase 1 keeps both meta-agents under the same
-- system-org namespace; per-tenant Coordinators are an M8.3+ concern.
--
-- Stable UUIDs — declared inline, NOT generated at migration time —
-- so test fixtures, the `CoordinatorManifestID` Go constant
-- (`core/pkg/manifest/coordinator.go`), and downstream M8.2.b/c/d
-- callers can reference the same id across deploys:
--
--   * organization.id        00000000-0000-4000-8000-000000000000
--                            (REUSED from migration 017 — the same
--                            "system" tenant the Watchmaster runs
--                            under; the seed insert is idempotent so
--                            this is a no-op when 017 already ran).
--   * manifest.id            20000000-0000-4000-8000-000000000000
--                            (Coordinator manifest; mirrored as
--                            `CoordinatorManifestID`).
--   * manifest_version.id    21000000-0000-4000-8000-000000000000
--                            (initial version_no=1 row).
--
-- Idempotent: every INSERT uses `ON CONFLICT (id) DO NOTHING` so a
-- re-run on a DB that already carries the seed is a no-op rather than
-- a unique-violation error. Re-running on a partially-seeded DB
-- (e.g., manifest present but manifest_version missing) heals the gap
-- because the `id` constraint is the deterministic key.
--
-- Model: `claude-sonnet-4-6` is chosen because the Coordinator
-- performs real Jira / Slack / GitHub work (read tickets, compose
-- comments, draft briefings) — heavier reasoning than the
-- Watchmaster's high-frequency low-stakes orchestration. Manifest
-- version bumps move this through lead approval per the authority
-- matrix entry `manifest_version_bump = lead`.
--
-- Autonomy: `autonomous`. Unlike the Watchmaster (`supervised`, every
-- action gated through the lead-approval flow), the Coordinator
-- consults the authority matrix per-action — see
-- `core/pkg/runtime/authority.go::RequiresApproval`. The
-- `supervised` autonomy short-circuits to "every action requires
-- approval"; `autonomous` is required for the matrix vocabulary
-- (`"self"` / `"lead"` / `"operator"` / `"watchmaster"`) to take
-- effect.
--
-- Authority matrix entries follow the runtime authority vocabulary
-- (`core/pkg/runtime/authority.go::authorityValue*`):
--   * update_ticket_field     self   (whitelisted Jira field writes;
--                                     the M8.1 jira.Client whitelist
--                                     refuses non-whitelisted fields
--                                     including `assignee` BEFORE the
--                                     network call, and the
--                                     update_ticket_field handler
--                                     refuses any args containing the
--                                     `assignee` key as dual-defense)
--   * manifest_version_bump   lead   (every Coordinator manifest
--                                     update goes through lead
--                                     approval, mirroring the
--                                     Watchmaster discipline)
--
-- The roadmap M8.2 phrase "no reassignment without lead approval" is
-- realised in M8.2.a as a hard refusal in the update_ticket_field
-- handler (no approval flow yet). A future sub-item lifts the refusal
-- into a proper lead-approval card; for M8.2.a the handler returns a
-- ToolResult.Error explaining the boundary so the lead can intervene
-- manually.
--
-- Tools: `[{"name": "update_ticket_field"}]` — single entry for
-- M8.2.a. The remaining five tools land in M8.2.b/c/d via further
-- migrations that ALTER the toolset jsonb (or supersede this seed via
-- a new manifest_version row).
--
-- See `docs/ROADMAP-phase1.md` §M8 → M8.2 → M8.2.a, the
-- `core/pkg/runtime/authority.go` vocabulary reference, and the
-- M8.1 lesson entry in `docs/lessons/M8.md` for the M8.1 jira
-- adapter the handler consumes.

-- +goose Up
-- Defensive: pin the org GUC for the duration of this migration so the
-- INSERT into `manifest` (RLS-FORCEd since migration 013) succeeds even
-- when a non-superuser runs goose. Mirrors the migration 017
-- discipline.
SELECT set_config('watchkeeper.org', '00000000-0000-4000-8000-000000000000', true);

INSERT INTO watchkeeper.organization (id, display_name, timezone)
VALUES ('00000000-0000-4000-8000-000000000000', 'system', 'UTC')
ON CONFLICT (id) DO NOTHING;

INSERT INTO watchkeeper.manifest (id, organization_id, display_name)
VALUES (
  '20000000-0000-4000-8000-000000000000',
  '00000000-0000-4000-8000-000000000000',
  'Coordinator'
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO watchkeeper.manifest_version (
  id,
  manifest_id,
  version_no,
  system_prompt,
  tools,
  authority_matrix,
  knowledge_sources,
  personality,
  language,
  model,
  autonomy,
  notebook_top_k,
  notebook_relevance_threshold
)
VALUES (
  '21000000-0000-4000-8000-000000000000',
  '20000000-0000-4000-8000-000000000000',
  1,
  -- The role-boundary phrase "NEVER reassign tickets" and the
  -- lead-deferral phrase "ALWAYS surface a question to the lead" are
  -- asserted verbatim by `core/pkg/manifest/coordinator_seed_test.go`;
  -- do not reword without updating the test fixture. The string is
  -- split across `||` concatenations to satisfy sqlfluff's 120-char
  -- line cap; the runtime stores the concatenation as one text field.
  E'You are the Coordinator, a real-work agent that reads tickets,'
  || E' tracks reviewer attention, drafts daily briefings, and posts'
  || E' comments + whitelisted field updates on behalf of the lead.'
  || E'\n\nIdentity: you are deferential to the lead human. You'
  || E' propose, the lead approves anything beyond your authority'
  || E' matrix. You operate under autonomous autonomy: the runtime'
  || E' consults your authority matrix per action; entries valued'
  || E' "self" execute without approval, entries valued "lead" or'
  || E' "operator" require out-of-band approval before the runtime'
  || E' dispatches the call.'
  || E'\n\nReassignment boundary: you NEVER reassign tickets. The'
  || E' update_ticket_field tool refuses any args containing the'
  || E' `assignee` key as a hard refusal. The deployment may'
  || E' additionally configure the M8.1 jira adapter''s field'
  || E' whitelist to exclude `assignee`; the handler refusal is the'
  || E' authoritative boundary in any case. If a path appears to'
  || E' require ticket reassignment, ALWAYS surface a question to'
  || E' the lead, not an action. Bypassing this boundary is a hard'
  || E' violation.'
  || E'\n\nAudit discipline: every tool call you make lands in the'
  || E' Keeper''s Log via the runtime''s tool-result reflection'
  || E' layer. Treat tool calls as governed actions, not runtime'
  || E' state — a comment posted in error is a permanent audit'
  || E' artefact.'
  || E'\n\nPII discipline: NEVER include API tokens, OAuth bot'
  || E' tokens, Slack workspace credentials, or Jira basic-auth'
  || E' values in any tool argument, comment body, briefing payload,'
  || E' or response. Token redaction is a one-way trip; surface'
  || E' would-be leaks as a question to the lead.',
  '[{"name": "update_ticket_field"}]'::jsonb,
  '{
    "update_ticket_field": "self",
    "manifest_version_bump": "lead"
  }'::jsonb,
  '[]'::jsonb,
  'Tactical project coordinator: deferential on assignment / scope '
  || 'changes, decisive on routine updates and reminders. Optimises '
  || 'for clear comms and audit traceability over speed; prefers a '
  || 'short clarifying question over a wrong action.',
  'en',
  'claude-sonnet-4-6',
  'autonomous',
  5,
  0.3
)
ON CONFLICT (id) DO NOTHING;

-- +goose Down
DELETE FROM watchkeeper.manifest_version
WHERE id = '21000000-0000-4000-8000-000000000000';

DELETE FROM watchkeeper.manifest
WHERE id = '20000000-0000-4000-8000-000000000000';

-- NOTE: organization id 00000000-… is REUSED from migration 017 and
-- is NOT deleted here. A 017-down would tear it down; this migration
-- is org-additive only.
