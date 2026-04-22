-- Watchkeeper Keep — row-level security + per-FK indexes (M2.1.d + M2.1.a
-- deferred-lesson closeout).
--
-- Creates three NOLOGIN session roles (`wk_org_role`, `wk_user_role`,
-- `wk_agent_role`) that the future Keep service (M2.7) will `SET ROLE` into
-- per request. Adds a `scope text NOT NULL DEFAULT 'org'` column on
-- `watch_order` (knowledge_chunk already has it from 004) with the same
-- prefix CHECK so every scoped row carries one of `'org'`, `'user:<uuid>'`,
-- or `'agent:<uuid>'`. ENABLE + FORCE ROW LEVEL SECURITY on both scoped
-- tables; per-role policies USING `(scope = 'org' OR scope =
-- current_setting('watchkeeper.scope', true))` and WITH CHECK `(scope =
-- current_setting('watchkeeper.scope', true))`. An unset session setting
-- evaluates to empty string, so requests without a configured scope see only
-- `scope = 'org'` rows and cannot INSERT at all — fail-closed by default.
--
-- Also lands the per-FK indexes deferred from M2.1.a. Postgres does not
-- auto-index FK columns; without these, planner cost on joins and ON
-- DELETE RESTRICT checks degrades at scale. `keepers_log.correlation_id`
-- already has a partial index from 003 and is not duplicated here.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wk_org_role') THEN
    CREATE ROLE wk_org_role NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wk_user_role') THEN
    CREATE ROLE wk_user_role NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wk_agent_role') THEN
    CREATE ROLE wk_agent_role NOLOGIN;
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE watchkeeper.watch_order
ADD COLUMN scope text NOT NULL DEFAULT 'org' CHECK (
  scope = 'org' OR scope LIKE 'user:%' OR scope LIKE 'agent:%'
);

-- Per-FK indexes on M2.1.a tables (deferred-lesson closeout). Named
-- `<table>_<column>_idx` to match the project convention.
CREATE INDEX human_organization_id_idx
ON watchkeeper.human (organization_id);

CREATE INDEX manifest_created_by_human_id_idx
ON watchkeeper.manifest (created_by_human_id);

CREATE INDEX manifest_version_manifest_id_idx
ON watchkeeper.manifest_version (manifest_id);

CREATE INDEX watchkeeper_manifest_id_idx
ON watchkeeper.watchkeeper (manifest_id);

CREATE INDEX watchkeeper_lead_human_id_idx
ON watchkeeper.watchkeeper (lead_human_id);

CREATE INDEX watchkeeper_active_manifest_version_id_idx
ON watchkeeper.watchkeeper (active_manifest_version_id);

CREATE INDEX watch_order_watchkeeper_id_idx
ON watchkeeper.watch_order (watchkeeper_id);

CREATE INDEX watch_order_lead_human_id_idx
ON watchkeeper.watch_order (lead_human_id);

CREATE INDEX keepers_log_actor_watchkeeper_id_idx
ON watchkeeper.keepers_log (actor_watchkeeper_id);

CREATE INDEX keepers_log_actor_human_id_idx
ON watchkeeper.keepers_log (actor_human_id);

ALTER TABLE watchkeeper.knowledge_chunk ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.knowledge_chunk FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.watch_order ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.watch_order FORCE ROW LEVEL SECURITY;

GRANT USAGE ON SCHEMA watchkeeper TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT, INSERT, UPDATE ON watchkeeper.knowledge_chunk
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT, INSERT, UPDATE ON watchkeeper.watch_order
TO wk_org_role, wk_user_role, wk_agent_role;

CREATE POLICY knowledge_chunk_wk_org_role_policy ON watchkeeper.knowledge_chunk
FOR ALL TO wk_org_role
USING (scope = 'org' OR scope = current_setting('watchkeeper.scope', true))
WITH CHECK (scope = current_setting('watchkeeper.scope', true));

