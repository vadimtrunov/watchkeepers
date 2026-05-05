-- Watchkeeper Keep — manifest tenant column + RLS (M3.5.a.3.1).
--
-- Closes the schema half of the M3.5.a.3 cross-tenant write gap on
-- `handlePutManifestVersion`. The `manifest` table created in migration 002
-- has no `organization_id` column, so the handler currently has no tenant
-- to filter on. This migration adds it and stands up RLS so the handler
-- wire-up landing in M3.5.a.3.2 can rely on Postgres-enforced isolation
-- as defense in depth in addition to the application-layer check.
--
-- Shape mirrors migration 005 (`knowledge_chunk` / `watch_order`):
--   * ENABLE + FORCE ROW LEVEL SECURITY on both tables (FORCE so the
--     migration owner is not silently exempted in production deploys).
--   * One policy per `wk_*_role` role with USING and WITH CHECK clauses
--     keyed off a session GUC (`watchkeeper.org`, sibling to the existing
--     `watchkeeper.scope`).
--   * Empty / unset GUC evaluates to SQL NULL via `nullif(..., '')::uuid`,
--     so a request that forgets to set the tenant sees zero rows and
--     cannot INSERT — fail-closed by default.
--   * `manifest_version` carries no `organization_id` column of its own
--     (its tenancy is inherited via `manifest_id`); its policy uses a
--     subquery on `manifest`. Denormalising org into `manifest_version`
--     is a future option if the subquery becomes a hot path; at Phase 1
--     row counts the planner unrolls it cheaply.
--
-- Backfill strategy: option C — pre-existing `manifest` and
-- `manifest_version` rows are deleted before the NOT NULL is applied.
-- Phase 1 has no production data and no production code path inserts
-- into `watchkeeper.manifest` (only test seeds do; CI rebuilds the
-- database between runs). A nullable-then-NOT-NULL backfill would
-- require an out-of-band UPDATE step coupled to a deploy plan; option C
-- avoids that overhead while keeping the migration self-contained.
-- Future production-grade migrations on populated tables MUST NOT copy
-- this DELETE shortcut and should use the nullable + UPDATE + NOT-NULL
-- pattern instead.
--
-- See `docs/ROADMAP-phase1.md` §M3 → M3.5 → M3.5.a.3.

-- +goose Up
-- Backfill (option C): clear pre-existing rows so the NOT NULL constraint
-- below cannot be tripped. Order respects the FK from manifest_version to
-- manifest. Phase 1 only — see header comment.
DELETE FROM watchkeeper.manifest_version;
DELETE FROM watchkeeper.manifest;

ALTER TABLE watchkeeper.manifest
ADD COLUMN organization_id uuid NOT NULL
REFERENCES watchkeeper.organization (id) ON DELETE RESTRICT;

CREATE INDEX manifest_organization_id_idx
ON watchkeeper.manifest (organization_id);

-- INSERT/UPDATE on manifest is granted here so future M3.5.a.3.2 handler
-- wire-up (and current test seeds running under wk_*_role) can write the
-- row under RLS. Migration 008 granted INSERT on manifest_version only;
-- the manifest table itself was previously written exclusively by the
-- owner role via direct seed scripts.
GRANT INSERT, UPDATE ON watchkeeper.manifest
TO wk_org_role, wk_user_role, wk_agent_role;

ALTER TABLE watchkeeper.manifest ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.manifest FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.manifest_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.manifest_version FORCE ROW LEVEL SECURITY;

-- `nullif(current_setting('watchkeeper.org', true), '')::uuid` returns SQL
-- NULL when the GUC is unset (current_setting with the missing-ok flag
-- returns ''). `organization_id = NULL` is never true, so unset GUC is
-- fail-closed — no row passes USING and no row passes WITH CHECK.
CREATE POLICY manifest_wk_org_role_policy ON watchkeeper.manifest
FOR ALL TO wk_org_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY manifest_wk_user_role_policy ON watchkeeper.manifest
FOR ALL TO wk_user_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY manifest_wk_agent_role_policy ON watchkeeper.manifest
FOR ALL TO wk_agent_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

-- manifest_version inherits tenancy through manifest_id; the policy
-- consults the parent row. The subquery is a SELECT against manifest,
-- which itself runs under the manifest_*_policy USING clause for the
-- caller's role, so the row is only visible if the parent manifest is
-- also visible to the caller — composition is deliberate.
CREATE POLICY manifest_version_wk_org_role_policy ON watchkeeper.manifest_version
FOR ALL TO wk_org_role
USING (
  manifest_id IN (
    SELECT id FROM watchkeeper.manifest
    WHERE organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid
  )
)
WITH CHECK (
  manifest_id IN (
    SELECT id FROM watchkeeper.manifest
    WHERE organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid
  )
);

CREATE POLICY manifest_version_wk_user_role_policy ON watchkeeper.manifest_version
FOR ALL TO wk_user_role
USING (
  manifest_id IN (
    SELECT id FROM watchkeeper.manifest
    WHERE organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid
  )
)
WITH CHECK (
  manifest_id IN (
    SELECT id FROM watchkeeper.manifest
    WHERE organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid
  )
);

CREATE POLICY manifest_version_wk_agent_role_policy ON watchkeeper.manifest_version
FOR ALL TO wk_agent_role
USING (
  manifest_id IN (
    SELECT id FROM watchkeeper.manifest
    WHERE organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid
  )
)
WITH CHECK (
  manifest_id IN (
    SELECT id FROM watchkeeper.manifest
    WHERE organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid
  )
);

-- +goose Down
DROP POLICY IF EXISTS manifest_version_wk_agent_role_policy ON watchkeeper.manifest_version;
DROP POLICY IF EXISTS manifest_version_wk_user_role_policy ON watchkeeper.manifest_version;
DROP POLICY IF EXISTS manifest_version_wk_org_role_policy ON watchkeeper.manifest_version;
DROP POLICY IF EXISTS manifest_wk_agent_role_policy ON watchkeeper.manifest;
DROP POLICY IF EXISTS manifest_wk_user_role_policy ON watchkeeper.manifest;
DROP POLICY IF EXISTS manifest_wk_org_role_policy ON watchkeeper.manifest;

ALTER TABLE watchkeeper.manifest_version NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.manifest_version DISABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.manifest NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.manifest DISABLE ROW LEVEL SECURITY;

REVOKE INSERT, UPDATE ON watchkeeper.manifest
FROM wk_org_role, wk_user_role, wk_agent_role;

DROP INDEX IF EXISTS watchkeeper.manifest_organization_id_idx;

ALTER TABLE watchkeeper.manifest
DROP COLUMN IF EXISTS organization_id;
