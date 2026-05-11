-- Watchkeeper Keep — Coordinator manifest_version V4 seed (M8.2.d).
--
-- INSERTs a NEW `manifest_version` row (version_no=4) for the same
-- Coordinator manifest seeded by migration 024 (M8.2.a). The new row
-- supersedes the version_no=3 baseline because keepclient.GetManifest
-- returns the manifest_version row with the highest version_no for a
-- given manifest_id (`core/pkg/keepclient/read_manifest.go:63-67`).
-- Migrations 024 / 025 / 026 stay untouched so the M8.2.a / M8.2.b /
-- M8.2.c baselines are recoverable from the migration sequence
-- alone, mirroring the append-only semantics of the
-- `manifest_version` table.
--
-- Toolset extension: V3 carried `[update_ticket_field,
-- find_overdue_tickets, fetch_watch_orders, nudge_reviewer,
-- post_daily_briefing]`; V4 carries the same plus `find_stale_prs`.
-- Authority matrix extension: V3 had every non-bump tool granted
-- `self`; V4 adds `find_stale_prs=self` (read-only GitHub PR scan
-- under the bot's own GitHub identity, no impersonation).
-- System prompt extends with narrative guidance for the new tool;
-- personality, model, autonomy, and notebook recall tunables are
-- unchanged from V3.
--
-- Stable UUIDs — declared inline so test fixtures and the
-- `CoordinatorManifestVersionV4ID` Go constant
-- (`core/pkg/manifest/coordinator.go`) reference the same id across
-- deploys:
--
--   * manifest.id            20000000-0000-4000-8000-000000000000
--                            (REUSED from migration 024 — the same
--                            Coordinator manifest; this seed only
--                            adds a new version_no=4 row beneath it).
--   * manifest_version.id    24000000-0000-4000-8000-000000000000
--                            (V4 row; mirrored as
--                            `CoordinatorManifestVersionV4ID`).
--
-- Idempotent: the INSERT uses `ON CONFLICT (id) DO NOTHING` so a
-- re-run on a DB that already carries the V4 seed is a no-op rather
-- than a unique-violation error. The (manifest_id, version_no)
-- UNIQUE constraint also guarantees a second seed attempt with a
-- different id would fail loudly rather than silently spawning a
-- duplicate version_no=4.
--
-- See `docs/ROADMAP-phase1.md` §M8 → M8.2 → M8.2.d, the
-- `core/pkg/runtime/authority.go` vocabulary reference, the M8.2.d
-- lesson entry in `docs/lessons/M8.md` for the `core/pkg/github`
-- adapter the handler consumes, and the M8.2.c lesson entry for the
-- V3 baseline this V4 row supersedes.

-- +goose Up
-- Defensive: pin the org GUC for the duration of this migration so
-- the INSERT into `manifest_version` (RLS-FORCEd via migration 013's
-- propagation onto manifest_version) succeeds even when a
-- non-superuser runs goose. Mirrors the migration 017 / 024 / 025 /
-- 026 discipline.
SELECT set_config('watchkeeper.org', '00000000-0000-4000-8000-000000000000', true);

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
  '24000000-0000-4000-8000-000000000000',
  '20000000-0000-4000-8000-000000000000',
  4,
  -- System prompt extends V3 with narrative guidance for the new
  -- GitHub tool. V1/V2/V3 phrases are PRESERVED verbatim because the
  -- V4 migration-shape test asserts them as load-bearing literals
  -- (see `core/pkg/manifest/coordinator_seed_test.go`). The string is
  -- split across `||` concatenations to satisfy sqlfluff's 120-char
  -- line cap.
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
  || E' would-be leaks as a question to the lead.'
  -- M8.2.b appendix — preserved verbatim.
  || E'\n\nReading tools: use find_overdue_tickets to surface'
  || E' tickets assigned to a teammate that have not been updated'
  || E' recently. Required args: project_key (Atlassian project),'
  || E' assignee_account_id (Atlassian Cloud accountId), status'
  || E' (array of workflow status names to include),'
  || E' age_threshold_days (integer 1..365). The handler caps the'
  || E' result at 200 issues across 10 pages and returns'
  || E' truncated=true when the cap fires; narrow the scope or'
  || E' lower the threshold and re-run on truncation.'
  -- M8.2.c appendix: narrative guidance for the three Slack tools.
  || E'\n\nSlack inbox: use fetch_watch_orders to read recent'
  || E' direct messages from the lead. Required args: lead_user_id'
  || E' (Slack user id matching [UWB][A-Z0-9]+), lookback_minutes'
  || E' (integer 1..1440). The handler resolves the 1:1 IM channel'
  || E' via conversations.open and reads the history; caps at 200'
  || E' messages across 10 pages. Use this when the lead has'
  || E' likely DM''d new orders since your last turn.'
  || E'\n\nReviewer nudges: use nudge_reviewer to DM a teammate'
  || E' about a stale review. Required args: reviewer_user_id'
  || E' (Slack user id), text (≤2000 chars). Optional: title (≤200'
  || E' chars). Slack auto-opens the DM; you do NOT need to resolve'
  || E' the channel id first. Compose the nudge as a SINGLE,'
  || E' actionable message — link to the PR + the asked action.'
  || E' Avoid daily spam: nudge at most once per reviewer per PR'
  || E' per 24h window.'
  || E'\n\nDaily briefing: use post_daily_briefing to post a'
  || E' structured daily summary to a configured channel.'
  || E' Required args: channel_id (Slack C…/G…/D… channel),'
  || E' title (≤200 chars, non-empty), sections (array of'
  || E' {heading, bullets}, ≤20 sections, each ≤20 bullets ≤1000'
  || E' chars). The handler caps the rendered text at 8000 chars'
  || E' and refuses overflow; trim sections when the limit fires.'
  -- M8.2.d appendix: narrative guidance for the new GitHub tool.
  -- Phrase "find_stale_prs to surface" is asserted verbatim by the
  -- V4 migration-shape test.
  || E'\n\nGitHub PRs: use find_stale_prs to surface pull requests'
  || E' awaiting a teammate''s review for too long. Required args:'
  || E' repo_owner (GitHub user/org login), repo_name (repository'
  || E' name), reviewer_login (GitHub login of the reviewer to'
  || E' filter by), age_threshold_days (integer 1..365). The'
  || E' handler scans open PRs in the repo, filters to those where'
  || E' the supplied reviewer is in the requested-reviewers list AND'
  || E' the PR has not been updated in more than the threshold; caps'
  || E' at 200 PRs across 10 pages. Reviewer login matching is'
  || E' case-insensitive. Use this when composing the daily briefing'
  || E' or deciding which reviewer to nudge.',
  -- Toolset V4: V3 entries preserved; M8.2.d adds find_stale_prs.
  '[
    {"name": "update_ticket_field"},
    {"name": "find_overdue_tickets"},
    {"name": "fetch_watch_orders"},
    {"name": "nudge_reviewer"},
    {"name": "post_daily_briefing"},
    {"name": "find_stale_prs"}
  ]'::jsonb,
  -- Authority matrix V4: V3 entries preserved; M8.2.d adds the new
  -- entry. find_stale_prs is read-only (self) — same shape as the
  -- other read tools.
  '{
    "update_ticket_field": "self",
    "find_overdue_tickets": "self",
    "fetch_watch_orders": "self",
    "nudge_reviewer": "self",
    "post_daily_briefing": "self",
    "find_stale_prs": "self",
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
WHERE id = '24000000-0000-4000-8000-000000000000';

-- NOTE: the V1 / V2 / V3 manifest_version rows and the manifest
-- itself belong to migrations 024 / 025 / 026; this Down only
-- removes the V4 row this migration introduced.
