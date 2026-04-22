-- Watchkeeper Keep — read grants for the three wk_*_role roles (M2.7.b+c).
--
-- The Keep read endpoints (search / get_manifest / log_tail) run under
-- `SET LOCAL ROLE wk_<kind>_role` inside a short transaction and rely on
-- row-level security to filter `knowledge_chunk` by scope (migration 005).
-- For the remaining read-side tables (manifest, manifest_version, audit log,
-- lookup tables) RLS is *not* enabled — they are org-wide by design — so
-- table-level `GRANT SELECT` is the entire access policy. Without these
-- grants, the Keep service cannot serve `get_manifest` or `log_tail`
-- because the session role has no SELECT privilege on the target table.
--
-- `organization`, `human`, `watchkeeper` are included because future
-- read endpoints join across them (e.g. manifest → created_by_human).
-- Keeping all three grants in one migration avoids a fragmented read
-- surface across milestones.
--
-- Write access on `manifest` / `manifest_version` / `keepers_log` lands in
-- M2.7.d (write endpoints) — this migration is intentionally SELECT-only.

-- +goose Up
GRANT SELECT ON watchkeeper.organization
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT ON watchkeeper.human
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT ON watchkeeper.watchkeeper
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT ON watchkeeper.manifest
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT ON watchkeeper.manifest_version
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT SELECT ON watchkeeper.keepers_log
TO wk_org_role, wk_user_role, wk_agent_role;

-- +goose Down
REVOKE SELECT ON watchkeeper.keepers_log
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE SELECT ON watchkeeper.manifest_version
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE SELECT ON watchkeeper.manifest
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE SELECT ON watchkeeper.watchkeeper
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE SELECT ON watchkeeper.human
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE SELECT ON watchkeeper.organization
FROM wk_org_role, wk_user_role, wk_agent_role;