CREATE POLICY knowledge_chunk_wk_user_role_policy ON watchkeeper.knowledge_chunk
FOR ALL TO wk_user_role
USING (scope = 'org' OR scope = current_setting('watchkeeper.scope', true))
WITH CHECK (scope = current_setting('watchkeeper.scope', true));

CREATE POLICY knowledge_chunk_wk_agent_role_policy ON watchkeeper.knowledge_chunk
FOR ALL TO wk_agent_role
USING (scope = 'org' OR scope = current_setting('watchkeeper.scope', true))
WITH CHECK (scope = current_setting('watchkeeper.scope', true));

CREATE POLICY watch_order_wk_org_role_policy ON watchkeeper.watch_order
FOR ALL TO wk_org_role
USING (scope = 'org' OR scope = current_setting('watchkeeper.scope', true))
WITH CHECK (scope = current_setting('watchkeeper.scope', true));

CREATE POLICY watch_order_wk_user_role_policy ON watchkeeper.watch_order
FOR ALL TO wk_user_role
USING (scope = 'org' OR scope = current_setting('watchkeeper.scope', true))
WITH CHECK (scope = current_setting('watchkeeper.scope', true));

CREATE POLICY watch_order_wk_agent_role_policy ON watchkeeper.watch_order
FOR ALL TO wk_agent_role
USING (scope = 'org' OR scope = current_setting('watchkeeper.scope', true))
WITH CHECK (scope = current_setting('watchkeeper.scope', true));

-- +goose Down
DROP POLICY IF EXISTS watch_order_wk_agent_role_policy ON watchkeeper.watch_order;
DROP POLICY IF EXISTS watch_order_wk_user_role_policy ON watchkeeper.watch_order;
DROP POLICY IF EXISTS watch_order_wk_org_role_policy ON watchkeeper.watch_order;
DROP POLICY IF EXISTS knowledge_chunk_wk_agent_role_policy ON watchkeeper.knowledge_chunk;
DROP POLICY IF EXISTS knowledge_chunk_wk_user_role_policy ON watchkeeper.knowledge_chunk;
DROP POLICY IF EXISTS knowledge_chunk_wk_org_role_policy ON watchkeeper.knowledge_chunk;

ALTER TABLE watchkeeper.watch_order NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.watch_order DISABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.knowledge_chunk NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.knowledge_chunk DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE ON watchkeeper.watch_order
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE SELECT, INSERT, UPDATE ON watchkeeper.knowledge_chunk
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE USAGE ON SCHEMA watchkeeper FROM wk_org_role, wk_user_role, wk_agent_role;

ALTER TABLE watchkeeper.watch_order DROP COLUMN IF EXISTS scope;

DROP INDEX IF EXISTS watchkeeper.keepers_log_actor_human_id_idx;
DROP INDEX IF EXISTS watchkeeper.keepers_log_actor_watchkeeper_id_idx;
DROP INDEX IF EXISTS watchkeeper.watch_order_lead_human_id_idx;
DROP INDEX IF EXISTS watchkeeper.watch_order_watchkeeper_id_idx;
DROP INDEX IF EXISTS watchkeeper.watchkeeper_active_manifest_version_id_idx;
DROP INDEX IF EXISTS watchkeeper.watchkeeper_lead_human_id_idx;
DROP INDEX IF EXISTS watchkeeper.watchkeeper_manifest_id_idx;
DROP INDEX IF EXISTS watchkeeper.manifest_version_manifest_id_idx;
DROP INDEX IF EXISTS watchkeeper.manifest_created_by_human_id_idx;
DROP INDEX IF EXISTS watchkeeper.human_organization_id_idx;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wk_agent_role') THEN
    DROP ROLE wk_agent_role;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wk_user_role') THEN
    DROP ROLE wk_user_role;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wk_org_role') THEN
    DROP ROLE wk_org_role;
  END IF;
END $$;
-- +goose StatementEnd
