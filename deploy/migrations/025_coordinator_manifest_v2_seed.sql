-- Watchkeeper Keep — Coordinator manifest_version V2 seed (M8.2.b).
--
-- INSERTs a NEW `manifest_version` row (version_no=2) for the same
-- Coordinator manifest seeded by migration 024 (M8.2.a). The new row
-- supersedes the version_no=1 baseline because keepclient.GetManifest
-- returns the manifest_version row with the highest version_no for a
-- given manifest_id (`core/pkg/keepclient/read_manifest.go:63-67`).
-- Migration 024 stays untouched so the M8.2.a baseline is recoverable
-- from the migration sequence alone, mirroring the append-only
-- semantics of the `manifest_version` table (UNIQUE (manifest_id,
-- version_no), `002_core_business_tables.sql:48`).
--
-- Toolset extension: V1 carried `[update_ticket_field]`; V2 carries
-- `[update_ticket_field, find_overdue_tickets]`. Authority matrix
-- extension: V1 had `update_ticket_field=self` +
-- `manifest_version_bump=lead`; V2 adds `find_overdue_tickets=self`
-- (read-only; the M5.5/runtime ACL gate consults the matrix per call,
-- and `self` means the runtime dispatches without out-of-band
-- approval). System prompt, personality, model, autonomy, and notebook
-- recall tunables are unchanged from V1 — the role definition and
-- reassignment boundary still hold; only the toolset surface grows.
--
-- Stable UUIDs — declared inline so test fixtures and the
-- `CoordinatorManifestVersionV2ID` Go constant
-- (`core/pkg/manifest/coordinator.go`) reference the same id across
-- deploys:
--
--   * manifest.id            20000000-0000-4000-8000-000000000000
--                            (REUSED from migration 024 — the same
--                            Coordinator manifest; this seed only
--                            adds a new version_no=2 row beneath it).
--   * manifest_version.id    22000000-0000-4000-8000-000000000000
--                            (V2 row; mirrored as
--                            `CoordinatorManifestVersionV2ID`).
--
-- Idempotent: the INSERT uses `ON CONFLICT (id) DO NOTHING` so a re-run
-- on a DB that already carries the V2 seed is a no-op rather than a
-- unique-violation error. The (manifest_id, version_no) UNIQUE
-- constraint also guarantees a second seed attempt with a different id
-- would fail loudly rather than silently spawning a duplicate
-- version_no=2.
--
-- See `docs/ROADMAP-phase1.md` §M8 → M8.2 → M8.2.b, the
-- `core/pkg/runtime/authority.go` vocabulary reference, the M8.1
-- lesson entry in `docs/lessons/M8.md` for the M8.1 jira adapter the
-- handler consumes, and the M8.2.a lesson entry for the V1 baseline
-- this V2 row supersedes.

-- +goose Up
-- Defensive: pin the org GUC for the duration of this migration so the
-- INSERT into `manifest_version` (RLS-FORCEd via migration 013's
-- propagation onto manifest_version) succeeds even when a non-superuser
-- runs goose. Mirrors the migration 017 / 024 discipline.
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
  '22000000-0000-4000-8000-000000000000',
  '20000000-0000-4000-8000-000000000000',
  2,
  -- System prompt is unchanged from V1 (migration 024). The
  -- reassignment boundary phrase "NEVER reassign tickets" and the
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
  || E' would-be leaks as a question to the lead.'
  -- M8.2.b appendix: narrative guidance for the new read tool added
  -- in V2. The asserted phrase "find_overdue_tickets to surface"
  -- is pinned by the V2 migration-shape test so a future reword
  -- without test update fails CI, not production.
  || E'\n\nReading tools: use find_overdue_tickets to surface'
  || E' tickets assigned to a teammate that have not been updated'
  || E' recently. Required args: project_key (Atlassian project),'
  || E' assignee_account_id (Atlassian Cloud accountId), status'
  || E' (array of workflow status names to include),'
  || E' age_threshold_days (integer 1..365). The handler caps the'
  || E' result at 200 issues across 10 pages and returns'
  || E' truncated=true when the cap fires; narrow the scope or'
  || E' lower the threshold and re-run on truncation.',
  -- Toolset V2: adds `find_overdue_tickets` (M8.2.b read tool) to the
  -- V1 baseline. M8.2.c will land Slack tools as a V3 row;
  -- M8.2.d will land `find_stale_prs` as a V4 row.
  '[
    {"name": "update_ticket_field"},
    {"name": "find_overdue_tickets"}
  ]'::jsonb,
  -- Authority matrix V2: adds `find_overdue_tickets=self` (read-only,
  -- safe to dispatch without approval). V1 entries unchanged.
  '{
    "update_ticket_field": "self",
    "find_overdue_tickets": "self",
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
WHERE id = '22000000-0000-4000-8000-000000000000';

-- NOTE: the V1 manifest_version (id 21000000-…) and the manifest
-- (id 20000000-…) both belong to migration 024; this Down only
-- removes the V2 row this migration introduced.
