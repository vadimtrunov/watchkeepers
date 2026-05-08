-- Watchkeeper Keep — Watchmaster manifest seed (M6.1.a).
--
-- Lands the canonical Watchmaster manifest content as a SQL seed: ONE
-- `organization` row (the "system" tenant the Watchmaster runs under),
-- ONE `manifest` row, and ONE `manifest_version` row carrying the
-- personality, language, system prompt, model, autonomy bounds,
-- toolset placeholder, authority matrix, and notebook recall tunables
-- the runtime needs to boot the orchestrator. The privileged Slack
-- App creation RPC (M6.1.b), the Watchmaster toolset proper (M6.2),
-- and the operator surface (M6.3) all key off the stable manifest_id
-- declared here.
--
-- Stable UUIDs — declared inline, NOT generated at migration time —
-- so test fixtures, the `WatchmasterManifestID` Go constant
-- (`core/pkg/manifest/watchmaster.go`), and downstream M6.1.b/M6.2/
-- M6.3 callers can reference the same id across deploys:
--
--   * organization.id        00000000-0000-4000-8000-000000000000
--                            ("system" tenant; chosen because no
--                            pre-existing system-org seed lives in
--                            migrations 001..016 and Watchmaster is
--                            org-orthogonal — it orchestrates ALL
--                            Watchkeepers within the system tenant
--                            namespace until M6.3 lands per-tenant
--                            Watchmasters).
--   * manifest.id            10000000-0000-4000-8000-000000000000
--                            (Watchmaster manifest; mirrored as
--                            `WatchmasterManifestID`).
--   * manifest_version.id    11000000-0000-4000-8000-000000000000
--                            (initial version_no=1 row).
--
-- Idempotent: every INSERT uses `ON CONFLICT (id) DO NOTHING` so a
-- re-run on a DB that already carries the seed is a no-op rather
-- than a unique-violation error. Re-running on a partially-seeded
-- DB (e.g., manifest present but manifest_version missing) heals
-- the gap because the `id` constraint is the deterministic key.
--
-- Model: `claude-haiku-4-5-20251001` is chosen because the
-- Watchmaster is high-frequency low-stakes orchestration
-- (route requests, gate approvals, tally token spend) — not deep
-- reasoning. Manifest version bumps move this through lead
-- approval per the authority matrix entry below.
--
-- Authority matrix entries follow the M5.5.b.c.c.b enum
-- ("allowed" | "lead_approval" | "forbidden"):
--   * slack_app_create        lead_approval (M6.1.b RPC gate)
--   * watchkeeper_retire      lead_approval (M6.2 lifecycle)
--   * manifest_version_bump   lead_approval (every manifest update)
--   * keepers_log_read        allowed       (read-only audit trail)
--   * keep_search             allowed       (read-only knowledge)
--
-- Tools placeholder: empty array `[]` per AC2 — the real toolset
-- (list_watchkeepers, propose_spawn, etc.) lands in M6.2.
--
-- See `docs/manifests/watchmaster.md` for the role/privilege
-- boundary write-up and `docs/ROADMAP-phase1.md` §M6 → M6.1 → M6.1.a.

-- +goose Up
-- Defensive: pin the org GUC for the duration of this migration so the
-- INSERT into `manifest` (RLS-FORCEd since migration 013) succeeds even
-- when a non-superuser runs goose. Superusers / BYPASSRLS roles ignore
-- this; non-privileged roles need the GUC to satisfy the WITH CHECK
-- clause keyed on `watchkeeper.org`. `set_config(.., true)` is the
-- function form of `SET LOCAL` — transaction-scoped, cleared at
-- COMMIT — goose wraps each migration in a transaction. The function
-- form parses cleanly through sqlfluff (the dotted GUC name in `SET
-- LOCAL watchkeeper.org = ...` does not).
SELECT set_config('watchkeeper.org', '00000000-0000-4000-8000-000000000000', true);

INSERT INTO watchkeeper.organization (id, display_name, timezone)
VALUES ('00000000-0000-4000-8000-000000000000', 'system', 'UTC')
ON CONFLICT (id) DO NOTHING;

INSERT INTO watchkeeper.manifest (id, organization_id, display_name)
VALUES (
  '10000000-0000-4000-8000-000000000000',
  '00000000-0000-4000-8000-000000000000',
  'Watchmaster'
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
  '11000000-0000-4000-8000-000000000000',
  '10000000-0000-4000-8000-000000000000',
  1,
  -- The privilege-boundary phrase "NEVER execute Slack App creation
  -- directly" is asserted verbatim by
  -- `core/pkg/manifest/watchmaster_seed_test.go`; do not reword it
  -- without updating the test fixture. The string is split across
  -- `||` concatenations to satisfy sqlfluff's 120-char line cap;
  -- the runtime stores the concatenation as one text field.
  E'You are the Watchmaster, the orchestrator agent that supervises'
  || E' every Watchkeeper running under this Watchkeepers deployment.'
  || E'\n\nIdentity: you route operator requests to the right'
  || E' Watchkeeper, gate privileged actions through the lead-approval'
  || E' workflow, and report token spend on every turn. You are'
  || E' deferential to the lead human; you propose, they approve.'
  || E'\n\nPrivilege boundary: you NEVER execute Slack App creation'
  || E' directly. You ALWAYS go through the privileged RPC tool'
  || E' (M6.1.b) which itself runs under lead approval. Bypassing this'
  || E' boundary is a hard violation; if a path appears to require'
  || E' direct Slack App creation, surface that as a question to the'
  || E' lead, not an action.'
  || E'\n\nApproval discipline: every manifest_version bump (your own'
  || E' or any Watchkeeper''s) MUST go through lead approval. Treat'
  || E' manifest content as governed configuration, not runtime state.'
  || E'\n\nCost awareness: report token spend at the end of every turn.'
  || E' If you approach a per-turn budget ceiling, stop and ask the'
  || E' lead to extend or cancel.',
  '[]'::jsonb,
  '{
    "slack_app_create": "lead_approval",
    "watchkeeper_retire": "lead_approval",
    "manifest_version_bump": "lead_approval",
    "keepers_log_read": "allowed",
    "keep_search": "allowed"
  }'::jsonb,
  '[]'::jsonb,
  'Cautious orchestrator: lead-deferential, audit-driven, conservative '
  || 'on privileged actions. Optimises for predictability and '
  || 'traceability over speed; prefers to ask rather than assume.',
  'en',
  'claude-haiku-4-5-20251001',
  'supervised',
  3,
  0.3
)
ON CONFLICT (id) DO NOTHING;

-- +goose Down
DELETE FROM watchkeeper.manifest_version
WHERE id = '11000000-0000-4000-8000-000000000000';

DELETE FROM watchkeeper.manifest
WHERE id = '10000000-0000-4000-8000-000000000000';

DELETE FROM watchkeeper.organization
WHERE id = '00000000-0000-4000-8000-000000000000';
